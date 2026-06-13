//go:build linux

package utility

import (
	"encoding/binary"
	"expvar"
	"fmt"
	"net"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FORWARD_TUN_VHOST: forward inner packets through a TUN fronted by vhost-net, so the
// in-kernel vhost worker batches them (TUN_MSG_PTR, up to 64/sendmsg) into
// tun_napi_receive -> napi_gro_receive == kernel-WireGuard's GRO path -> large
// super-skbs -> ip_forward. This is the only userspace route to WG-grade forward
// coalescing (proven from drivers/vhost/net.c + drivers/net/tun.c; kernel >= 5.18).
// Pair with FORWARD_UPLOAD_RESEQ (GRO wants ordered input). Single TX virtqueue keeps
// a flow in order. NOTE: current impl shares one writer (singleton) — fine for the
// single-flow target; multi-conn would need per-conn vrings or a producer lock.
var vhostDbgOnce sync.Once

// forwardViaVhost is config-driven (FORWARD_TUN_VHOST), set via SetForwardMode; default off.
var forwardViaVhost bool

const (
	vhostTunName    = "tmvhost0" // "tm" prefix so buildLocalLANs + the tm+ iptables cover it
	tunSetVnetHdrSz = 0x400454d8 // TUNSETVNETHDRSZ = _IOW('T', 216, int)
)

var (
	vhostTunFd  = -1
	vhostTunMAC [6]byte
	vhostWriter *vhostNetWriter
	vhostOnce   sync.Once
	vhostErr    error

	tunVhostWrites = expvar.NewInt("tun_vhost_writes")
	tunVhostDrops  = expvar.NewInt("tun_vhost_drops")
	tunVhostFull   = expvar.NewInt("tun_vhost_full")
)

func createVhostTun() error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte
	copy(ifr[:], vhostTunName)
	// IFF_TAP (L2): the vhost xdp path runs eth_type_trans, which needs an Ethernet
	// header — IFF_TUN (L3) would have it strip 14 bytes of the IP header.
	binary.LittleEndian.PutUint16(ifr[16:], unix.IFF_TAP|unix.IFF_NO_PI|iffVnetHdr|iffNapi)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		unix.Close(fd)
		return fmt.Errorf("TUNSETIFF %s (IFF_TAP|IFF_NAPI|IFF_VNET_HDR): %v", vhostTunName, e)
	}
	sz := int32(vnetHdrV1Len)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetVnetHdrSz, uintptr(unsafe.Pointer(&sz))); e != 0 {
		unix.Close(fd)
		return fmt.Errorf("TUNSETVNETHDRSZ=12: %v", e)
	}
	// Pin a fixed MAC (while the device is still down) and use it as the Ethernet dst,
	// so the frame's dst always == the device MAC -> eth_type_trans yields PACKET_HOST
	// (not OTHERHOST -> dropped before ip_rcv). Querying the MAC raced/returned stale bytes.
	vhostTunMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0xab, 0xcd}
	if err := setIfaceMAC(vhostTunName, vhostTunMAC); err != nil {
		unix.Close(fd)
		return fmt.Errorf("set %s MAC: %w", vhostTunName, err)
	}
	if err := setIfaceUp(vhostTunName); err != nil {
		unix.Close(fd)
		return err
	}
	writeProc("/proc/sys/net/ipv4/ip_forward", "1")
	writeProc("/proc/sys/net/ipv4/conf/"+vhostTunName+"/accept_local", "1")
	writeProc("/proc/sys/net/ipv4/conf/"+vhostTunName+"/rp_filter", "0")
	writeProc("/proc/sys/net/ipv4/conf/all/accept_local", "1")
	fmt.Printf("[vhost] %s up IFF_TAP mac=% x\n", vhostTunName, vhostTunMAC)
	vhostTunFd = fd
	return nil
}

// setIfaceMAC sets an interface's hardware address (SIOCSIFHWADDR). Device must be down.
func setIfaceMAC(name string, mac [6]byte) error {
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(s)
	var ifr [40]byte
	copy(ifr[:], name)
	binary.LittleEndian.PutUint16(ifr[16:], unix.ARPHRD_ETHER) // sa_family
	copy(ifr[18:24], mac[:])                                   // sa_data[0:6]
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(s), unix.SIOCSIFHWADDR, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return e
	}
	return nil
}

func initVhost() {
	if vhostErr = createVhostTun(); vhostErr != nil {
		return
	}
	vhostWriter, vhostErr = newVhostNetWriter(vhostTunFd, 256, 4096)
}

// TunVhostBatch implements fwdEgress over the vhost-net writer. Each L3 packet is
// wrapped in a 14-byte Ethernet header (dst = the tap's MAC so the kernel accepts it
// for L3/forward; the vhost xdp path's eth_type_trans then strips it cleanly).
type TunVhostBatch struct {
	w       *vhostNetWriter
	queued  int
	dstMAC  [6]byte
	scratch []byte
}

func NewTunVhostBatch() (*TunVhostBatch, error) {
	vhostOnce.Do(initVhost)
	if vhostErr != nil {
		return nil, vhostErr
	}
	return &TunVhostBatch{w: vhostWriter, dstMAC: vhostTunMAC, scratch: make([]byte, 14+4096)}, nil
}

func (b *TunVhostBatch) Add(ip []byte, _ net.HardwareAddr) error {
	if 14+len(ip) > len(b.scratch) {
		tunVhostDrops.Add(1)
		return nil
	}
	f := b.scratch[:14+len(ip)]
	copy(f[0:6], b.dstMAC[:])                                  // dst = tap MAC
	f[6], f[7], f[8], f[9], f[10], f[11] = 0x02, 0, 0, 0, 0, 1 // src = locally-administered
	if ip[0]>>4 == 6 {
		f[12], f[13] = 0x86, 0xDD // IPv6
	} else {
		f[12], f[13] = 0x08, 0x00 // IPv4
	}
	copy(f[14:], ip)
	vhostDbgOnce.Do(func() {
		n := len(f)
		if n > 22 {
			n = 22
		}
		fmt.Printf("[vhost] first fwd frame len=%d head=% x\n", len(f), f[:n])
	})
	if !b.w.Submit(f) {
		// ring full: kick the worker to drain + advance used.idx, then retry once.
		b.w.Flush()
		tunVhostFull.Add(1)
		if !b.w.Submit(f) {
			tunVhostDrops.Add(1)
			return nil
		}
	}
	b.queued++
	return nil
}

func (b *TunVhostBatch) Flush(_ bool) error {
	if b.queued == 0 {
		return nil
	}
	b.w.Flush()
	tunVhostWrites.Add(int64(b.queued))
	b.queued = 0
	return nil
}

func (b *TunVhostBatch) Full() bool  { return false }
func (b *TunVhostBatch) Empty() bool { return b.queued == 0 }
func (b *TunVhostBatch) Close()      {} // singleton writer; leave open

//go:build linux

package utility

import (
	"encoding/binary"
	"expvar"
	"fmt"
	"net"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const tunSetIff = 0x400454ca // TUNSETIFF

var (
	tunNapiWrites = expvar.NewInt("tun_napi_writes")
	tunNapiDrops  = expvar.NewInt("tun_napi_drops")
)

// setIfaceUp brings a TUN/TAP interface up (IFF_UP|IFF_RUNNING via SIOCSIFFLAGS).
func setIfaceUp(name string) error {
	s, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(s)
	var ifr [40]byte
	copy(ifr[:], name)
	binary.LittleEndian.PutUint16(ifr[16:], unix.IFF_UP|unix.IFF_RUNNING)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(s), unix.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		return fmt.Errorf("SIOCSIFFLAGS up %s: %v", name, e)
	}
	return nil
}

func writeProc(path, val string) { _ = os.WriteFile(path, []byte(val), 0o644) }

// FORWARD_TUN_NAPI: forward XDP-eligible packets by writing INDIVIDUAL (non-GSO)
// MTU-sized packets to a TUN opened IFF_TUN|IFF_NAPI|IFF_NO_PI. With IFF_NAPI the
// TUN driver delivers writes via napi_gro_receive() (tun_get_user -> napi_schedule
// -> tun_napi_poll -> napi_gro_receive), so the KERNEL'S GRO engine coalesces
// same-flow in-order TCP into a NON-DODGY GSO skb (tcp4_gro_complete sets gso_type
// WITHOUT SKB_GSO_DODGY). When ip_forward sends that skb out the virtio NIC it goes
// to host-TSO IN ORDER — exactly the path kernel WireGuard uses
// (drivers/net/wireguard/receive.c:411 napi_gro_receive). This is the OPPOSITE of
// the (deleted) TunGSOBatch, which wrote a pre-built virtio_net_hdr GSO super-frame
// (IFF_VNET_HDR) -> SKB_GSO_DODGY -> software-segmented -> reordered -> collapsed.
//
// GRO only coalesces segments landing in the same NAPI poll, so the forward consumer
// must write bursts back-to-back (the pktChan drain loop does). Input must be in order
// (GRO flushes on OOO) — pair with FORWARD_UPLOAD_RESEQ. Keep ethtool -K eth0 gso/tso on.
// Off by default; set FORWARD_TUN_NAPI=true in tmasqued.conf (applied by LoadConfig).
var forwardViaNapiTun = false

const (
	napiTunName = "tmnapi0"
	iffNapi     = 0x0010
)

var (
	napiTunFd   int = -1
	napiTunOnce sync.Once
	napiTunErr  error
)

// TunNapiBatch writes individual packets to the IFF_NAPI TUN. Add writes immediately
// (back-to-back across a drain loop = one NAPI poll = GRO coalesce); Flush is a no-op.
type TunNapiBatch struct {
	fd    int
	wrote int // diag: packets written since last reset
}

func createNapiTun() error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	var ifr [40]byte
	copy(ifr[:], napiTunName)
	binary.LittleEndian.PutUint16(ifr[16:], unix.IFF_TUN|unix.IFF_NO_PI|iffNapi)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), tunSetIff, uintptr(unsafe.Pointer(&ifr))); e != 0 {
		unix.Close(fd)
		return fmt.Errorf("TUNSETIFF %s (IFF_NAPI — needs kernel tun NAPI support): %v", napiTunName, e)
	}
	if err := setIfaceUp(napiTunName); err != nil {
		unix.Close(fd)
		return err
	}
	// kernel-forward plumbing: enable forwarding; accept the pre-SNAT'd local-source
	// packet on TUN ingress (else martian); disable rp_filter on the TUN.
	writeProc("/proc/sys/net/ipv4/ip_forward", "1")
	writeProc("/proc/sys/net/ipv4/conf/"+napiTunName+"/accept_local", "1")
	writeProc("/proc/sys/net/ipv4/conf/"+napiTunName+"/rp_filter", "0")
	writeProc("/proc/sys/net/ipv4/conf/all/accept_local", "1")
	napiTunFd = fd
	return nil
}

func NewTunNapiBatch() (*TunNapiBatch, error) {
	napiTunOnce.Do(func() { napiTunErr = createNapiTun() })
	if napiTunErr != nil {
		return nil, napiTunErr
	}
	return &TunNapiBatch{fd: napiTunFd}, nil
}

// Add writes one already-SNAT'd L3 packet to the NAPI TUN. dstMAC is ignored (the
// kernel resolves the next hop while forwarding).
func (b *TunNapiBatch) Add(pkt []byte, _ net.HardwareAddr) error {
	if _, err := unix.Write(b.fd, pkt); err != nil {
		tunNapiDrops.Add(1)
		return err
	}
	b.wrote++
	tunNapiWrites.Add(1)
	return nil
}

func (b *TunNapiBatch) Flush(_ bool) error { b.wrote = 0; return nil } // writes are immediate
func (b *TunNapiBatch) Full() bool         { return false }
func (b *TunNapiBatch) Empty() bool        { return true }
func (b *TunNapiBatch) Close()             {} // singleton fd; leave open

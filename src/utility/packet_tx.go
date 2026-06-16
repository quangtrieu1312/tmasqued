//go:build linux

package utility

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

var totalPacketFlushes atomic.Int64
var totalPacketPackets atomic.Int64

// htonsEth byte-swaps a uint16 to network order for sll_protocol / socket(2)'s
// protocol arg. amd64 (the testbed) is little-endian; this is a no-op the kernel
// would otherwise reject as the wrong EtherType.
func htonsEth(p uint16) uint16 { return p<<8 | p>>8 }

// PacketBatch transmits complete L2 frames out a bound interface via an AF_PACKET
// raw socket (sendmmsg). Unlike AF_XDP TX, the frames traverse the kernel qdisc
// and driver TX path — the same in-order egress WireGuard relies on.
//
// Motivation: on the reprovisioned virtio testbed, AF_XDP TX reorders bursts of a
// single flow (UMEM frame reuse races the host's deferred TX completion), inflating
// the target's OFO queue to ~54% and collapsing inner-TCP to ~1.1 G; the kernel TX
// path stays ordered there (GW→target kernel iperf3 = 1% OFO @9.2 G). This path is
// the WireGuard-equivalent forward egress.
//
// Frames carry their own valid L3/L4 checksums (SNAT already recomputed them), so
// no checksum offload is needed for the plain (non-GSO) path.
//
// Gated by FORWARD_KERNEL_TX (see ForwardBatch). Reuses socket.go's mmsghdr /
// sendmmsg machinery; the AF_XDP forward path is left untouched.
type PacketBatch struct {
	fd     int
	srcMAC net.HardwareAddr
	msgs   []mmsghdr
	iovs   []unix.Iovec
	bufs   [MaxSocketBatchSize][maxFrameSize]byte
	count  int
}

// NewPacketBatch opens an AF_PACKET raw socket bound to ifindex. srcMAC is stamped
// as the Ethernet source on every frame (the WAN NIC's own MAC).
func NewPacketBatch(ifindex int, srcMAC net.HardwareAddr) (*PacketBatch, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htonsEth(unix.ETH_P_ALL)))
	if err != nil {
		return nil, fmt.Errorf("AF_PACKET socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htonsEth(unix.ETH_P_ALL),
		Ifindex:  ifindex,
	}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("AF_PACKET bind ifindex %d: %w", ifindex, err)
	}
	// Large send buffer so high-rate qdisc bursts don't ENOBUFS-drop (the
	// IPPROTO_RAW FORWARD_ALL_VIA_SOCKET attempt overflowed here → ~27% loss).
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 16<<20)
	return &PacketBatch{
		fd:     fd,
		srcMAC: srcMAC,
		msgs:   make([]mmsghdr, MaxSocketBatchSize),
		iovs:   make([]unix.Iovec, MaxSocketBatchSize),
	}, nil
}

// Add wraps a raw L3 IP packet in a 14-byte Ethernet header (addressed to dstMAC)
// and queues the frame. The caller's slice is copied immediately.
func (b *PacketBatch) Add(pkt []byte, dstMAC net.HardwareAddr) error {
	if b.count >= MaxSocketBatchSize {
		return errors.New("packet batch full")
	}
	var ethertype [2]byte
	switch v := IPVersion(pkt); v {
	case 4:
		if len(pkt) < ipv4.HeaderLen {
			return errors.New("IPv4 packet too short")
		}
		ethertype = [2]byte{0x08, 0x00}
	case 6:
		if len(pkt) < ipv6.HeaderLen {
			return errors.New("IPv6 packet too short")
		}
		ethertype = [2]byte{0x86, 0xDD}
	default:
		return fmt.Errorf("unknown IP version: %d", v)
	}
	total := ethHdrSize + len(pkt)
	if total > maxFrameSize {
		return fmt.Errorf("packet too large (%d bytes, max payload %d)", len(pkt), maxFrameSize-ethHdrSize)
	}

	i := b.count
	frame := b.bufs[i][:]
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], b.srcMAC)
	frame[12] = ethertype[0]
	frame[13] = ethertype[1]
	copy(frame[14:], pkt)

	b.iovs[i] = unix.Iovec{Base: &b.bufs[i][0]}
	b.iovs[i].SetLen(total)
	// Bound socket → no per-message address; kernel transmits on the bound iface.
	b.msgs[i].Hdr.Iov = &b.iovs[i]
	b.msgs[i].Hdr.SetIovlen(1)
	b.count++
	return nil
}

func (b *PacketBatch) Flush(enableStats bool) error {
	if b.count == 0 {
		return nil
	}
	var p runtime.Pinner
	for i := 0; i < b.count; i++ {
		p.Pin(&b.iovs[i])
		p.Pin(&b.bufs[i][0])
	}
	defer p.Unpin()
	_, _, errno := syscall.Syscall6(
		unix.SYS_SENDMMSG,
		uintptr(b.fd),
		uintptr(unsafe.Pointer(&b.msgs[0])),
		uintptr(b.count),
		uintptr(unix.MSG_DONTWAIT),
		0, 0,
	)
	if enableStats {
		totalPacketFlushes.Add(1)
		totalPacketPackets.Add(int64(b.count))
	}
	b.count = 0
	if errno != 0 {
		return errno
	}
	return nil
}

func (b *PacketBatch) Full() bool  { return b.count >= MaxSocketBatchSize }
func (b *PacketBatch) Empty() bool { return b.count == 0 }
func (b *PacketBatch) Close()      { unix.Close(b.fd) }

// PacketBatchStats returns cumulative counters and the mean batch size.
func PacketBatchStats() (flushes, packets int64, avg float64) {
	f := totalPacketFlushes.Load()
	p := totalPacketPackets.Load()
	if f == 0 {
		return f, p, 0
	}
	return f, p, float64(p) / float64(f)
}

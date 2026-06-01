//go:build linux

package utility

import (
	"errors"
	"expvar"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/slavc/xdp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const MaxXDPBatchSize = 1024

// ethHdrSize is the fixed 14-byte Ethernet II header (dst+src MAC + EtherType).
const ethHdrSize = 14

// maxFrameSize is the largest L2 frame we'll ever build: Ethernet + 1500-byte MTU.
const maxFrameSize = ethHdrSize + 1500

var totalXDPFlushes atomic.Int64
var totalXDPPackets atomic.Int64

// xdpFwdTxDrops counts inner packets dropped in the forward TX path because the
// TX ring/UMEM couldn't accept the whole batch (GetDescs returned fewer descs
// than queued). Previously these were only logged by the caller — now queryable
// at /debug/vars to localize single-stream forward loss.
var xdpFwdTxDrops = expvar.NewInt("xdp_fwd_tx_drops")

// XDPBatch accumulates raw L3 IP packets and flushes them in one XDP batch
// transmit, replacing the sendmmsg-based SocketBatch.
type XDPBatch struct {
	sock        *xdp.Socket
	srcMAC      net.HardwareAddr
	dstMAC      net.HardwareAddr
	genericMode bool // true → XDP_COPY/generic path; kick via unix.Send
	mu          *sync.Mutex

	// Pre-allocated frame buffers.  Each slot holds an Ethernet-wrapped
	// copy of the original IP packet, ready to copy into UMEM.
	bufs  [MaxXDPBatchSize][maxFrameSize]byte
	lens  [MaxXDPBatchSize]int // actual frame length for bufs[i]
	count int
}

// NewXDPBatch creates an XDPBatch for sock.
//
//   - srcMAC / dstMAC are the Ethernet addresses stamped on every frame.
//   - genericMode selects the TX kick strategy: true → XDP_COPY (unix.Send),
//     false → native driver (sock.Poll).
//   - mu is the caller's TX mutex that guards sock; it is held for the
//     entire Flush operation, matching the pattern in Conn.WriteTo.
func NewXDPBatch(sock *xdp.Socket, srcMAC, dstMAC net.HardwareAddr, genericMode bool, mu *sync.Mutex) *XDPBatch {
	return &XDPBatch{
		sock:        sock,
		srcMAC:      srcMAC,
		dstMAC:      dstMAC,
		genericMode: genericMode,
		mu:          mu,
	}
}

// IPVersion returns the IP version byte (4 or 6) of a raw IP packet.

// Add validates pkt (a raw L3 IP packet), prepends a 14-byte Ethernet header,
// and copies the result into the next available batch slot.
//
// dstMAC is the L2 next-hop the frame is addressed to. Pass nil to use the
// batch's default dstMAC (set at construction) — used by single-next-hop
// callers like SendOne. The forward path passes a per-destination MAC so
// on-link targets are reached directly instead of hairpinning via the gateway.
//
// The caller's original slice is not retained; the data is copied immediately.
func (b *XDPBatch) Add(pkt []byte, dstMAC net.HardwareAddr) error {
	if b.count >= MaxXDPBatchSize {
		return errors.New("XDP batch full")
	}
	if dstMAC == nil {
		dstMAC = b.dstMAC
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

	// Ethernet II header
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], b.srcMAC)
	frame[12] = ethertype[0]
	frame[13] = ethertype[1]

	// L3 payload (IP packet verbatim — IP header is already present)
	copy(frame[14:], pkt)
	b.lens[i] = total
	b.count++
	return nil
}

func (b *XDPBatch) Flush(enableStats bool) error {
    if b.count == 0 {
        return nil
    }
    n := b.count
    b.count = 0

    // Lock 1: reap + alloc only
    b.mu.Lock()
    if nc := b.sock.NumCompleted(); nc > 0 {
        b.sock.Complete(nc)
    }
    descs := b.sock.GetDescs(n, false)
    b.mu.Unlock()
	if len(descs) < n {
    	fmt.Printf("XDPBatch.Flush: wanted %d descs, got %d\n", n, len(descs))
	}
    if len(descs) == 0 {
        xdpFwdTxDrops.Add(int64(n))
        return fmt.Errorf("TX UMEM exhausted, dropped %d packets", n)
    }
    send := len(descs)

    // Frame filling outside the lock — each desc is exclusively ours
    for i := 0; i < send; i++ {
        umemFrame := b.sock.GetFrame(descs[i])
        copied := copy(umemFrame, b.bufs[i][:b.lens[i]])
        descs[i].Len = uint32(copied)
    }

    // Lock 2: submit + kick only
    b.mu.Lock()
    b.sock.Transmit(descs)
    b.mu.Unlock()
	if (enableStats) {
    	totalXDPFlushes.Add(1)
    	totalXDPPackets.Add(int64(send))
	}

    if dropped := n - send; dropped > 0 {
        xdpFwdTxDrops.Add(int64(dropped))
        return fmt.Errorf("XDP UMEM partial: sent %d, dropped %d packets", send, dropped)
    }
    return nil
}

// Full reports whether the batch has reached MaxBatchSize and must be flushed
// before more packets can be added.
func (b *XDPBatch) Full() bool {
	return b.count >= MaxXDPBatchSize
}

// Empty reports whether the batch holds no packets.
func (b *XDPBatch) Empty() bool {
	return b.count == 0
}

// SendOne is a convenience wrapper for single-packet sends (backward-compat
// replacement for the old SendOnSocket).
func SendOne(sock *xdp.Socket, srcMAC, dstMAC net.HardwareAddr, genericMode bool, mu *sync.Mutex, pkt []byte, enableStats bool) error {
	b := NewXDPBatch(sock, srcMAC, dstMAC, genericMode, mu)
	if err := b.Add(pkt, nil); err != nil {
		return err
	}
	return b.Flush(enableStats)
}

// BatchStats returns cumulative counters and the mean batch size across all
// Flush calls since process start.
func XDPBatchStats() (flushes, packets int64, avg float64) {
	f := totalXDPFlushes.Load()
	p := totalXDPPackets.Load()
	if f == 0 {
		return f, p, 0
	}
	return f, p, float64(p) / float64(f)
}

//go:build linux

package utility

import (
	"errors"
	"expvar"
	"fmt"
	"net"
	"sync/atomic"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// FrameSubmitter accepts a complete L2 frame for transmission. It is satisfied
// by *xdp.txPump (the single-owner TX ring pump); declaring it here lets the
// utility package enqueue frames without importing the xdp package (avoiding a
// cycle, since xdp depends on utility).
type FrameSubmitter interface {
	Submit(frame []byte)
}

const MaxXDPBatchSize = 1024

// ethHdrSize is the fixed 14-byte Ethernet II header (dst+src MAC + EtherType).
const ethHdrSize = 14

// maxFrameSize is the largest L2 frame we'll ever build: Ethernet + 1500-byte MTU.
const maxFrameSize = ethHdrSize + 4082

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
	pump   FrameSubmitter
	srcMAC net.HardwareAddr
	dstMAC net.HardwareAddr

	// Pre-allocated frame buffers.  Each slot holds an Ethernet-wrapped
	// copy of the original IP packet, ready to hand to the TX pump.
	bufs  [MaxXDPBatchSize][maxFrameSize]byte
	lens  [MaxXDPBatchSize]int // actual frame length for bufs[i]
	count int
}

// NewXDPBatch creates an XDPBatch that enqueues frames to pump.
//
//   - srcMAC / dstMAC are the Ethernet addresses stamped on every frame.
//   - pump is the single-owner TX ring pump; Flush hands it each built frame,
//     so forward consumers never touch the ring directly or take a TX lock.
func NewXDPBatch(pump FrameSubmitter, srcMAC, dstMAC net.HardwareAddr) *XDPBatch {
	return &XDPBatch{
		pump:   pump,
		srcMAC: srcMAC,
		dstMAC: dstMAC,
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

    // Hand every built frame to the TX pump. The pump (single owner of the TX
    // ring) does the reap/alloc/transmit; Submit is lock-free and never blocks,
    // so concurrent forward consumers no longer serialize on a shared TX mutex.
    for i := 0; i < n; i++ {
        b.pump.Submit(b.bufs[i][:b.lens[i]])
    }
	if (enableStats) {
    	totalXDPFlushes.Add(1)
    	totalXDPPackets.Add(int64(n))
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
func SendOne(pump FrameSubmitter, srcMAC, dstMAC net.HardwareAddr, pkt []byte, enableStats bool) error {
	b := NewXDPBatch(pump, srcMAC, dstMAC)
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

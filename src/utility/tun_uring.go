//go:build linux

package utility

import (
	"expvar"
	"net"
	"os"
)

// FORWARD_TUN_URING: forward via the SAME IFF_NAPI TUN as FORWARD_TUN_NAPI (so the
// kernel GRO/host-TSO ordering path is kept), but batch the per-packet writes through
// io_uring (one io_uring_enter per drain batch instead of one write() per packet).
// Rationale (measured 2026-06-06, 8-core→5.80): FORWARD_TUN_NAPI gives WG-like ORDER
// (recv-OFO 22%→10% with reseq tuning, vs AF_PACKET 42%) but its single-flow THROUGHPUT
// (590M) trails AF_PACKET (912M) on the per-packet write() — not CPU-bound (no core
// pegged), so it's write syscall overhead/latency. io_uring batches the submission to
// recover the throughput while keeping the ordering. Pair with FORWARD_UPLOAD_RESEQ.
var forwardViaUring = os.Getenv("FORWARD_TUN_URING") == "1"

const uringDepth = 512

var (
	tunUringWrites = expvar.NewInt("tun_uring_writes")
	tunUringFails  = expvar.NewInt("tun_uring_fails")
	tunUringDrops  = expvar.NewInt("tun_uring_drops")
)

// TunUringBatch drives the IFF_NAPI forward TUN with an io_uring batched writer.
type TunUringBatch struct {
	fd       int
	uw       *uringWriter
	maxFrame int
	queued   int
}

func NewTunUringBatch() (*TunUringBatch, error) {
	napiTunOnce.Do(func() { napiTunErr = createNapiTun() })
	if napiTunErr != nil {
		return nil, napiTunErr
	}
	const maxFrame = 4096 // jumbo inner MTU (3422) + headroom
	uw, err := newUringWriter(napiTunFd, uringDepth, maxFrame)
	if err != nil {
		return nil, err
	}
	return &TunUringBatch{fd: napiTunFd, uw: uw, maxFrame: maxFrame}, nil
}

// Add queues one already-SNAT'd L3 packet for batched submission. dstMAC is ignored
// (the kernel resolves the next hop while forwarding the IFF_NAPI TUN ingress).
func (b *TunUringBatch) Add(pkt []byte, _ net.HardwareAddr) error {
	if len(pkt) > b.maxFrame {
		tunUringDrops.Add(1)
		return nil
	}
	if b.uw.Full() {
		b.Flush(false)
	}
	b.uw.Submit(b.fd, pkt)
	b.queued++
	return nil
}

func (b *TunUringBatch) Flush(_ bool) error {
	if b.queued == 0 {
		return nil
	}
	n := b.queued
	fails := b.uw.Flush()
	b.queued = 0
	tunUringWrites.Add(int64(n - fails))
	if fails > 0 {
		tunUringFails.Add(int64(fails))
	}
	return nil
}

func (b *TunUringBatch) Full() bool  { return b.uw.Full() }
func (b *TunUringBatch) Empty() bool { return b.queued == 0 }
func (b *TunUringBatch) Close()      { b.uw.Close() }

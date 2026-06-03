//go:build linux

package xdp

import (
	"sync"
	"sync/atomic"

	"github.com/slavc/xdp"
)

// txPump is the single owner of one AF_XDP socket's TX + completion rings.
//
// Why: on a single-queue NIC there is exactly one TX ring per socket, and every
// TX producer (forward data path + QUIC WriteBatch/WriteTo) previously serialized
// on a shared per-socket mutex (txMu) around Complete/GetDescs/Transmit. Under
// concurrent load that mutex — not CPU — was the aggregate ceiling (a live mutex
// profile showed ~90% of contention there, server only ~48% busy). This mirrors
// what the kernel's *lockless* qdisc does for WireGuard: many producers enqueue
// cheaply, one owner drains the ring.
//
// The pump owns TX + Completion exclusively, so no lock is needed for the ring
// ops. The RX + Fill rings remain owned by dispatchSocket; those are separate
// rings and separate descriptor arrays in the lib, so RX and TX never conflict
// on the same socket.
//
// Ordering: each inner flow is produced by a single goroutine (one forward
// consumer per tunnel; one quic-go send loop per connection), the channel is
// FIFO, and the pump transmits in dequeue order — so per-flow order is preserved
// (no reorder, which inner TCP is very sensitive to). Flows from different
// producers interleave, which is fine.
const (
	txPumpQueueLen = 16384 // buffered frames in flight to the pump
	txPumpBatch    = 1024  // max frames transmitted per ring submission
	txPumpFrameCap = 4096  // matches the AF_XDP FrameSize
)

type txMsg struct {
	bp *[]byte // pooled backing buffer
	n  int     // frame length in *bp
}

type txPump struct {
	sock  *xdp.Socket
	ch    chan txMsg
	pool  sync.Pool
	drops atomic.Uint64 // frames dropped: queue full or TX UMEM exhausted
	done  chan struct{}
}

func newTxPump(sock *xdp.Socket, done chan struct{}) *txPump {
	p := &txPump{
		sock: sock,
		ch:   make(chan txMsg, txPumpQueueLen),
		done: done,
	}
	p.pool.New = func() any { b := make([]byte, txPumpFrameCap); return &b }
	go p.run()
	return p
}

// Submit hands a complete L2 frame to the pump. It copies the frame into a
// pooled buffer and enqueues it; it never blocks (drops + counts if the queue
// is full, like UDP ENOBUFS) so producers are never stalled by the TX ring.
func (p *txPump) Submit(frame []byte) {
	bp := p.pool.Get().(*[]byte)
	if cap(*bp) < len(frame) {
		*bp = make([]byte, len(frame))
	}
	n := copy((*bp)[:cap(*bp)], frame)
	select {
	case p.ch <- txMsg{bp: bp, n: n}:
	case <-p.done:
		p.pool.Put(bp)
	default:
		p.pool.Put(bp)
		p.drops.Add(1)
	}
}

// Drops returns the number of frames dropped by the pump.
func (p *txPump) Drops() uint64 { return p.drops.Load() }

func (p *txPump) run() {
	batch := make([]txMsg, 0, txPumpBatch)
	for {
		// Block for the first frame (or shutdown).
		select {
		case <-p.done:
			return
		case m := <-p.ch:
			batch = append(batch, m)
		}
		// Opportunistically drain more, up to a batch.
	drain:
		for len(batch) < txPumpBatch {
			select {
			case m := <-p.ch:
				batch = append(batch, m)
			default:
				break drain
			}
		}

		// Single owner of TX + completion rings: no lock required.
		if nc := p.sock.NumCompleted(); nc > 0 {
			p.sock.Complete(nc)
		}
		descs := p.sock.GetDescs(len(batch), false)
		got := len(descs)
		for i := 0; i < got; i++ {
			f := p.sock.GetFrame(descs[i])
			ln := copy(f, (*batch[i].bp)[:batch[i].n])
			descs[i].Len = uint32(ln)
		}
		if got > 0 {
			p.sock.Transmit(descs)
		}
		if got < len(batch) {
			p.drops.Add(uint64(len(batch) - got))
		}
		for i := range batch {
			p.pool.Put(batch[i].bp)
			batch[i] = txMsg{}
		}
		batch = batch[:0]
	}
}

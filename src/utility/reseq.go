//go:build linux

package utility

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

// PreReseqTotal / PreReseqOOO measure download-path inner-TCP-seq order AS THE
// SERVER FRAMES IT, at the tunChan consumer just before SendDatagram. The server
// XDP ingress is known in-order (xdpdump genuine reorder = 0%), so if these show
// the consumer ALREADY out of order, the reorder is introduced between XDP RX and
// the consumer (dispatch/Deliver/tunChan); if ~0, it is introduced after the
// consumer (quic datagram queue / packer / TX). Pure measurement; never reorders.
//
// NOTE: PreReseqOOO is a HIGHWATER-only metric (flags any seq<max), so it conflates
// retransmits with genuine reorder. Use the seen-set counters below for a clean
// genuine-reorder signal directly comparable to the tcpdump analysis at client tun0.
var (
	PreReseqTotal atomic.Uint64
	PreReseqOOO   atomic.Uint64
)

// PreSend{Total,Genuine,Retr} measure the same point as PreReseq{Total,OOO} but with
// a per-flow seen-set, so a seq below the running max is classified as RETRANSMIT
// (already seen) or GENUINE_REORDER (new seq below max — the producer reordered it).
// Compared directly against client-side tun0 tcpdump genuine-reorder to localize
// where the residual download reorder enters: if PreSendGenuine ≈ 0% but tun0 shows
// ~1.5%, the reorder enters AFTER WritePacket (QUIC packer / AF_XDP TX / wire / client).
var (
	PreSendTotal   atomic.Uint64
	PreSendGenuine atomic.Uint64
	PreSendRetr    atomic.Uint64
)

// PreReseqObserver tracks, per flow, the highest TCP seq seen so far and counts a
// segment as out-of-order on ARRIVAL when its seq is below that high-water mark.
// One observer per consumer goroutine (single tunChan consumer per conn), so it
// needs no lock. A genuine retransmit also registers; acceptable since the path is
// known drop-free so OOO ~= reorder.
type PreReseqObserver struct {
	maxSeq map[flowKey]uint32
}

func NewPreReseqObserver() *PreReseqObserver {
	return &PreReseqObserver{maxSeq: make(map[flowKey]uint32)}
}

func (o *PreReseqObserver) Observe(ip []byte) {
	seq, _, key, ok := parseTCP(ip)
	if !ok {
		return
	}
	PreReseqTotal.Add(1)
	mx, seen := o.maxSeq[key]
	switch {
	case !seen:
		if len(o.maxSeq) >= 1<<16 {
			o.maxSeq = make(map[flowKey]uint32)
		}
		o.maxSeq[key] = seq
	case int32(seq-mx) < 0:
		PreReseqOOO.Add(1)
	default:
		o.maxSeq[key] = seq
	}
}

// preSendFlow is the per-flow state for the seen-set observer: the running max seq
// + a bounded ring of recent seqs so a sub-max seq can be classified as retransmit
// (was in the ring) vs genuine reorder (was not).
const preSendRingSize = 1024 // covers ~1.2s of in-flight at 25k pkts/30s

type preSendFlow struct {
	maxSeq uint32
	ring   [preSendRingSize]uint32
	in     map[uint32]struct{}
	head   int // next write index (mod preSendRingSize)
	count  int // entries populated (≤ preSendRingSize)
}

// PreSendGenuineObserver counts inner-TCP-seq inversions as the server frames them,
// distinguishing retransmits from genuine reorder via a per-flow seen-set. Called from
// the single tunChan-consumer goroutine, so it needs no lock. Bounded memory: ≤256
// flows × (1024 uint32 + ~1024 map entries) ≈ 9MB worst case; map is cleared on cap.
type PreSendGenuineObserver struct {
	flows map[flowKey]*preSendFlow
}

func NewPreSendGenuineObserver() *PreSendGenuineObserver {
	return &PreSendGenuineObserver{flows: make(map[flowKey]*preSendFlow)}
}

func (o *PreSendGenuineObserver) Observe(ip []byte) {
	seq, _, key, ok := parseTCP(ip)
	if !ok {
		return
	}
	PreSendTotal.Add(1)
	f, ok := o.flows[key]
	if !ok {
		if len(o.flows) >= 256 {
			o.flows = make(map[flowKey]*preSendFlow)
		}
		f = &preSendFlow{in: make(map[uint32]struct{}, preSendRingSize)}
		o.flows[key] = f
	}
	if _, dup := f.in[seq]; dup {
		PreSendRetr.Add(1)
		return
	}
	// Evict the slot we're about to overwrite, then insert.
	if f.count == preSendRingSize {
		old := f.ring[f.head]
		delete(f.in, old)
	} else {
		f.count++
	}
	f.ring[f.head] = seq
	f.head = (f.head + 1) % preSendRingSize
	f.in[seq] = struct{}{}
	if f.count == 1 || int32(seq-f.maxSeq) >= 0 {
		f.maxSeq = seq
		return
	}
	// seq < maxSeq and not in ring ⇒ genuine reorder (or out of ring window —
	// indistinguishable, but at preSendRingSize=1024 the false-positive rate is
	// negligible for download bulk on the test rig).
	PreSendGenuine.Add(1)
}

// ForwardReseq restores per-flow ordering on the server's download (forward)
// path. The N AF_XDP dispatch goroutines (one per NIC RX queue) race to deliver
// a connection's return packets, and on NICs/clouds where RSS does NOT keep a
// single flow on one RX queue (no-RSS virtio steering, aRFS/RFS migration,
// indirection-table reconfig) the packets of one flow arrive split across queues
// and get interleaved out of order. RSS with a Toeplitz flow-hash never splits a
// flow, so in that case this stage is a no-op (every packet arrives in order and
// is emitted immediately) — it solves BOTH cases.
//
// It reorders by the inner TCP sequence number, which is the only true-order
// reference once a flow has been split (our own per-tunnel seq is stamped after
// this point, so it can't recover pre-split order). Non-TCP and zero-payload TCP
// (pure ACKs / window updates) pass through immediately — only data segments are
// reordered.
//
// A ForwardReseq is owned by a single connection's tunChan-consumer goroutine, so
// it needs no locking. Because this fork never retransmits datagrams, a missing
// segment is skipped once the per-flow window fills or the flow goes idle
// (gap-skip), so a real loss never stalls delivery.
type ForwardReseq struct {
	flows  map[flowKey]*reseqFlow
	window int           // max buffered out-of-order segments per flow before gap-skip
	maxAge time.Duration // a flow idle longer than this is flushed + reaped
}

type flowKey [13]byte // srcIP(4) dstIP(4) srcPort(2) dstPort(2) proto(1)

type reseqEntry struct {
	data      []byte // owned copy of the full IP packet
	nextAfter uint32 // TCP seq immediately following this segment
}

type reseqFlow struct {
	primed   bool
	nextSeq  uint32
	buf      map[uint32]reseqEntry
	lastSeen time.Time
}

// NewForwardReseq creates a resequencer. window ~64 covers the observed
// adjacent-swap reorder with headroom; maxAge ~5ms bounds added latency when a
// segment is genuinely missing.
func NewForwardReseq(window int, maxAge time.Duration) *ForwardReseq {
	if window <= 0 {
		window = 64
	}
	if maxAge <= 0 {
		maxAge = 5 * time.Millisecond
	}
	return &ForwardReseq{flows: make(map[flowKey]*reseqFlow), window: window, maxAge: maxAge}
}

// Push feeds one inbound IP packet and appends the packets that are now ready to
// send, in per-flow order, to out (returning the extended slice). Packets emitted
// for the just-pushed segment reference ip directly (the caller must send them
// before reusing ip's backing buffer); packets drained from the buffer are owned
// copies. Reordered/buffered packets are copied, so ip may be recycled by the
// caller as soon as Push returns.
func (r *ForwardReseq) Push(ip []byte, now time.Time, out [][]byte) [][]byte {
	seq, nextAfter, key, ok := parseTCP(ip)
	if !ok {
		// Not reorderable (non-IPv4, non-TCP, or pure ACK/window update) —
		// pass straight through.
		return append(out, ip)
	}

	f := r.flows[key]
	if f == nil {
		f = &reseqFlow{buf: make(map[uint32]reseqEntry)}
		r.flows[key] = f
	}
	f.lastSeen = now

	// First segment of a flow primes the expected sequence.
	if !f.primed {
		f.primed = true
		f.nextSeq = seq
	}

	d := int32(seq - f.nextSeq)
	switch {
	case d == 0:
		// In order: emit now, advance, then drain any buffered successors.
		out = append(out, ip)
		f.nextSeq = nextAfter
		return r.drain(f, out)
	case d < 0:
		// Retransmit / already-delivered seq: hand it through unchanged. The
		// inner TCP receiver wants it; we don't touch nextSeq.
		return append(out, ip)
	default:
		// Future segment: buffer an owned copy and keep order intact.
		if _, exists := f.buf[seq]; !exists {
			cp := make([]byte, len(ip))
			copy(cp, ip)
			f.buf[seq] = reseqEntry{data: cp, nextAfter: nextAfter}
		}
		// Window full: presume the missing nextSeq is lost (no retransmission) and
		// skip the gap to the oldest buffered segment.
		if len(f.buf) > r.window {
			out, _ = r.skipGap(f, out)
		}
		return out
	}
}

// FlushExpired emits buffered segments for flows that have gone idle past maxAge
// (a genuinely missing segment must not stall the rest forever) and reaps empty
// idle flows. Call it periodically from the consumer loop.
func (r *ForwardReseq) FlushExpired(now time.Time, out [][]byte) [][]byte {
	for k, f := range r.flows {
		if now.Sub(f.lastSeen) < r.maxAge {
			continue
		}
		for len(f.buf) > 0 {
			var ok bool
			out, ok = r.skipGap(f, out)
			if !ok {
				break // only unrecoverable orphans remain — abandon them
			}
		}
		delete(r.flows, k)
	}
	return out
}

// drain emits buffered segments that are now consecutive with f.nextSeq.
func (r *ForwardReseq) drain(f *reseqFlow, out [][]byte) [][]byte {
	for {
		e, ok := f.buf[f.nextSeq]
		if !ok {
			return out
		}
		delete(f.buf, f.nextSeq)
		out = append(out, e.data)
		f.nextSeq = e.nextAfter
	}
}

// skipGap abandons the missing f.nextSeq, jumps to the oldest (nearest-future)
// buffered segment, emits it, and drains any now-consecutive successors. It also
// sweeps unrecoverable past-seq entries (dist <= 0), which can appear when an
// overlapping retransmit lands after a prior skip advanced nextSeq past it; left
// in place they would leak and spin FlushExpired's drain loop. Returns false when
// no future segment remains to emit (the caller must stop, not loop).
func (r *ForwardReseq) skipGap(f *reseqFlow, out [][]byte) ([][]byte, bool) {
	var oldest uint32
	best := int32(0)
	found := false
	for s := range f.buf {
		dist := int32(s - f.nextSeq)
		if dist <= 0 {
			delete(f.buf, s)
			continue
		}
		if !found || dist < best {
			found, best, oldest = true, dist, s
		}
	}
	if !found {
		return out, false
	}
	e := f.buf[oldest]
	delete(f.buf, oldest)
	out = append(out, e.data)
	f.nextSeq = e.nextAfter
	return r.drain(f, out), true
}

// parseTCP extracts the flow key, TCP sequence, and the sequence immediately
// following this segment. ok is false for anything that should bypass reordering:
// non-IPv4, non-TCP, truncated headers, or zero-advance segments (pure ACKs).
func parseTCP(ip []byte) (seq, nextAfter uint32, key flowKey, ok bool) {
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return 0, 0, key, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl+20 || ip[9] != 6 { // proto 6 = TCP
		return 0, 0, key, false
	}
	tcp := ip[ihl:]
	dataOff := int(tcp[12]>>4) * 4
	if dataOff < 20 || len(tcp) < dataOff {
		return 0, 0, key, false
	}
	seq = binary.BigEndian.Uint32(tcp[4:8])
	payloadLen := len(ip) - ihl - dataOff
	consumed := payloadLen
	flags := tcp[13]
	if flags&0x02 != 0 || flags&0x01 != 0 { // SYN or FIN each consume one seq
		consumed++
	}
	if consumed == 0 {
		// Pure ACK / window update: no data order to preserve.
		return 0, 0, key, false
	}
	copy(key[0:4], ip[12:16])  // src IP
	copy(key[4:8], ip[16:20])  // dst IP
	copy(key[8:10], tcp[0:2])  // src port
	copy(key[10:12], tcp[2:4]) // dst port
	key[12] = 6                // proto
	return seq, seq + uint32(consumed), key, true
}

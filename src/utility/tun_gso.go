//go:build linux

package utility

import (
	"encoding/binary"
	"expvar"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// FORWARD_TUN_GSO (REBUILT 2026-06-06 with the corrected TCP pseudo-checksum seed):
// coalesce same-flow contiguous inner TCP into a GSO super-frame and write it to a TUN
// opened IFF_TUN|IFF_NO_PI|IFF_VNET_HDR + TUNSETOFFLOAD(TUN_F_CSUM|TSO4|TSO6); the kernel
// forwards it out the WAN NIC with host-TSO. The PREVIOUS version collapsed; the wireguard-go
// reference shows the TCP partial-checksum field must be seeded with the pseudo-header sum over
// the FULL COALESCED TCP length (not length=0). A wrong seed makes host-TSO emit bad per-segment
// checksums -> silent target drops -> cwnd=1 (the collapse signature). DIAGNOSTIC: read the
// target's TcpInCsumErrors — if it drops to ~0 with this seed, the old length=0 seed was the bug.
// forwardViaTun: coalesce upload TCP into GSO super-frames written to the MAIN tun
// (water, opened IFF_VNET_HDR) so the kernel host-TSO-segments + ip_forwards them —
// no separate tmfwd0 device. Config-driven (FORWARD_TUN_GSO), set via SetForwardMode.
var forwardViaTun bool

const (
	vnetHdrLen   = 10
	gsoMaxL3     = 64000
	gsoMaxSegs   = 64
	iffVnetHdr   = 0x4000 // IFF_VNET_HDR (also used by tun_vhost.go)
	vGSONone     = 0
	vGSOTcpv4    = 1
	vFNeedsCsum  = 1
	ipprotoTCPv4 = 6
)

var tunMaxSegs = func() int {
	if s := os.Getenv("FORWARD_TUN_GSO_MAXSEGS"); s != "" {
		if n, e := strconv.Atoi(s); e == nil && n >= 1 && n <= gsoMaxSegs {
			return n
		}
	}
	return gsoMaxSegs
}()

var tunNoCoalesce = os.Getenv("FORWARD_TUN_NOCOALESCE") == "1"

// flushIdle: an open super-frame untouched this long is finalized (the latency
// bound on coalescing). Env-tunable for experiments: FORWARD_TUN_GSO_FLUSH_IDLE_US.
var flushIdle = func() time.Duration {
	if s := os.Getenv("FORWARD_TUN_GSO_FLUSH_IDLE_US"); s != "" {
		if n, e := strconv.Atoi(s); e == nil && n > 0 {
			return time.Duration(n) * time.Microsecond
		}
	}
	return 120 * time.Microsecond
}()

var (
	tunGsoWrites = expvar.NewInt("tun_gso_writes")
	tunGsoCoal   = expvar.NewInt("tun_gso_coalesced")
	tunGsoDrops  = expvar.NewInt("tun_gso_drops")
	// Close-reason counters (why a super-frame was finalized). With per-flow slots
	// a different flow's packet no longer closes anything, so the old dominant
	// reason ("flow switch") is gone by design; what remains tells us the next
	// bottleneck: gap = in-flow seq mismatch (retransmit/reorder), full = segment/
	// size cap reached, idle = flushIdle sweep, evict = slot-cap LRU eviction.
	tunGsoCloseGap   = expvar.NewInt("tun_gso_close_gap")
	tunGsoCloseFull  = expvar.NewInt("tun_gso_close_full")
	tunGsoCloseIdle  = expvar.NewInt("tun_gso_close_idle")
	tunGsoCloseEvict = expvar.NewInt("tun_gso_close_evict")
)

// tunMaxFlowSlots caps the per-batch (= per-connection) number of concurrently
// open super-frames. One slot per active inner TCP flow; a typical bench client
// runs -P12, so 32 covers it with room. At the cap, the least-recently-grown
// slot is flushed and reused (counted in tun_gso_close_evict). Each slot owns a
// ~64 KB build buffer, allocated lazily on first use and kept on a freelist.
const tunMaxFlowSlots = 32

type tcpView struct {
	ihl, thl   int
	seq        uint32
	payloadOff int
	payloadLen int
	key        [12]byte // srcIP(4) dstIP(4) srcPort(2) dstPort(2)
}

func parseV4TCP(pkt []byte) (v tcpView, ok bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return v, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 || pkt[9] != ipprotoTCPv4 {
		return v, false
	}
	tcp := pkt[ihl:]
	thl := int(tcp[12]>>4) * 4
	if thl < 20 || len(tcp) < thl {
		return v, false
	}
	v.ihl, v.thl = ihl, thl
	v.seq = binary.BigEndian.Uint32(tcp[4:8])
	v.payloadOff = ihl + thl
	v.payloadLen = len(pkt) - v.payloadOff
	copy(v.key[0:4], pkt[12:16])
	copy(v.key[4:8], pkt[16:20])
	copy(v.key[8:10], tcp[0:2])
	copy(v.key[10:12], tcp[2:4])
	return v, true
}

func sum16(b []byte, initial uint32) uint32 {
	ac := initial
	i := 0
	for ; i+1 < len(b); i += 2 {
		ac += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if i < len(b) {
		ac += uint32(b[i]) << 8
	}
	return ac
}

func fold16(ac uint32) uint16 {
	for ac>>16 != 0 {
		ac = (ac & 0xffff) + (ac >> 16)
	}
	return uint16(ac)
}

// tcpPseudoSeedV4 returns the folded (NOT complemented) TCP CHECKSUM_PARTIAL seed: the
// pseudo-header sum over srcIP, dstIP, IPPROTO_TCP and the FULL coalesced TCP length
// (tcpLen = IP-total-len - ihl). ★ THE FIX: the old version passed length=0 here, which
// made host-TSO produce wrong per-segment checksums.
func tcpPseudoSeedV4(srcIP, dstIP []byte, tcpLen uint16) uint16 {
	ac := sum16(srcIP, 0)
	ac = sum16(dstIP, ac)
	ac += uint32(ipprotoTCPv4)
	ac += uint32(tcpLen)
	return fold16(ac)
}

// gsoSlot is one in-progress super-frame for one inner TCP flow. With one slot
// per flow, a packet from flow B no longer forces flow A's frame closed (the old
// single-open-frame design's dominant close reason — measured 7.4 segs/frame
// under rss vs 12.7 under none purely from interleave-driven early closes).
type gsoSlot struct {
	buf      []byte // [vnet(10) | ip | tcp | payload...]
	aoff     int
	segs     int
	ihl      int
	thl      int
	segSize  int // payload bytes per segment (gso_size)
	nextSeq  uint32
	lastGrow time.Time
}

type TunGSOBatch struct {
	fd      int
	slots   map[[12]byte]*gsoSlot // open frame per flow (only entries with segs>0)
	free    []*gsoSlot            // buffer freelist (slots are ~64 KB each)
	open    int                   // number of slots with segs>0
	scratch []byte                // writeNonGSO staging
}

// NewTunGSOBatch builds a coalescing writer over an EXISTING IFF_VNET_HDR tun fd
// (the main water tun's GSOFd). It writes TSO super-frames straight to that fd and
// the kernel segments + ip_forwards them — no dedicated tmfwd0 device is created.
// tunFd<0 means the main tun was not opened with GSO, which is a config/wiring bug.
// NOT goroutine-safe: one instance per connection goroutine (ordering within a
// flow is a writer-thread property — a flow must never span two instances).
func NewTunGSOBatch(tunFd int) (*TunGSOBatch, error) {
	if tunFd < 0 {
		return nil, fmt.Errorf("FORWARD_TUN_GSO set but main tun was not opened with GSO (IFF_VNET_HDR); GSOFd=-1")
	}
	return &TunGSOBatch{
		fd:      tunFd,
		slots:   make(map[[12]byte]*gsoSlot, tunMaxFlowSlots),
		scratch: make([]byte, vnetHdrLen+gsoMaxL3+128),
	}, nil
}

func (b *TunGSOBatch) getSlot(key [12]byte) *gsoSlot {
	if s := b.slots[key]; s != nil {
		return s
	}
	if len(b.slots) >= tunMaxFlowSlots {
		b.evictOldest()
	}
	var s *gsoSlot
	if n := len(b.free); n > 0 {
		s = b.free[n-1]
		b.free = b.free[:n-1]
	} else {
		s = &gsoSlot{buf: make([]byte, vnetHdrLen+gsoMaxL3+128)}
	}
	b.slots[key] = s
	return s
}

// evictOldest flushes + removes the least-recently-grown slot (slot cap reached).
func (b *TunGSOBatch) evictOldest() {
	var oldKey [12]byte
	var old *gsoSlot
	for k, s := range b.slots {
		if old == nil || s.lastGrow.Before(old.lastGrow) {
			oldKey, old = k, s
		}
	}
	if old == nil {
		return
	}
	if old.segs > 0 {
		tunGsoCloseEvict.Add(1)
		b.flushSlot(old)
	}
	delete(b.slots, oldKey)
	b.free = append(b.free, old)
}

func (b *TunGSOBatch) Add(pkt []byte, _ net.HardwareAddr) error {
	if tunNoCoalesce {
		return b.writeNonGSO(pkt)
	}
	v, ok := parseV4TCP(pkt)
	if !ok {
		return b.writeNonGSO(pkt)
	}
	s := b.getSlot(v.key)
	if s.segs > 0 &&
		v.seq == s.nextSeq &&
		v.ihl == s.ihl && v.thl == s.thl &&
		v.payloadLen <= s.segSize &&
		s.segs < tunMaxSegs &&
		(s.aoff-vnetHdrLen)+v.payloadLen <= gsoMaxL3 &&
		tcpOptsEqual(pkt, v, s.buf[vnetHdrLen:], s.ihl, s.thl) {
		copy(s.buf[s.aoff:], pkt[v.payloadOff:v.payloadOff+v.payloadLen])
		s.aoff += v.payloadLen
		s.segs++
		s.nextSeq += uint32(v.payloadLen)
		s.lastGrow = time.Now()
		tunGsoCoal.Add(1)
		if v.payloadLen < s.segSize || s.segs >= tunMaxSegs ||
			(s.aoff-vnetHdrLen)+s.segSize > gsoMaxL3 {
			tunGsoCloseFull.Add(1)
			b.flushSlot(s)
		}
		return nil
	}
	if s.segs > 0 {
		// same flow, but seq gap / header change / size misfit: close and restart.
		tunGsoCloseGap.Add(1)
		b.flushSlot(s)
	}
	s.aoff = vnetHdrLen
	s.aoff += copy(s.buf[s.aoff:], pkt)
	s.segs = 1
	s.ihl = v.ihl
	s.thl = v.thl
	s.segSize = v.payloadLen
	s.nextSeq = v.seq + uint32(v.payloadLen)
	s.lastGrow = time.Now()
	b.open++
	return nil
}

// flushSlot finalizes a slot's super-frame and writes it. The slot stays in the
// map (flow likely continues); empty slots are reaped by the Flush sweep.
func (b *TunGSOBatch) flushSlot(s *gsoSlot) {
	if s.segs == 0 {
		return
	}
	ip := s.buf[vnetHdrLen:s.aoff]
	if s.segs == 1 {
		// single packet: already-valid SNAT'd checksum; emit GSO_NONE, no offload.
		for i := 0; i < vnetHdrLen; i++ {
			s.buf[i] = 0
		}
		b.writeFrame(s.buf[:s.aoff])
		s.segs = 0
		s.aoff = 0
		b.open--
		return
	}
	ihl := s.ihl
	l3 := s.aoff - vnetHdrLen
	// IP total length + checksum
	binary.BigEndian.PutUint16(ip[2:4], uint16(l3))
	ip[10], ip[11] = 0, 0
	binary.BigEndian.PutUint16(ip[10:12], ^fold16(sum16(ip[:ihl], 0)))
	// TCP CHECKSUM_PARTIAL seed over the FULL coalesced TCP length (the fix).
	tcp := ip[ihl:]
	tcpLen := uint16(l3 - ihl)
	seed := tcpPseudoSeedV4(ip[12:16], ip[16:20], tcpLen)
	binary.BigEndian.PutUint16(tcp[16:18], seed)
	// virtio_net_hdr
	v := s.buf[:vnetHdrLen]
	v[0] = vFNeedsCsum
	v[1] = vGSOTcpv4
	binary.LittleEndian.PutUint16(v[2:4], uint16(ihl+s.thl))   // hdr_len
	binary.LittleEndian.PutUint16(v[4:6], uint16(s.segSize))   // gso_size
	binary.LittleEndian.PutUint16(v[6:8], uint16(ihl))         // csum_start (L3 TUN: IP header len)
	binary.LittleEndian.PutUint16(v[8:10], 16)                 // csum_offset (TCP checksum)
	b.writeFrame(s.buf[:s.aoff])
	s.segs = 0
	s.aoff = 0
	b.open--
}

// writeNonGSO writes a single packet with a GSO_NONE virtio_net_hdr (pass-through).
func (b *TunGSOBatch) writeNonGSO(pkt []byte) error {
	if vnetHdrLen+len(pkt) > len(b.scratch) {
		tunGsoDrops.Add(1)
		return nil
	}
	for i := 0; i < vnetHdrLen; i++ {
		b.scratch[i] = 0
	}
	n := copy(b.scratch[vnetHdrLen:], pkt)
	return b.writeFrame(b.scratch[:vnetHdrLen+n])
}

func (b *TunGSOBatch) writeFrame(frame []byte) error {
	if _, err := unix.Write(b.fd, frame); err != nil {
		tunGsoDrops.Add(1)
		return err
	}
	tunGsoWrites.Add(1)
	return nil
}

// slotReapIdle: an empty slot untouched this long is removed and its buffer
// returned to the freelist (flow finished). Generous vs flushIdle so a live
// flow's slot isn't churned.
const slotReapIdle = 10 * time.Millisecond

// Flush sweeps all slots: finalizes frames idle >= flushIdle (the latency bound)
// and reaps long-empty slots. Called by the drain loop on batch boundaries and
// from its 250 µs ticker, so the sweep cadence is bounded even with no traffic.
func (b *TunGSOBatch) Flush(_ bool) error {
	now := time.Now()
	for k, s := range b.slots {
		if s.segs > 0 {
			if now.Sub(s.lastGrow) >= flushIdle {
				tunGsoCloseIdle.Add(1)
				b.flushSlot(s)
			}
		} else if now.Sub(s.lastGrow) >= slotReapIdle {
			delete(b.slots, k)
			b.free = append(b.free, s)
		}
	}
	return nil
}
func (b *TunGSOBatch) Full() bool  { return false }
func (b *TunGSOBatch) Empty() bool { return b.open == 0 }
func (b *TunGSOBatch) Close() {
	for _, s := range b.slots {
		b.flushSlot(s)
	}
}

// tcpOptsEqual reports whether pkt's TCP options match the open frame's (so the replicated
// header is valid for every coalesced segment — TSval etc. must be identical).
func tcpOptsEqual(pkt []byte, v tcpView, open []byte, ihl, thl int) bool {
	if v.thl != thl {
		return false
	}
	a := pkt[v.ihl+20 : v.ihl+thl]
	c := open[ihl+20 : ihl+thl]
	if len(a) != len(c) {
		return false
	}
	for i := range a {
		if a[i] != c[i] {
			return false
		}
	}
	return true
}

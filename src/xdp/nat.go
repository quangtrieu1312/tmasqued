//go:build linux

package xdp

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
	"golang.org/x/sys/unix"

	"github.com/cilium/ebpf"

	"github.com/quangtrieu1312/tmasqued/logger"
)

const (
	natMapPin     = "/sys/fs/bpf/nat_rev_table"
	idleTimeout   = 120 * time.Second
	gcInterval    = 30 * time.Second

	// Ephemeral port range allocated by the SNAT port allocator.
	// Kept away from the OS ephemeral range (32768-60999) intentionally.
	snatPortMin = 61000
	snatPortMax = 65535
)

// NatRevKey mirrors struct nat_rev_key in nat.h.
// All port/IP fields are stored in network byte order (big-endian) to match
// what the BPF program writes, because cilium/ebpf does not byte-swap structs.
type NatRevKey struct {
	WanIP   [4]byte
	DstIP   [4]byte // remote server IP (src IP of the reply)
	WanPort uint16  // our ephemeral WAN port (network byte order)
	DstPort uint16  // remote server port   (network byte order)
	Proto   uint8
	Pad     [3]byte
}

// NatRevVal mirrors struct nat_rev_val in nat.h (packed, 16 bytes).
type NatRevVal struct {
	ClientIP   [4]byte
	ClientPort uint16
	Pad        uint16
	LastSeenNs uint64
}

// flowKey identifies a unique outbound flow from a client's perspective.
// All ports are in host byte order.
type flowKey struct {
	clientIP   [4]byte
	dstIP      [4]byte
	clientPort uint16
	dstPort    uint16
	proto      uint8
}

// NatTable wraps the pinned BPF LRU-hash map and the port allocator.
type NatTable struct {
	m    *ebpf.Map
	mu   sync.Mutex

	// next ephemeral port (atomic bump-and-wrap)
	nextPort atomic.Uint32

	// fwd maps each outbound flow to its allocated WAN port so that
	// subsequent packets from the same flow reuse the same port instead
	// of consuming a new one on every call to AllocPort.
	fwd map[flowKey]uint16

	stopGC chan struct{}
}

// OpenNatTable opens the pinned BPF map created by the XDP program.
// Call once after xdp.Load(); the returned *NatTable is safe for concurrent use.
func OpenNatTable() (*NatTable, error) {
	m, err := ebpf.LoadPinnedMap(natMapPin, &ebpf.LoadPinOptions{})
	if err != nil {
		return nil, fmt.Errorf("open pinned NAT map %s: %w", natMapPin, err)
	}
	t := &NatTable{
		m:      m,
		fwd:    make(map[flowKey]uint16),
		stopGC: make(chan struct{}),
	}
	t.nextPort.Store(snatPortMin)
	go t.gcLoop()
	return t, nil
}

func monotonicNs() uint64 {
    var ts unix.Timespec
    unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
    return uint64(ts.Sec)*1e9 + uint64(ts.Nsec)
}

// AllocPort returns the WAN port for the given flow, allocating a new one
// only if the flow has not been seen before.  Subsequent packets from the
// same (clientIP, clientPort, proto, dstIP, dstPort) 5-tuple reuse the
// previously allocated port and refresh the idle timer in the BPF map.
//
// Returns the WAN port in host byte order.
func (t *NatTable) AllocPort(
	wanIP   netip.Addr,
	proto   uint8,
	dstIP   netip.Addr,
	dstPort uint16,
	clientIP   netip.Addr,
	clientPort uint16,
) (uint16, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fk := makeFlowKey(clientIP, clientPort, proto, dstIP, dstPort)

	// ── Fast path: flow already has a WAN port ────────────────────────────
	if wanPort, ok := t.fwd[fk]; ok {
		return wanPort, nil
	}

	// ── Slow path: new flow — find a free ephemeral port ──────────────────
	const rangeSize = snatPortMax - snatPortMin + 1
	for i := 0; i < rangeSize; i++ {
		p := t.bumpPort()
		key := makeNatKey(wanIP, p, proto, dstIP, dstPort)

		var val NatRevVal
		err := t.m.Lookup(&key, &val)
		if err != nil {
			// Slot free for this (wanPort, dstIP, dstPort) tuple.
			val = NatRevVal{
				ClientPort: hostToNet16(clientPort),
				LastSeenNs: monotonicNs(),
			}
			copy(val.ClientIP[:], clientIP.AsSlice())
			if err := t.m.Put(&key, &val); err != nil {
				return 0, fmt.Errorf("nat map put: %w", err)
			}
			t.fwd[fk] = p
			if logger.ShouldLog(logger.TRACE) {
				logger.Trace(fmt.Sprintf("NAT entry added: wanPort=%d dst=%s:%d client=%s:%d proto=%d", p, dstIP, dstPort, clientIP, clientPort, proto))
			}
			return p, nil
		}
	}
	return 0, fmt.Errorf("NAT port pool exhausted for dst %s:%d proto %d", dstIP, dstPort, proto)
}

// ReleasePort removes the reverse-NAT entry for the given flow.
func (t *NatTable) ReleasePort(wanIP netip.Addr, wanPort uint16, proto uint8, dstIP netip.Addr, dstPort uint16) error {
	key := makeNatKey(wanIP, wanPort, proto, dstIP, dstPort)
	t.mu.Lock()
	defer t.mu.Unlock()
	// Clean up the forward map entry using the client info stored in the BPF value.
	var val NatRevVal
	if err := t.m.Lookup(&key, &val); err == nil {
		delete(t.fwd, flowKeyFromNatEntry(key, val))
	}
	return t.m.Delete(&key)
}

// Close stops the GC goroutine and closes the map FD.
func (t *NatTable) Close() {
	close(t.stopGC)
	t.m.Close()
}

// ── internal ──────────────────────────────────────────────────────────────

func (t *NatTable) bumpPort() uint16 {
	for {
		p := t.nextPort.Load()
		next := p + 1
		if next > snatPortMax {
			next = snatPortMin
		}
		if t.nextPort.CompareAndSwap(p, next) {
			return uint16(p)
		}
	}
}

func (t *NatTable) gcLoop() {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.gc()
		case <-t.stopGC:
			return
		}
	}
}

func (t *NatTable) gc() {
	now := monotonicNs()
	idleNs := uint64(idleTimeout.Nanoseconds())

	var key, nextKey NatRevKey
	var val NatRevVal

	t.mu.Lock()
	defer t.mu.Unlock()

	iter := t.m.Iterate()
	for iter.Next(&key, &val) {
		if val.LastSeenNs == 0 {
			continue // never touched by XDP yet
		}
		// LastSeenNs is stamped by the BPF program concurrently with this scan.
		// If a flow was touched after we sampled `now`, last_seen > now and the
		// unsigned subtraction below would underflow to a huge value, wrongly
		// deleting an actively-used entry — which silently breaks its reverse-NAT
		// path (return traffic dropped → inner-TCP retransmits). Skip those.
		if val.LastSeenNs >= now {
			continue
		}
		if now-val.LastSeenNs > idleNs {
			delete(t.fwd, flowKeyFromNatEntry(key, val))
			_ = t.m.Delete(&key)
		}
	}
	_ = nextKey // suppress unused warning
}

// ── helpers ───────────────────────────────────────────────────────────────

func makeNatKey(wanIP netip.Addr, wanPort uint16, proto uint8, dstIP netip.Addr, dstPort uint16) NatRevKey {
	key := NatRevKey{
		WanPort: hostToNet16(wanPort),
		DstPort: hostToNet16(dstPort),
		Proto:   proto,
	}
	copy(key.WanIP[:], wanIP.AsSlice())
	copy(key.DstIP[:], dstIP.AsSlice())
	return key
}

func makeFlowKey(clientIP netip.Addr, clientPort uint16, proto uint8, dstIP netip.Addr, dstPort uint16) flowKey {
	fk := flowKey{
		clientPort: clientPort,
		dstPort:    dstPort,
		proto:      proto,
	}
	copy(fk.clientIP[:], clientIP.AsSlice())
	copy(fk.dstIP[:], dstIP.AsSlice())
	return fk
}

// flowKeyFromNatEntry reconstructs a flowKey from a BPF map entry.
// Ports in NatRevKey/NatRevVal are network byte order; hostToNet16 is
// self-inverse (it's a byte-swap), so calling it again converts back to
// host byte order.
func flowKeyFromNatEntry(key NatRevKey, val NatRevVal) flowKey {
	return flowKey{
		clientIP:   val.ClientIP,
		dstIP:      key.DstIP,
		clientPort: hostToNet16(val.ClientPort),
		dstPort:    hostToNet16(key.DstPort),
		proto:      key.Proto,
	}
}

func hostToNet16(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

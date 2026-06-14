//go:build linux

package xdp

import (
	"context"
	"encoding/binary"
	"expvar"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"sync"
	"time"

	"github.com/slavc/xdp"
	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"github.com/quangtrieu1312/tmasqued/config"
	"github.com/quangtrieu1312/tmasqued/logger"
)

// rxPollTimeoutMs is the timeout passed to poll() in the AF_XDP RX loop when the
// ring is empty. Default 5ms (block-and-yield: the dispatch goroutine sleeps
// between sparse packets, so the host can deschedule the idle vCPU → wake-latency
// jitter that is fatal to a single low-rate inner-TCP flow). Set XDP_RX_POLL_MS=0
// to BUSY-POLL: the loop spins (one core hot) and never yields, eliminating the
// deschedule/wake jitter on both the upload (QUIC ingest) and download (NAT-return
// ingest + inline SendDatagram) paths. Default 5; overridable via tmasqued.conf
// (applied at startup by LoadConfig).
var rxPollTimeoutMs = 5

// LoadConfig applies the xdp tuning knobs from the tmasqued.conf context. Call once
// at startup (after config.Load, before the RX loop starts).
func LoadConfig(ctx context.Context) {
	if n := config.Int(ctx, "XDP_RX_POLL_MS", rxPollTimeoutMs); n >= 0 {
		rxPollTimeoutMs = n
	}
}

const (
    ethHdr   = 14
    maxFrameSize = ethHdr + 4082
    umemFrameSize = 4096 // must match SocketOptions.FrameSize; upper bound on any RX frame
)

// rxBufPool recycles the payload buffers handed to quic-go over quicCh,
// removing a per-packet heap allocation from the QUIC RX dispatch hot path.
// ReadFrom returns the buffer once it has copied the data out. Buffers are
// *[]byte so Put is alloc-free. Sized to the UMEM frame size so any frame fits.
var rxBufPool = sync.Pool{
    New: func() any {
        b := make([]byte, umemFrameSize)
        return &b
    },
}

// xskRxRounds/xskRxFrames measure RX batch size: frames-per-receive-round
// (avg = xsk_rx_frames / xsk_rx_rounds). The de-batching diagnosis: under 16
// queues each ring drains in small rounds; under 1 queue rounds are large.
var (
	xskRxRounds = expvar.NewInt("xsk_rx_rounds")
	xskRxFrames = expvar.NewInt("xsk_rx_frames")
)

// quicShard is one queue's QUIC frame buffer (single producer, swept by the
// single ReadFrom consumer). Bounded: full shard drops (counted) like the old
// channel did.
type quicShard struct {
	mu  sync.Mutex
	buf []quicFrame
}

const quicShardCap = 1024

// quicRxSweeps/Frames measure consumer batch size (avg = frames/sweeps).
var (
	quicRxSweeps = expvar.NewInt("quic_rx_sweeps")
	quicRxFrames = expvar.NewInt("quic_rx_frames")
)

// quicFrame is a QUIC/UDP payload delivered to quic-go via ReadFrom.
type quicFrame struct {
    n    int
    addr net.Addr
    data []byte  // backed by bufp
    bufp *[]byte // pooled backing buffer; recycled by ReadFrom
}

// ForwardHandler delivers a NAT-return IP packet to the right tunnel. It MUST
// copy ipPkt synchronously (not retain the slice) — the backing UMEM frame is
// recycled the moment the handler returns. Returns true if delivered, false if
// no session matched dstIP (counted as a return-path drop).
type ForwardHandler func(ipPkt []byte, dstIP net.IP) bool

// Conn implements net.PacketConn over AF_XDP sockets.
// One XSK socket is created per NIC queue.
type Conn struct {
	sockets   []*xdp.Socket
	txPumps   []*txPump // one per socket; single owner of that socket's TX ring (de-funnels TX)
	localAddr *net.UDPAddr
	srcMAC    net.HardwareAddr
	gwMAC     net.HardwareAddr // next-hop fallback for IPs we haven't learned yet
	ifindex   int              // WAN interface index (for AF_PACKET-bound forward egress)

	// neigh is a copy-on-write IPv4→MAC neighbour table. We learn it by L2
	// reverse-path: the source MAC of any inbound frame is, by definition, the
	// correct next-hop MAC to reach that frame's source IP — true whether the
	// peer is directly on the WAN L2 or behind the gateway (then srcMAC is the
	// gateway's and srcIP the routed peer's, which is exactly what we want).
	//
	// This replaces a single global "peerMAC": on a flat L2 with multiple peers
	// (clients + forwarded targets + gateway), one shared next-hop MAC is wrong —
	// a target's reply frame would overwrite it and misdeliver QUIC returns meant
	// for a client. Keyed by IP, each peer keeps its own next hop.
	//
	// Reads (TX hot path) are a lock-free atomic load + map read. Writes are rare
	// (only when a new peer is seen or its MAC changes) and clone-on-write under
	// neighMu, so readers never see a half-built map.
	neigh   atomic.Pointer[map[netip.Addr]net.HardwareAddr]
	neighMu sync.Mutex

	// arpPending dedups in-flight async ARP probes for on-link LAN destinations
	// (LanNextHopMAC). Keyed by netip.Addr; an entry means a probe goroutine is
	// already resolving that destination, so we don't spawn another per packet.
	arpPending sync.Map

	txIdx     atomic.Uint64
	done      chan struct{}
	mode	XDPMode
	// QUIC/443 RX handoff: per-queue SHARDS instead of one shared channel.
	// One producer (the queue's dispatch goroutine) per shard kills the 16-way
	// chan-lock contention; the consumer (ReadFrom) sweeps all shards under one
	// short lock each and serves frames from a local batch — amortizing the
	// per-frame lock+wakeup that dominated the 16q profile (selectgo/lock2).
	// Per-flow order: a flow lives on ONE queue → ONE shard (slice = FIFO) →
	// batch sweep preserves intra-shard order. Cross-shard interleave is
	// cross-flow and therefore unordered by definition.
	quicShards []quicShard
	rxNotify   chan struct{} // 1-buffered edge trigger: a shard went nonempty
	rxPending  []quicFrame   // consumer-local batch (single ReadFrom caller)
	rxPendIdx  int
    fwdHandler atomic.Pointer[ForwardHandler] // NAT-return delivery, called inline per dispatch shard
	quicChDrops atomic.Uint64
	fwdDrops    atomic.Uint64 // return packets with no matching session
	txDrops     atomic.Uint64 // TX UMEM exhausted → outbound drops
}

// SetForwardHandler registers the NAT-return delivery callback. Safe to call
// after NewConn; dispatch goroutines pick it up atomically.
func (c *Conn) SetForwardHandler(h ForwardHandler) {
	c.fwdHandler.Store(&h)
}

// NewConn creates AF_XDP sockets for each NIC queue, registers them into xskMap,
// and returns a net.PacketConn ready for quic-go.
func NewConn(
	iface *net.Interface,
	xskQuicMap *ebpf.Map, // xsks_quic BPF map
    xskFwdMap  *ebpf.Map, // xsks_fwd  BPF map (same FDs, different map)
	localAddr *net.UDPAddr,
	numQueues int,
	mode XDPMode,
	needWakeup bool,
	quicViaKernel bool, // skip registering xsks_quic → UDP/443 falls through to the kernel
) (*Conn, error) {
	// If localAddr has an unspecified IP (0.0.0.0), resolve the real
    // IP from the interface. The raw Ethernet frames we build need a
    // valid unicast source — the kernel won't fill this in for us.
    frameLocalAddr := localAddr
    if localAddr.IP == nil || localAddr.IP.IsUnspecified() {
        ifaceIP, err := resolveIfaceIP(iface)
        if err != nil {
            return nil, fmt.Errorf("resolving IP for %s: %w", iface.Name, err)
        }
        frameLocalAddr = &net.UDPAddr{IP: ifaceIP, Port: localAddr.Port}
    }

	gwMAC, err := resolveNextHopMAC(iface, localAddr.IP)
	if err != nil {
		return nil, fmt.Errorf("resolving next-hop MAC: %w", err)
	}
	if gwMAC == nil && logger.ShouldLog(logger.INFO) {
		// On-link LAN NIC (no gateway): egress MACs are resolved per destination
		// (LanNextHopMAC) from return traffic / the kernel ARP table.
		logger.Info(fmt.Sprintf("NIC %s has no gateway fallback (on-link LAN); per-destination next-hop resolution", iface.Name))
	}

	sockets := make([]*xdp.Socket, numQueues)
	if mode == XDPModeGeneric {
    	xdp.DefaultSocketFlags = unix.XDP_COPY
	}
	// Native mode: always shoot for zero-copy first (virtio supports it on 6.11+,
	// single-buffer), and fail over to copy mode when the driver rejects the bind.
	// Forcing the flag (instead of flags=0 auto-negotiation) is deliberate: the
	// bind result tells us which datapath we actually got, so it can be logged
	// (auto mode falls back silently). The first queue's outcome decides for all
	// queues — ZC support is per-driver, not per-queue.
	zeroCopy := false
	if mode != XDPModeGeneric {
		xdp.DefaultSocketFlags = unix.XDP_ZEROCOPY
		zeroCopy = true
	}
	for i := 0; i < numQueues; i++ {
		opts := &xdp.SocketOptions{
    		NumFrames:              4096,  // 2048 TX + 2048 RX
    		FrameSize:              4096,
    		FillRingNumDescs:       2048,
    		CompletionRingNumDescs: 2048,
    		RxRingNumDescs:         2048,
    		TxRingNumDescs:         2048,
    		UseNeedWakeup:          needWakeup,
		}
		sock, err := xdp.NewSocket(iface.Index, i, opts)
		if err != nil && zeroCopy && i == 0 {
			// driver can't do ZC (e.g. virtio without VIRTIO_F_ACCESS_PLATFORM,
			// or any virtio on <6.11) → fail over to copy mode. Log the errno —
			// it says WHY (EINVAL = dma-dev/queue gate, EOPNOTSUPP = no driver
			// ZC support, EBUSY = queue occupied).
			if logger.ShouldLog(logger.WARN) {
				logger.Warn(fmt.Sprintf("AF_XDP zero-copy bind rejected (queue 0), falling back to copy: %v", err))
			}
			zeroCopy = false
			xdp.DefaultSocketFlags = unix.XDP_COPY
			sock, err = xdp.NewSocket(iface.Index, i, opts)
		}
		if err != nil {
			closeSockets(sockets[:i])
			return nil, fmt.Errorf("XSK queue %d (XDP mode %s): %w", i, mode, err)
		}

		nFill := sock.NumFreeFillSlots()
    	sock.Fill(sock.GetDescs(nFill, true))

		// quicViaKernel: do NOT register xsks_quic, so the XDP prog's UDP/443 redirect
		// (xdp.c: `if (lookup(xsks_quic)) redirect`) finds no socket and falls through to
		// XDP_PASS → the kernel UDP stack receives QUIC. The forward-return path (xsks_fwd
		// reverse-NAT redirect) is unaffected, so SNAT/return still work.
		if !quicViaKernel {
			if err := xskQuicMap.Update(uint32(i), uint32(sock.FD()), ebpf.UpdateAny); err != nil {
				sock.Close()
				closeSockets(sockets[:i])
				return nil, fmt.Errorf("xsks_quic update queue %d: %w", i, err)
			}
		}

		if err := xskFwdMap.Update(uint32(i), uint32(sock.FD()), ebpf.UpdateAny); err != nil {
			sock.Close()
			closeSockets(sockets[:i])
			return nil, fmt.Errorf("xsks_fwd update queue %d: %w", i, err)
		}

		sockets[i] = sock
	}

	if logger.ShouldLog(logger.INFO) {
		bindMode := "copy"
		if zeroCopy {
			bindMode = "zero-copy"
		}
		logger.Info(fmt.Sprintf("AF_XDP bind: %s", bindMode))
	}

	c := &Conn{
		sockets:   sockets,
		localAddr: frameLocalAddr,
		srcMAC:    iface.HardwareAddr,
		gwMAC:     gwMAC,
		ifindex:   iface.Index,
		done:      make(chan struct{}),
		mode:      mode,
		quicShards: make([]quicShard, numQueues),
		rxNotify:   make(chan struct{}, 1),
	}
	// One TX pump per socket: the sole owner of that socket's TX + completion
	// rings. All TX producers (QUIC WriteTo/WriteBatch + the forward path)
	// enqueue lock-free instead of contending a shared txMu. This is the
	// userspace analogue of the kernel's lockless qdisc that lets WireGuard
	// saturate many cores on a single NIC queue.
	c.txPumps = make([]*txPump, len(sockets))
	for i := range sockets {
		c.txPumps[i] = newTxPump(sockets[i], c.done)
	}
	// One dispatch goroutine per NIC queue. Each is the sole owner of its
	// socket's RX ring, so RX needs no lock and spreads across cores when
	// RSS hashes distinct 5-tuples (multiple tunnels) to distinct queues.
	for i := range sockets {
		go c.dispatchSocket(i)
	}
	return c, nil
}

// dispatchSocket owns one NIC queue's RX ring exclusively: it is the only
// goroutine that calls Receive/Fill/GetDescs(true) on this socket, so the RX
// path needs no lock. It reads every frame, inspects the destination port,
// and routes to the appropriate channel.
//
// NOTE: a single-queue RX-worker fan-out (1 ring-drainer + 1 per-frame worker on
// a 2nd core) was tried here and measured FLAT — the single-queue server ceiling
// is TX-side (one AF_XDP TX ring + one txMu funnel + single-core softirq), not RX
// dispatch, so engaging a 2nd core on RX buys nothing. See the perf log. The
// multi-core de-funnel belongs on the M3 multi-queue NIC, not here.
func (c *Conn) dispatchSocket(idx int) {
    sock := c.sockets[idx]
    pfds := []unix.PollFd{{Fd: int32(sock.FD()), Events: unix.POLLIN}}

    for {
        select {
        case <-c.done:
            return
        default:
        }

        // Keep the fill ring topped up with free RX frames.
        if n := sock.NumFreeFillSlots(); n > 0 {
            sock.Fill(sock.GetDescs(n, true))
        }

        numReady := sock.NumReceived()
        if numReady == 0 {
            pfds[0].Revents = 0
            if _, err := unix.Poll(pfds, rxPollTimeoutMs); err != nil {
                if err == unix.EINTR {
                    continue
                }
                return // fd closed on shutdown
            }
            if numReady = sock.NumReceived(); numReady == 0 {
                continue
            }
        }

        xskRxRounds.Add(1)
        xskRxFrames.Add(int64(numReady))
        descs := sock.Receive(numReady)
        for i := range descs {
            desc := descs[i]
            frame := sock.GetFrame(desc)
            frame = frame[:desc.Len]

            // L2 reverse-path learning: record (src IP → src MAC) so replies to
            // this peer use the right next hop. Only IPv4 frames carry an IP we
            // can key on; the fast path bails without allocating when the entry
            // is already present and unchanged (the common case).
            if len(frame) >= ethHdr+20 && binary.BigEndian.Uint16(frame[12:14]) == 0x0800 {
                var ip4 [4]byte
                copy(ip4[:], frame[ethHdr+12:ethHdr+16])
                c.learnNeighbour(netip.AddrFrom4(ip4), frame[6:12])
            }

            c.dispatchFrame(idx, frame, sock, desc)
        }
    }
}

func macEq(a net.HardwareAddr, b []byte) bool {
    if len(a) != 6 || len(b) < 6 {
        return false
    }
    return a[0] == b[0] && a[1] == b[1] && a[2] == b[2] &&
        a[3] == b[3] && a[4] == b[4] && a[5] == b[5]
}

// learnNeighbour records ip→mac in the copy-on-write neighbour table. The fast
// path (entry already present and unchanged) is a lock-free atomic load + map
// read with no allocation — the steady-state case for every inbound frame.
// Only a genuinely new or changed mapping takes neighMu and clones the map.
func (c *Conn) learnNeighbour(ip netip.Addr, mac []byte) {
    if m := c.neigh.Load(); m != nil {
        if cur, ok := (*m)[ip]; ok && macEq(cur, mac) {
            return
        }
    }

    c.neighMu.Lock()
    defer c.neighMu.Unlock()

    old := c.neigh.Load()
    if old != nil {
        if cur, ok := (*old)[ip]; ok && macEq(cur, mac) {
            return // re-check under lock: another goroutine already learned it
        }
    }

    var nm map[netip.Addr]net.HardwareAddr
    if old == nil {
        nm = make(map[netip.Addr]net.HardwareAddr, 8)
    } else {
        nm = make(map[netip.Addr]net.HardwareAddr, len(*old)+1)
        for k, v := range *old {
            nm[k] = v
        }
    }
    hw := make(net.HardwareAddr, 6)
    copy(hw, mac)
    nm[ip] = hw
    c.neigh.Store(&nm)
}

// NextHopMACForIP returns the L2 next-hop MAC to reach dstIP out the WAN. It
// returns the MAC learned from dstIP's own inbound frames (correct for both
// on-link and gateway-routed peers); if dstIP has not been learned yet it falls
// back to the default gateway MAC. ip may be 4- or 16-byte (v4-mapped); both
// resolve to the same IPv4 key used at learn time.
func (c *Conn) NextHopMACForIP(ip []byte) net.HardwareAddr {
    if a, ok := netip.AddrFromSlice(ip); ok {
        if m := c.neigh.Load(); m != nil {
            if mac, ok := (*m)[a.Unmap()]; ok {
                return mac
            }
        }
    }
    return c.gwMAC
}

// LanNextHopMAC resolves the L2 next hop for an on-link LAN destination egressing
// THIS NIC, for the AF_XDP-TX forward egress (which builds the Ethernet frame in
// userspace and so needs the destination's MAC — unlike the tun-GSO/kernel egress,
// which lets the kernel ARP). There is no gateway fallback on a LAN NIC, so:
//   1. learned neighbour table (primed by this flow's XDP NAT-reverse return) — hot;
//   2. on miss, the kernel ARP table once (cheap; the kernel keeps on-link entries
//      warm), caching the hit into neigh so step 1 serves it next time;
//   3. on a cold miss, a single deduped async ARP probe, returning nil so the caller
//      bootstraps this packet through the kernel (which ARPs + delivers). Because the
//      caller has ALREADY applied userspace SNAT, that kernel bootstrap keeps the
//      same WAN source port as the AF_XDP path — the flow never changes 5-tuple.
func (c *Conn) LanNextHopMAC(dst []byte) net.HardwareAddr {
	a, ok := netip.AddrFromSlice(dst)
	if !ok {
		return nil
	}
	a = a.Unmap()
	if m := c.neigh.Load(); m != nil {
		if mac, ok := (*m)[a]; ok {
			return mac
		}
	}
	ip := a.AsSlice()
	if mac, err := lookupNeighbourMAC(c.ifindex, ip); err == nil && len(mac) == 6 {
		c.learnNeighbour(a, mac)
		return mac
	}
	// Cold: probe once (deduped) so a later packet of this flow finds the entry.
	if _, busy := c.arpPending.LoadOrStore(a, struct{}{}); !busy {
		go func() {
			defer c.arpPending.Delete(a)
			_ = probeARP(ip)
			for range 5 {
				time.Sleep(200 * time.Millisecond)
				if mac, err := lookupNeighbourMAC(c.ifindex, ip); err == nil && len(mac) == 6 {
					c.learnNeighbour(a, mac)
					return
				}
			}
		}()
	}
	return nil
}

func (c *Conn) dispatchFrame(idx int, frame []byte, sock *xdp.Socket, desc xdp.Desc) {
    refill := func() {
        sock.Fill([]xdp.Desc{desc})
    }

    if len(frame) < ethHdr+1 {
        refill()
        return
    }

    ethertype := binary.BigEndian.Uint16(frame[12:14])

    switch ethertype {
    case 0x0800: // IPv4
        if len(frame) < ethHdr+20 {
            refill()
            return
        }
        ihl := int(frame[ethHdr]&0x0f) * 4
        proto := frame[ethHdr+9]

        if proto == 17 { // UDP
            udpBase := ethHdr + ihl
            if len(frame) >= udpBase+4 {
                dstPort := binary.BigEndian.Uint16(frame[udpBase+2 : udpBase+4])
                if dstPort == 443 {
                    c.dispatchQUIC(idx, frame, sock, desc, ihl)
                    return
                }
            }
        }
        // Everything else (TCP, non-443 UDP) → forwarding consumer.
        c.dispatchFwd(frame, sock, desc)

    case 0x86DD: // IPv6 — treat as forwarded for now; extend as needed
        c.dispatchFwd(frame, sock, desc)

    default:
        refill() // ARP, etc. — drop
    }
}

func (c *Conn) QuicChDrops() uint64 { return c.quicChDrops.Load() }
func (c *Conn) FwdDrops() uint64    { return c.fwdDrops.Load() }
func (c *Conn) TxDrops() uint64     { return c.txDrops.Load() }

// DiagSnapshot returns a one-line snapshot of XDP health for periodic logging.
// Ring depths are read without locking — a benign stats-only race, since each
// RX ring has a single owner goroutine and TX rings are mutated under txMus[i].
func (c *Conn) DiagSnapshot() string {
	quicLen := 0
	for i := range c.quicShards {
		sh := &c.quicShards[i]
		sh.mu.Lock()
		quicLen += len(sh.buf)
		sh.mu.Unlock()
	}
	quicCap := quicShardCap * len(c.quicShards)

	// Min free fill ring slots across all sockets (low = RX starvation risk).
	minFill, minTxFree := 99999, 99999
	// xsk KERNEL-level RX drops — the uninstrumented loss source. rxRingFull =
	// the kernel dropped because the RX ring was full (the single dispatch
	// goroutine couldn't drain a burst in time); fillEmpty = no fill buffers.
	// These become inner-TCP retransmits (unreliable datagrams don't recover).
	var rxDropped, rxRingFull, rxFillEmpty uint64
	for _, s := range c.sockets {
		if f := s.NumFreeFillSlots(); f < minFill {
			minFill = f
		}
		if f := s.NumFreeTxSlots(); f < minTxFree {
			minTxFree = f
		}
		if st, err := s.Stats(); err == nil {
			rxDropped += st.KernelStats.Rx_dropped
			rxRingFull += st.KernelStats.Rx_ring_full
			rxFillEmpty += st.KernelStats.Rx_fill_ring_empty_descs
		}
	}

	return fmt.Sprintf(
		"quicCh=%d/%d(drops=%d) fwdDrops=%d txDrops=%d fillFree=%d txFree=%d xskRxDrop=%d ringFull=%d fillEmpty=%d",
		quicLen, quicCap, c.quicChDrops.Load(),
		c.fwdDrops.Load(),
		c.txDrops.Load(),
		minFill, minTxFree,
		rxDropped, rxRingFull, rxFillEmpty,
	)
}

func (c *Conn) dispatchQUIC(idx int, frame []byte, sock *xdp.Socket, desc xdp.Desc, ihl int) {
    // Strip eth(14) + ipv4(ihl) + udp(8). Read everything out of the frame
    // BEFORE refilling, since Fill hands the buffer back to the kernel.
    udpBase := ethHdr + ihl
    if len(frame) < udpBase+8 {
        sock.Fill([]xdp.Desc{desc})
        return
    }
    srcPort := binary.BigEndian.Uint16(frame[udpBase : udpBase+2])
    payload := frame[udpBase+8:]
    n := len(payload)

    bufp := rxBufPool.Get().(*[]byte)
    data := (*bufp)[:n]
    copy(data, payload)

    srcIP := make(net.IP, 4)
    copy(srcIP, frame[ethHdr+12:ethHdr+16])

    sock.Fill([]xdp.Desc{desc})

    sh := &c.quicShards[idx]
    sh.mu.Lock()
    if len(sh.buf) >= quicShardCap {
        sh.mu.Unlock()
        rxBufPool.Put(bufp)
        c.quicChDrops.Add(1)
        return
    }
    sh.buf = append(sh.buf, quicFrame{n: n, addr: &net.UDPAddr{IP: srcIP, Port: int(srcPort)}, data: data, bufp: bufp})
    sh.mu.Unlock()
    select {
    case c.rxNotify <- struct{}{}:
    default:
    }
}

func (c *Conn) dispatchFwd(frame []byte, sock *xdp.Socket, desc xdp.Desc) {
    if len(frame) <= ethHdr+20 {
        sock.Fill([]xdp.Desc{desc})
        return
    }

    // Zero-copy: hand the raw IP packet (still in the UMEM frame) straight to
    // the handler, which copies it synchronously before we recycle the frame.
    ipPkt := frame[ethHdr:]

    var dstIP net.IP
    if (ipPkt[0] >> 4) == 4 {
        dstIP = ipPkt[16:20] // points into the frame; valid until Fill below
    }

    delivered := false
    if h := c.fwdHandler.Load(); h != nil && dstIP != nil {
        delivered = (*h)(ipPkt, dstIP)
    }
    if !delivered {
        c.fwdDrops.Add(1)
    }

    sock.Fill([]xdp.Desc{desc})
}

// ReadFrom satisfies net.PacketConn and returns only QUIC/443 payloads
// to quic-go. It will never see NAT-return tunnel traffic again.
func (c *Conn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		if c.rxPendIdx < len(c.rxPending) {
			f := c.rxPending[c.rxPendIdx]
			c.rxPending[c.rxPendIdx] = quicFrame{} // drop refs for GC
			c.rxPendIdx++
			n := copy(p, f.data)
			rxBufPool.Put(f.bufp) // quic-go copied into p; backing buffer is free now
			return n, f.addr, nil
		}
		// local batch exhausted: sweep every shard once (short lock each).
		c.rxPending = c.rxPending[:0]
		c.rxPendIdx = 0
		for i := range c.quicShards {
			sh := &c.quicShards[i]
			sh.mu.Lock()
			if len(sh.buf) > 0 {
				c.rxPending = append(c.rxPending, sh.buf...)
				sh.buf = sh.buf[:0]
			}
			sh.mu.Unlock()
		}
		if len(c.rxPending) > 0 {
			quicRxSweeps.Add(1)
			quicRxFrames.Add(int64(len(c.rxPending)))
			continue
		}
		select {
		case <-c.done:
			return 0, nil, net.ErrClosed
		case <-c.rxNotify:
		}
	}
}

// WriteTo builds a raw Ethernet+IPv4+UDP frame and sends it via AF_XDP.
// This is TX flow for QUIC
// txSocketIdx picks a TX queue by a STABLE hash of the remote address, so every
// packet of one QUIC connection uses the same NIC TX queue. Per-packet
// round-robin across queues reorders ~2% of a connection's packets (the queues
// drain at slightly different rates), which is enough to keep the inner TCP's
// cwnd pinned low via spurious fast-retransmits. Distinct connections (the
// bonded tunnels) still spread across queues for parallelism.
func (c *Conn) txSocketIdx(dst *net.UDPAddr) int {
	var h uint64 = 1469598103934665603 // FNV-1a offset basis
	for _, b := range dst.IP {
		h = (h ^ uint64(b)) * 1099511628211
	}
	h = (h ^ uint64(uint16(dst.Port))) * 1099511628211
	return int(h % uint64(len(c.sockets)))
}

func (c *Conn) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}

	dst, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, fmt.Errorf("expected *net.UDPAddr, got %T", addr)
	}

	// Stable per-connection queue (see txSocketIdx) — NOT round-robin.
	idx := c.txSocketIdx(dst)

	// Per-client next hop: address the frame to the MAC learned from this
	// client's own inbound traffic (gwMAC fallback until first packet seen).
	dstMAC := c.NextHopMACForIP(dst.IP)

	// Build the L2 frame on the stack and hand it to this socket's TX pump.
	// The pump (single ring owner) does Complete/GetDescs/Transmit — no lock
	// here, so concurrent connections never serialize on a shared txMu.
	var frame [txPumpFrameCap]byte
	total := buildUDPFrame(frame[:], c.srcMAC, dstMAC, c.localAddr, dst, p)
	c.txPumps[idx].Submit(frame[:total])
	return len(p), nil
}

func (c *Conn) Close() error {
	close(c.done)
	closeSockets(c.sockets)
	return nil
}

func (c *Conn) LocalAddr() net.Addr                { return c.localAddr }
func (c *Conn) SetDeadline(t time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(t time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { return nil }

// --- frame helpers ---

// buildUDPFrame writes eth + ipv4 + udp + payload into frame.
// Caller must ensure frame is large enough (42 + len(payload)).
func buildUDPFrame(frame []byte, srcMAC, dstMAC net.HardwareAddr, src, dst *net.UDPAddr, payload []byte) int {
	// Ethernet
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	frame[12], frame[13] = 0x08, 0x00 // EtherType IPv4

	// IPv4
	ipLen := 20 + 8 + len(payload)
	frame[14] = 0x45 // version=4, IHL=5 (no options)
	frame[15] = 0x00 // DSCP/ECN
	binary.BigEndian.PutUint16(frame[16:18], uint16(ipLen))
	binary.BigEndian.PutUint16(frame[18:20], 0)    // ID
	binary.BigEndian.PutUint16(frame[20:22], 0x4000) // DF flag, no fragment offset
	frame[22] = 64                                  // TTL
	frame[23] = 17                                  // proto = UDP
	frame[24], frame[25] = 0, 0                    // checksum placeholder
	copy(frame[26:30], src.IP.To4())
	copy(frame[30:34], dst.IP.To4())
	binary.BigEndian.PutUint16(frame[24:26], ipv4Checksum(frame[14:34]))

	// UDP (checksum=0 is legal for IPv4)
	binary.BigEndian.PutUint16(frame[34:36], uint16(src.Port))
	binary.BigEndian.PutUint16(frame[36:38], uint16(dst.Port))
	binary.BigEndian.PutUint16(frame[38:40], uint16(8+len(payload)))
	frame[40], frame[41] = 0, 0

	copy(frame[42:], payload)
	return 42 + len(payload)
}

func ipv4Checksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(hdr[i:]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// SetReadBuffer sets SO_RCVBUF on every underlying XSK file descriptor.
// quic-go calls this via interface{ SetReadBuffer(int) error } —
// without it, quic-go logs a "Not a *net.UDPConn?" warning and skips
// buffer tuning entirely. AF_XDP's actual data path uses UMEM, not the
// kernel socket buffer, but setting this silences the warning and
// applies the option consistently across all queue sockets.
func (c *Conn) SetReadBuffer(bytes int) error {
	for _, sock := range c.sockets {
		if err := unix.SetsockoptInt(sock.FD(), unix.SOL_SOCKET, unix.SO_RCVBUF, bytes); err != nil {
			return fmt.Errorf("SO_RCVBUF on XSK fd %d: %w", sock.FD(), err)
		}
	}
	return nil
}

// SetWriteBuffer sets SO_SNDBUF on every underlying XSK file descriptor.
func (c *Conn) SetWriteBuffer(bytes int) error {
	for _, sock := range c.sockets {
		if err := unix.SetsockoptInt(sock.FD(), unix.SOL_SOCKET, unix.SO_SNDBUF, bytes); err != nil {
			return fmt.Errorf("SO_SNDBUF on XSK fd %d: %w", sock.FD(), err)
		}
	}
	return nil
}

func closeSockets(sockets []*xdp.Socket) {
	for _, s := range sockets {
		if s != nil {
			s.Close()
		}
	}
}

// resolveIfaceIP returns the first non-loopback unicast IPv4 address on iface.
func resolveIfaceIP(iface *net.Interface) (net.IP, error) {
    addrs, err := iface.Addrs()
    if err != nil {
        return nil, err
    }
    for _, a := range addrs {
        var ip net.IP
        switch v := a.(type) {
        case *net.IPNet:
            ip = v.IP
        case *net.IPAddr:
            ip = v.IP
        }
        if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
            return ip4, nil
        }
    }
    return nil, fmt.Errorf("no IPv4 address found on %s", iface.Name)
}

// ForwardPump returns a TX pump (single owner of a socket's TX ring) and its
// Ethernet parameters, for constructing a utility.XDPBatch. Queues are selected
// round-robin. Submitting to the pump is lock-free, so forward consumers no
// longer serialize on a shared per-socket TX mutex — this was the single largest
// source of aggregate-throughput contention on a single-queue NIC.
func (c *Conn) ForwardPump() (*txPump, net.HardwareAddr, net.HardwareAddr, bool) {
    idx := int(c.txIdx.Add(1) % uint64(len(c.sockets)))
    return c.txPumps[idx], c.srcMAC, c.gwMAC, c.mode == XDPModeGeneric
}

// WanIfindex returns the WAN interface index, used to bind an AF_PACKET socket
// for the kernel-TX (qdisc/GSO) forward egress path.
func (c *Conn) WanIfindex() int { return c.ifindex }

// WriteBatch sends a batch of QUIC packets in a single XDP TX kick.
// Called by basicConn.WriteBatch via the nativeBatcher interface check.
func (c *Conn) WriteBatch(pkts [][]byte, addr net.Addr) error {
	select {
	case <-c.done:
		return net.ErrClosed
	default:
	}

	dst, ok := addr.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("expected *net.UDPAddr, got %T", addr)
	}

	n := len(pkts)
	if n == 0 {
		return nil
	}

	// Per-client next hop (see NextHopMACForIP); gwMAC fallback until learned.
	dstMAC := c.NextHopMACForIP(dst.IP)

	// Stable per-connection queue (see txSocketIdx) — NOT round-robin. Keeps every
	// packet of one QUIC connection on one TX queue so the inner stream (incl. the
	// tunneled ACK return path) stays in order; bonded tunnels still spread queues.
	idx := c.txSocketIdx(dst)
	pump := c.txPumps[idx]

	// Build each frame and enqueue to the TX pump (single ring owner). FIFO +
	// single producer per connection preserves this connection's packet order.
	var frame [txPumpFrameCap]byte
	for i := 0; i < n; i++ {
		total := buildUDPFrame(frame[:], c.srcMAC, dstMAC, c.localAddr, dst, pkts[i])
		pump.Submit(frame[:total])
	}
	return nil
}

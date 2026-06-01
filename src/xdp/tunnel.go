//go:build linux

package xdp

import (
    "bytes"
    "context"
    "encoding/binary"
    "fmt"
    "io"
    "net"
    "sync"
)

type TunnelConn interface {
    SendDatagram([]byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
    CloseWithError(code uint64, msg string) error
}

type TunnelSession struct {
    Conn        TunnelConn
    InnerIP     net.IP
    TunnelIndex int // this tunnel's index within the client's bonded set (0..count-1)
}

// sessionGroup holds the N bonded tunnels of a single client (Model A: all
// tunnels share one inner IP). sessions is indexed by TunnelIndex; a nil slot
// means that tunnel hasn't connected or has dropped.
type sessionGroup struct {
    sessions []*TunnelSession
    count    int // tunnel count the client declared
}

// pick selects the tunnel for a return packet using the symmetric 5-tuple hash,
// so a flow's return path matches the tunnel the client chose for it outbound.
// Falls back to the first live tunnel if the chosen slot is empty.
func (g *sessionGroup) pick(pkt []byte) *TunnelSession {
    if g.count <= 0 || len(g.sessions) == 0 {
        return nil
    }
    idx := int(flowHashSym(pkt) % uint32(g.count))
    if idx < len(g.sessions) && g.sessions[idx] != nil {
        return g.sessions[idx]
    }
    for _, s := range g.sessions {
        if s != nil {
            return s
        }
    }
    return nil
}

// SessionTable maps inner-tunnel IP → the client's bonded session group.
// Keyed by [4]byte (not innerIP.String()) so the return-path lookup is
// allocation-free on the hot dispatch goroutine.
type SessionTable struct {
    mu      sync.RWMutex
    byInner map[[4]byte]*sessionGroup
}

func NewSessionTable() *SessionTable {
    return &SessionTable{byInner: make(map[[4]byte]*sessionGroup)}
}

func ipKey(ip net.IP) (k [4]byte) {
    if v4 := ip.To4(); v4 != nil {
        copy(k[:], v4)
    }
    return
}

// Register adds a tunnel session at its declared TunnelIndex within the
// client's group; count is the total tunnels the client is bonding.
func (t *SessionTable) Register(s *TunnelSession, count int) {
    if count < 1 {
        count = 1
    }
    k := ipKey(s.InnerIP)
    t.mu.Lock()
    g := t.byInner[k]
    if g == nil {
        g = &sessionGroup{sessions: make([]*TunnelSession, count), count: count}
        t.byInner[k] = g
    }
    if count > g.count {
        g.count = count
    }
    for s.TunnelIndex >= len(g.sessions) {
        g.sessions = append(g.sessions, nil)
    }
    g.sessions[s.TunnelIndex] = s
    t.mu.Unlock()
}

// RemoveSession removes one tunnel from its client's group, dropping the whole
// group once its last tunnel disconnects.
func (t *SessionTable) RemoveSession(s *TunnelSession) {
    k := ipKey(s.InnerIP)
    t.mu.Lock()
    defer t.mu.Unlock()
    g := t.byInner[k]
    if g == nil {
        return
    }
    if s.TunnelIndex < len(g.sessions) && g.sessions[s.TunnelIndex] == s {
        g.sessions[s.TunnelIndex] = nil
    }
    for _, x := range g.sessions {
        if x != nil {
            return // group still has live tunnels
        }
    }
    delete(t.byInner, k)
}

// Deliver is the xdp.ForwardHandler: it re-encapsulates a NAT-return IP packet
// as a QUIC datagram back to the VPN client whose inner IP is dstIP, choosing
// the bonded tunnel by symmetric flow hash. Called inline on a dispatch shard
// goroutine, so it must stay fast and non-blocking — SendDatagram copies pkt
// synchronously and never blocks. Returns true if a tunnel matched.
func (t *SessionTable) Deliver(pkt []byte, dstIP net.IP) bool {
    if len(pkt) < 20 {
        return false
    }
    var k [4]byte
    copy(k[:], dstIP)

    t.mu.RLock()
    g := t.byInner[k]
    var sess *TunnelSession
    if g != nil {
        sess = g.pick(pkt)
    }
    t.mu.RUnlock()
    if sess == nil {
        return false // client disconnected or unknown inner IP
    }
    if err := sess.Conn.SendDatagram(pkt); err != nil {
        t.RemoveSession(sess) // client gone; clean up and drop
        return false
    }
    return true
}

// flowHashSym computes an order-independent (symmetric) hash over an IPv4
// packet's 5-tuple, so a flow's two directions hash to the same value. The
// bonded client MUST use the identical algorithm to pick its outbound tunnel.
//
// FNV-1a (32-bit) over the canonical byte sequence:
//   lo.ip(4) | lo.port(2,BE) | hi.ip(4) | hi.port(2,BE) | proto(1)
// where (lo, hi) are the two (ip, port) endpoints ordered ascending by IP
// bytes, then by port. Ports are 0 for non-TCP/UDP.
func flowHashSym(pkt []byte) uint32 {
    ihl := int(pkt[0]&0x0f) * 4
    proto := pkt[9]
    srcIP := pkt[12:16]
    dstIP := pkt[16:20]
    var srcPort, dstPort uint16
    if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
        srcPort = binary.BigEndian.Uint16(pkt[ihl : ihl+2])
        dstPort = binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
    }

    loIP, loPort, hiIP, hiPort := srcIP, srcPort, dstIP, dstPort
    if c := bytes.Compare(srcIP, dstIP); c > 0 || (c == 0 && srcPort > dstPort) {
        loIP, loPort, hiIP, hiPort = dstIP, dstPort, srcIP, srcPort
    }

    const offset32, prime32 = 2166136261, 16777619
    h := uint32(offset32)
    upd := func(b byte) { h ^= uint32(b); h *= prime32 }
    for _, b := range loIP {
        upd(b)
    }
    upd(byte(loPort >> 8))
    upd(byte(loPort))
    for _, b := range hiIP {
        upd(b)
    }
    upd(byte(hiPort >> 8))
    upd(byte(hiPort))
    upd(proto)
    return h
}

// ── IP allocation (minimal example — replace with your real allocator) ────

type IPPool struct {
    mu   sync.Mutex
    next uint32 // e.g. start at 10.0.0.2
    base uint32
    size uint32
}

func NewIPPool(subnet *net.IPNet) *IPPool {
    base := binary.BigEndian.Uint32(subnet.IP.To4())
    ones, bits := subnet.Mask.Size()
    return &IPPool{base: base, next: base + 1, size: 1 << (bits - ones)}
}

func (p *IPPool) Allocate() (net.IP, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if p.next >= p.base+p.size-1 {
        return nil, fmt.Errorf("IP pool exhausted")
    }
    ip := make(net.IP, 4)
    binary.BigEndian.PutUint32(ip, p.next)
    p.next++
    return ip, nil
}

// ── Framing helpers (only needed if you use streams instead of datagrams) ─
// QUIC datagrams are self-delimiting, so no framing is needed above.
// If you ever switch to streams, use these.

func WriteFramed(w io.Writer, pkt []byte) error {
    hdr := [2]byte{byte(len(pkt) >> 8), byte(len(pkt))}
    if _, err := w.Write(hdr[:]); err != nil {
        return err
    }
    _, err := w.Write(pkt)
    return err
}

func ReadFramed(r io.Reader) ([]byte, error) {
    var hdr [2]byte
    if _, err := io.ReadFull(r, hdr[:]); err != nil {
        return nil, err
    }
    n := int(hdr[0])<<8 | int(hdr[1])
    buf := make([]byte, n)
    _, err := io.ReadFull(r, buf)
    return buf, err
}

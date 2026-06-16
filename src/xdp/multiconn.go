//go:build linux

package xdp

import (
	"net"
	"sync"
	"time"
)

// MultiConn aggregates several per-NIC AF_XDP Conns into a single net.PacketConn
// for quic-go, so the QUIC server can accept client tunnels on ANY listening NIC
// (WAN_INTERFACE unset = listen on all). RX is merged across NICs; each client's
// source address is remembered against the Conn it arrived on, and WriteTo (the
// download datagrams + handshake) is dispatched back to that same Conn — so a
// connection keeps its NIC (and, inside that Conn, its queue) for the whole of
// its life, preserving the per-connection affinity the reorder-sensitive datapath
// depends on.
//
// NewMultiConn returns the single Conn unchanged when only one NIC listens (the
// common WAN_INTERFACE-set case), so that path keeps the Conn's own optimized
// quic-go interfaces (WriteBatch/GSO) with zero wrapping overhead.
type MultiConn struct {
	conns      []*Conn
	rxCh       chan *mcPkt
	addrToConn sync.Map // addr.String() -> *Conn
	done       chan struct{}
	closeOnce  sync.Once
}

type mcPkt struct {
	buf  *[]byte
	n    int
	addr net.Addr
	conn *Conn
}

var mcPktPool = sync.Pool{New: func() any { b := make([]byte, 2048); return &mcPkt{buf: &b} }}

// NewMultiConn builds a PacketConn over the given listening Conns. With exactly
// one Conn it returns that Conn directly (no wrapper).
func NewMultiConn(conns []*Conn) net.PacketConn {
	if len(conns) == 1 {
		return conns[0]
	}
	mc := &MultiConn{
		conns: conns,
		rxCh:  make(chan *mcPkt, 1024),
		done:  make(chan struct{}),
	}
	for _, c := range conns {
		go mc.rxLoop(c)
	}
	return mc
}

// rxLoop drains one Conn's RX and feeds the merged channel, recording the source
// address against this Conn so replies route back out the same NIC.
func (mc *MultiConn) rxLoop(c *Conn) {
	for {
		select {
		case <-mc.done:
			return
		default:
		}
		pk := mcPktPool.Get().(*mcPkt)
		n, addr, err := c.ReadFrom(*pk.buf)
		if err != nil {
			mcPktPool.Put(pk)
			select {
			case <-mc.done:
				return
			default:
				return // a Conn read error is terminal for that NIC's RX
			}
		}
		pk.n, pk.addr, pk.conn = n, addr, c
		mc.addrToConn.Store(addr.String(), c)
		select {
		case mc.rxCh <- pk:
		case <-mc.done:
			mcPktPool.Put(pk)
			return
		}
	}
}

func (mc *MultiConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pk := <-mc.rxCh:
		n := copy(p, (*pk.buf)[:pk.n])
		addr := pk.addr
		mcPktPool.Put(pk)
		return n, addr, nil
	case <-mc.done:
		return 0, nil, net.ErrClosed
	}
}

func (mc *MultiConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if v, ok := mc.addrToConn.Load(addr.String()); ok {
		return v.(*Conn).WriteTo(p, addr)
	}
	// No recorded ingress NIC yet (shouldn't happen server-side: the client's
	// initial packet is RX'd before we reply). Fall back to the first Conn.
	return mc.conns[0].WriteTo(p, addr)
}

func (mc *MultiConn) Close() error {
	mc.closeOnce.Do(func() { close(mc.done) })
	var firstErr error
	for _, c := range mc.conns {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// LocalAddr / deadlines delegate to the first Conn — quic-go only needs a
// representative local address, and the datapath ignores deadlines (it is
// always-on), matching the single-Conn behavior.
func (mc *MultiConn) LocalAddr() net.Addr                { return mc.conns[0].LocalAddr() }
func (mc *MultiConn) SetDeadline(t time.Time) error      { return mc.conns[0].SetDeadline(t) }
func (mc *MultiConn) SetReadDeadline(t time.Time) error  { return mc.conns[0].SetReadDeadline(t) }
func (mc *MultiConn) SetWriteDeadline(t time.Time) error { return mc.conns[0].SetWriteDeadline(t) }

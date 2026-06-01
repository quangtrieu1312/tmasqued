//go:build linux

package utility

import (
    "context"
	"sync/atomic"

    connectip "github.com/quic-go/connect-ip-go"
)

var TunAdapterDrops atomic.Uint64

// TunAdapterSends counts download packets pushed into tunChan via the AF_XDP
// forward path (SessionTable.Deliver). Compared against the TUN-read-loop
// producer (tunLoopSends in main.go) to confirm whether a download flow is
// split between two producers — the leading suspect for the inner reorder.
var TunAdapterSends atomic.Uint64

type ConnectIPAdapter struct {
    C       *connectip.Conn
    TunChan chan<- *Packet
}

func NewConnectIPAdapter(c *connectip.Conn, tunChan chan<- *Packet) *ConnectIPAdapter {
    return &ConnectIPAdapter{C: c, TunChan: tunChan}
}

func (a *ConnectIPAdapter) SendDatagram(p []byte) error {
    pkt := PacketPool.Get().(*Packet)
    pkt.N = copy(pkt.Buf[:], p)
    select {
    case a.TunChan <- pkt:
		TunAdapterSends.Add(1)
    default:
        PacketPool.Put(pkt)
		TunAdapterDrops.Add(1)
    }
    return nil
}

func (a *ConnectIPAdapter) ReceiveDatagram(ctx context.Context) ([]byte, error) {
    buf := make([]byte, 1500)
    n, err := a.C.ReadPacket(buf)
    if err != nil {
        return nil, err
    }
    return buf[:n], nil
}

func (a *ConnectIPAdapter) CloseWithError(code uint64, msg string) error {
    return a.C.Close()
}

//go:build linux

package utility

import (
    "net"
    "os"
    "sync"

    "github.com/slavc/xdp"
    "golang.org/x/sys/unix"
)

// forwardAckViaSocket routes small TCP packets (pure inner-TCP ACKs, <128B — a
// download's client→server feedback) through the kernel raw socket instead of
// AF_XDP. AF_XDP TX bypasses the kernel stack (qdisc, normal NIC TX timing); for a
// single-stream download the ACKs to the remote sender are SPARSE, and the XSK TX
// ring's per-packet timing/jitter disturbs the remote CUBIC sender's ACK clock →
// it under-paces with no loss (single-stream external P1-dn collapse: ~20 vs ~400
// Mbit). The kernel raw socket — the same path local-deliver (tunTapDevice) and
// WireGuard use — delivers sparse ACKs with clean timing, so the sender ramps.
// Bulk traffic (≥128B) stays on AF_XDP, where the ring is full and batching wins.
// On by default; kill switch: FORWARD_ACK_VIA_SOCKET=0. Read once at startup.
var forwardAckViaSocket = os.Getenv("FORWARD_ACK_VIA_SOCKET") != "0"

// ForwardBatch routes inner-tunnel IP packets to the right send path:
//   TCP / UDP  →  XDPBatch   (kernel-bypass)
//   everything else  →  SocketBatch  (raw socket; covers ICMP, etc.)
type ForwardBatch struct {
    xdp  *XDPBatch
    sock *SocketBatch
}

func NewForwardBatch(
    xdpSock        *xdp.Socket,
    srcMAC, dstMAC net.HardwareAddr,
    genericMode    bool,
    mu             *sync.Mutex,
) (*ForwardBatch, error) {
    sock, err := NewSocketBatch()
    if err != nil {
        return nil, err
    }
    return &ForwardBatch{
        xdp:  NewXDPBatch(xdpSock, srcMAC, dstMAC, genericMode, mu),
        sock: sock,
    }, nil
}

// Add routes pkt to the right send path. dstMAC is the resolved L2 next hop for
// the packet's destination; it is only used by the XDP path (the raw-socket
// path lets the kernel resolve the next hop via its own neighbour table).
func (b *ForwardBatch) Add(pkt []byte, dstMAC net.HardwareAddr) error {
    if isXDPEligible(pkt) {
        if forwardAckViaSocket && len(pkt) < 128 {
            return b.sock.Add(pkt) // route small TCP (ACKs) via kernel raw socket
        }
        return b.xdp.Add(pkt, dstMAC)
    }
    return b.sock.Add(pkt)
}

func (b *ForwardBatch) Flush() error {
    e1 := b.xdp.Flush(true)
    e2 := b.sock.Flush(true)
    if e1 != nil {
        return e1
    }
    return e2
}

func (b *ForwardBatch) Full() bool {
    return b.xdp.Full() || b.sock.Full()
}

// Empty reports whether both underlying batches hold no packets. The forward
// consumer uses this to flush as soon as its input channel drains, instead of
// letting sparse traffic (e.g. a download's inner-TCP ACKs) sit until the periodic
// ticker fires — that idle delay throttled the ACK-clocked remote sender (P1-dn).
func (b *ForwardBatch) Empty() bool {
    return b.xdp.Empty() && b.sock.Empty()
}

// ForwardSendOne is a single-packet send that applies the same routing rule.
// Use this for low-frequency one-offs (e.g. ICMP errors from WritePacket).
func ForwardSendOne(
    xdpSock     *xdp.Socket,
    srcMAC, dstMAC net.HardwareAddr,
    genericMode bool,
    mu          *sync.Mutex,
    pkt         []byte,
) error {
    if isXDPEligible(pkt) {
        return SendOne(xdpSock, srcMAC, dstMAC, genericMode, mu, pkt, false)
    }
    return SendOnSocket(pkt, false)
}

// isXDPEligible returns true only for TCP and UDP (IPv4 and IPv6).
func isXDPEligible(pkt []byte) bool {
    switch IPVersion(pkt) {
    case 4:
        if len(pkt) < 10 {
            return false
        }
        return pkt[9] == unix.IPPROTO_TCP || pkt[9] == unix.IPPROTO_UDP
    case 6:
        if len(pkt) < 7 {
            return false
        }
        return pkt[6] == unix.IPPROTO_TCP || pkt[6] == unix.IPPROTO_UDP
    }
    return false
}

func (b *ForwardBatch) Close() {
    b.sock.Close()
}

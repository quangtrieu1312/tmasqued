//go:build linux

package utility

import (
    "net"

    "golang.org/x/sys/unix"
)

// forwardAckViaSocket routes small TCP packets (pure inner-TCP ACKs, <128B — a
// download's client->server feedback) through the kernel raw socket instead of
// AF_XDP. AF_XDP TX bypasses the kernel stack (qdisc, normal NIC TX timing); for a
// single-stream download the ACKs to the remote sender are SPARSE, and the XSK TX
// ring's per-packet timing/jitter disturbs the remote CUBIC sender's ACK clock so
// it under-paces with no loss (single-stream external P1-dn collapse: ~20 vs ~400
// Mbit). The kernel raw socket — the same path local-deliver (tunTapDevice) and
// WireGuard use — delivers sparse ACKs with clean timing, so the sender ramps.
// Bulk traffic (>=128B) stays on the forward egress, where batching wins.
// On by default; set FORWARD_ACK_VIA_SOCKET=false in tmasqued.conf to disable
// (applied at startup by LoadConfig).
var forwardAckViaSocket = true

// forwardViaPacket routes XDP-eligible forwarded packets through an AF_PACKET-bound
// L2 egress (PacketBatch, one sendmmsg per batch) instead of AF_XDP TX. This is the
// WINNING 2-core forward path: batching the forward TX as individual frames roughly
// HALVED forward-path CPU (pprof: the per-packet write()/AF_XDP submit was ~41% of
// CPU) and lifted single-flow ~750->820M (then ->1080M stacked with AF_XDP RX +
// combined=1). Opt-in via FORWARD_KERNEL_TX=1; off by default (not yet A/B'd on 8-core).
//
// Dead siblings (tested NEGATIVE on the reprovisioned virtio): FORWARD_GSO (AF_PACKET+
// VNET_HDR) software-segmented in-guest; FORWARD_ALL_VIA_SOCKET (IPPROTO_RAW) overflowed
// the qdisc. NOTE: FORWARD_TUN_GSO is NOT dead — it was rebuilt with the corrected TCP
// checksum seed (tun_gso.go) and now coalesces TSO super-frames into the MAIN water tun
// (IFF_VNET_HDR) instead of a dedicated tmfwd0 device; selected via SetForwardMode.
// Off by default; set FORWARD_KERNEL_TX=true in tmasqued.conf (applied by LoadConfig).
var forwardViaPacket = false

// SetForwardMode selects the upload-forward egress from config (FORWARD_TUN_GSO /
// FORWARD_TUN_VHOST in tmasqued.conf), replacing the old env-var package init. Must be
// called once at startup BEFORE any NewForwardBatch. The caller defaults GSO to true
// when vhost is not selected (tun-GSO is the shipped default forward path).
func SetForwardMode(gso, vhost bool) {
    forwardViaTun = gso
    forwardViaVhost = vhost
}

// ForwardCoalesces reports whether the selected upload-forward egress coalesces
// packets into super-frames (tun-GSO or vhost) — the modes that need the upload
// resequencer (coalescing turns scattered 1-packet reorder into super-frame-sized
// gaps that collapse the inner-TCP cwnd).
func ForwardCoalesces() bool { return forwardViaTun || forwardViaVhost }

// fwdEgress is the common shape of the alternative (kernel-TX) forward egress.
type fwdEgress interface {
    Add(pkt []byte, dstMAC net.HardwareAddr) error
    Flush(enableStats bool) error
    Full() bool
    Empty() bool
    Close()
}

// ForwardBatch routes inner-tunnel IP packets to the right send path:
//   TCP / UDP        ->  XDPBatch     (kernel-bypass)               [default]
//                    ->  PacketBatch  (kernel qdisc TX, sendmmsg)   [FORWARD_KERNEL_TX=1]
//   everything else  ->  SocketBatch  (raw socket; covers ICMP, etc.)
type ForwardBatch struct {
    xdp  *XDPBatch
    alt  fwdEgress // non-nil when FORWARD_KERNEL_TX is set
    sock *SocketBatch
}

// gsoTunFd is the main water tun's GSOFd (>=0 only when it was opened IFF_VNET_HDR);
// used by the forwardViaTun (GSO) egress to write TSO super-frames to the main tun.
func NewForwardBatch(
    pump           FrameSubmitter,
    srcMAC, dstMAC net.HardwareAddr,
    wanIfindex     int,
    gsoTunFd       int,
) (*ForwardBatch, error) {
    sock, err := NewSocketBatch()
    if err != nil {
        return nil, err
    }
    fb := &ForwardBatch{
        xdp:  NewXDPBatch(pump, srcMAC, dstMAC),
        sock: sock,
    }
    switch {
    case forwardViaVhost:
        t, err := NewTunVhostBatch()
        if err != nil {
            sock.Close()
            return nil, err
        }
        fb.alt = t
    case forwardViaUring:
        t, err := NewTunUringBatch()
        if err != nil {
            sock.Close()
            return nil, err
        }
        fb.alt = t
    case forwardViaTun:
        t, err := NewTunGSOBatch(gsoTunFd)
        if err != nil {
            sock.Close()
            return nil, err
        }
        fb.alt = t
    case forwardViaNapiTun:
        t, err := NewTunNapiBatch()
        if err != nil {
            sock.Close()
            return nil, err
        }
        fb.alt = t
    case forwardViaPacket:
        p, err := NewPacketBatch(wanIfindex, srcMAC)
        if err != nil {
            sock.Close()
            return nil, err
        }
        fb.alt = p
    }
    return fb, nil
}

// Add routes pkt to the right send path. dstMAC is the resolved L2 next hop for
// the packet's destination; it is used by the XDP and AF_PACKET paths.
func (b *ForwardBatch) Add(pkt []byte, dstMAC net.HardwareAddr) error {
    if isXDPEligible(pkt) {
        // Sparse small TCP (pure ACKs, <128B) ALWAYS go via the kernel raw socket for
        // clean qdisc pacing — even when a coalescing/alt egress (GSO/vhost/uring/napi/
        // kernel-TX) is active. Checking this BEFORE b.alt is what lets the download
        // ACK-clock fix compose with the gso forward default (else gso buffers the ACKs
        // in the GSO tun and the download collapses).
        if forwardAckViaSocket && len(pkt) < 128 {
            return b.sock.Add(pkt) // route small TCP (ACKs) via kernel raw socket
        }
        if b.alt != nil {
            return b.alt.Add(pkt, dstMAC) // kernel TX (batched sendmmsg), like WG
        }
        return b.xdp.Add(pkt, dstMAC)
    }
    return b.sock.Add(pkt)
}

func (b *ForwardBatch) Flush() error {
    e1 := b.xdp.Flush(true)
    e2 := b.sock.Flush(true)
    var e3 error
    if b.alt != nil {
        e3 = b.alt.Flush(true)
    }
    if e1 != nil {
        return e1
    }
    if e2 != nil {
        return e2
    }
    return e3
}

func (b *ForwardBatch) Full() bool {
    if b.alt != nil && b.alt.Full() {
        return true
    }
    return b.xdp.Full() || b.sock.Full()
}

// Empty reports whether the underlying batches hold no packets. The forward
// consumer uses this to flush as soon as its input channel drains, instead of
// letting sparse traffic (e.g. a download's inner-TCP ACKs) sit until the periodic
// ticker fires — that idle delay throttled the ACK-clocked remote sender (P1-dn).
func (b *ForwardBatch) Empty() bool {
    if b.alt != nil && !b.alt.Empty() {
        return false
    }
    return b.xdp.Empty() && b.sock.Empty()
}

// ForwardSendOne is a single-packet send that applies the same routing rule.
// Use this for low-frequency one-offs (e.g. ICMP errors from WritePacket).
func ForwardSendOne(
    pump        FrameSubmitter,
    srcMAC, dstMAC net.HardwareAddr,
    pkt         []byte,
) error {
    if isXDPEligible(pkt) {
        return SendOne(pump, srcMAC, dstMAC, pkt, false)
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
    if b.alt != nil {
        b.alt.Close()
    }
}

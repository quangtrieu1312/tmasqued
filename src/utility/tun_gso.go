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

const flushIdle = 120 * time.Microsecond

var (
	tunGsoWrites = expvar.NewInt("tun_gso_writes")
	tunGsoCoal   = expvar.NewInt("tun_gso_coalesced")
	tunGsoDrops  = expvar.NewInt("tun_gso_drops")
)

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

type TunGSOBatch struct {
	fd       int
	buf      []byte // [vnet(10) | ip | tcp | payload...]
	aoff     int
	openSegs int
	openIHL  int
	openTHL  int
	openSeg  int // payload bytes per segment (gso_size)
	openNext uint32
	openKey  [12]byte
	lastGrow time.Time
}

// NewTunGSOBatch builds a coalescing writer over an EXISTING IFF_VNET_HDR tun fd
// (the main water tun's GSOFd). It writes TSO super-frames straight to that fd and
// the kernel segments + ip_forwards them — no dedicated tmfwd0 device is created.
// tunFd<0 means the main tun was not opened with GSO, which is a config/wiring bug.
func NewTunGSOBatch(tunFd int) (*TunGSOBatch, error) {
	if tunFd < 0 {
		return nil, fmt.Errorf("FORWARD_TUN_GSO set but main tun was not opened with GSO (IFF_VNET_HDR); GSOFd=-1")
	}
	return &TunGSOBatch{fd: tunFd, buf: make([]byte, vnetHdrLen+gsoMaxL3+128)}, nil
}

func (b *TunGSOBatch) Add(pkt []byte, _ net.HardwareAddr) error {
	if tunNoCoalesce {
		return b.writeNonGSO(pkt)
	}
	v, ok := parseV4TCP(pkt)
	if ok && b.openSegs > 0 &&
		v.key == b.openKey &&
		v.seq == b.openNext &&
		v.ihl == b.openIHL && v.thl == b.openTHL &&
		v.payloadLen <= b.openSeg &&
		b.openSegs < tunMaxSegs &&
		(b.aoff-vnetHdrLen)+v.payloadLen <= gsoMaxL3 &&
		tcpOptsEqual(pkt, v, b.buf[vnetHdrLen:], b.openIHL, b.openTHL) {
		copy(b.buf[b.aoff:], pkt[v.payloadOff:v.payloadOff+v.payloadLen])
		b.aoff += v.payloadLen
		b.openSegs++
		b.openNext += uint32(v.payloadLen)
		b.lastGrow = time.Now()
		tunGsoCoal.Add(1)
		if v.payloadLen < b.openSeg || b.openSegs >= tunMaxSegs ||
			(b.aoff-vnetHdrLen)+b.openSeg > gsoMaxL3 {
			b.flushOpen()
		}
		return nil
	}

	b.flushOpen()
	if ok {
		b.aoff = vnetHdrLen
		b.aoff += copy(b.buf[b.aoff:], pkt)
		b.openSegs = 1
		b.openIHL = v.ihl
		b.openTHL = v.thl
		b.openSeg = v.payloadLen
		b.openNext = v.seq + uint32(v.payloadLen)
		b.openKey = v.key
		b.lastGrow = time.Now()
		return nil
	}
	return b.writeNonGSO(pkt)
}

// flushOpen finalizes the open super-frame and writes it.
func (b *TunGSOBatch) flushOpen() {
	if b.openSegs == 0 {
		return
	}
	ip := b.buf[vnetHdrLen:b.aoff]
	if b.openSegs == 1 {
		// single packet: already-valid SNAT'd checksum; emit GSO_NONE, no offload.
		for i := 0; i < vnetHdrLen; i++ {
			b.buf[i] = 0
		}
		b.writeFrame(b.buf[:b.aoff])
		b.openSegs = 0
		b.aoff = 0
		return
	}
	ihl := b.openIHL
	l3 := b.aoff - vnetHdrLen
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
	v := b.buf[:vnetHdrLen]
	v[0] = vFNeedsCsum
	v[1] = vGSOTcpv4
	binary.LittleEndian.PutUint16(v[2:4], uint16(ihl+b.openTHL)) // hdr_len
	binary.LittleEndian.PutUint16(v[4:6], uint16(b.openSeg))     // gso_size
	binary.LittleEndian.PutUint16(v[6:8], uint16(ihl))           // csum_start (L3 TUN: IP header len)
	binary.LittleEndian.PutUint16(v[8:10], 16)                   // csum_offset (TCP checksum)
	b.writeFrame(b.buf[:b.aoff])
	b.openSegs = 0
	b.aoff = 0
}

// writeNonGSO writes a single packet with a GSO_NONE virtio_net_hdr (pass-through).
func (b *TunGSOBatch) writeNonGSO(pkt []byte) error {
	if vnetHdrLen+len(pkt) > len(b.buf) {
		tunGsoDrops.Add(1)
		return nil
	}
	for i := 0; i < vnetHdrLen; i++ {
		b.buf[i] = 0
	}
	n := copy(b.buf[vnetHdrLen:], pkt)
	return b.writeFrame(b.buf[:vnetHdrLen+n])
}

func (b *TunGSOBatch) writeFrame(frame []byte) error {
	if _, err := unix.Write(b.fd, frame); err != nil {
		tunGsoDrops.Add(1)
		return err
	}
	tunGsoWrites.Add(1)
	return nil
}

// Flush finalizes the open super-frame if it has gone idle.
func (b *TunGSOBatch) Flush(_ bool) error {
	if b.openSegs > 0 && time.Since(b.lastGrow) >= flushIdle {
		b.flushOpen()
	}
	return nil
}
func (b *TunGSOBatch) Full() bool  { return false }
func (b *TunGSOBatch) Empty() bool { return b.openSegs == 0 }
func (b *TunGSOBatch) Close()      {}

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

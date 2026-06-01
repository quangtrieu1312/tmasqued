//go:build linux

package xdp

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"
)


// applySNAT rewrites the source IP and source port of an outgoing TCP/UDP
// packet so it appears to come from wanAddr, and installs the reverse-NAT
// entry that the XDP program needs to rewrite return traffic.
//
// Non-TCP/UDP packets (ICMP etc.) are left untouched; they travel via the
// raw-socket path where iptables MASQUERADE still applies.
//
// pkt must be a raw IPv4 packet (no Ethernet header).
// The slice is modified in-place.
func ApplySNAT(pkt []byte, wanAddr netip.Addr, natTable *NatTable) error {
	if len(pkt) < 20 {
		return nil
	}
	if pkt[0]>>4 != 4 {
		return nil // IPv4 only
	}

	ihl := int(pkt[0]&0x0f) * 4
	if len(pkt) < ihl+4 {
		return nil
	}

	proto := pkt[9]
	if proto != unix.IPPROTO_TCP && proto != unix.IPPROTO_UDP {
		return nil // let ICMP etc. pass through unchanged
	}

	// Minimum transport header we need: 4 bytes (src+dst port) for port
	// extraction, 18 for TCP checksum field, 8 for UDP checksum field.
	minTransport := ihl + 4
	if proto == unix.IPPROTO_TCP {
		minTransport = ihl + 20
	} else {
		minTransport = ihl + 8
	}
	if len(pkt) < minTransport {
		return nil
	}

	// ── read fields (all in host-byte-order numeric values) ──────────────

	oldSrcIP := binary.BigEndian.Uint32(pkt[12:16])
	dstIPRaw := binary.BigEndian.Uint32(pkt[16:20])
	srcPort  := binary.BigEndian.Uint16(pkt[ihl : ihl+2])
	dstPort  := binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])

	newSrcIPBytes := wanAddr.As4()
	newSrcIP := binary.BigEndian.Uint32(newSrcIPBytes[:])

	if oldSrcIP == newSrcIP {
		// Already appears to come from the WAN IP — nothing to do.
		// (Shouldn't happen in practice but guard anyway.)
		return nil
	}

	var dstIPArr [4]byte
	binary.BigEndian.PutUint32(dstIPArr[:], dstIPRaw)
	dstAddr := netip.AddrFrom4(dstIPArr)

	var srcIPArr [4]byte
	binary.BigEndian.PutUint32(srcIPArr[:], oldSrcIP)
	srcAddr := netip.AddrFrom4(srcIPArr)

	// ── allocate WAN port & install reverse-NAT entry ────────────────────

	wanPort, err := natTable.AllocPort(wanAddr, proto, dstAddr, dstPort, srcAddr, srcPort)
	if err != nil {
		return fmt.Errorf("SNAT AllocPort: %w", err)
	}

	// ── rewrite IP header ─────────────────────────────────────────────────

	ipCsum := binary.BigEndian.Uint16(pkt[10:12])
	ipCsum = CsumUpdate32(ipCsum, oldSrcIP, newSrcIP)
	binary.BigEndian.PutUint32(pkt[12:16], newSrcIP)
	binary.BigEndian.PutUint16(pkt[10:12], ipCsum)

	// ── rewrite transport header ──────────────────────────────────────────

	switch proto {
	case unix.IPPROTO_TCP:
		// TCP checksum is at offset 16 from the start of the TCP header.
		tcpCsum := binary.BigEndian.Uint16(pkt[ihl+16 : ihl+18])
		tcpCsum = CsumUpdate32(tcpCsum, oldSrcIP, newSrcIP) // pseudo-header src IP
		tcpCsum = CsumUpdate16(tcpCsum, srcPort, wanPort)   // src port field
		binary.BigEndian.PutUint16(pkt[ihl+16:ihl+18], tcpCsum)

	case unix.IPPROTO_UDP:
		// UDP checksum is at offset 6 from the start of the UDP header.
		// A value of 0 means the sender disabled checksumming — leave it.
		udpCsum := binary.BigEndian.Uint16(pkt[ihl+6 : ihl+8])
		if udpCsum != 0 {
			udpCsum = CsumUpdate32(udpCsum, oldSrcIP, newSrcIP)
			udpCsum = CsumUpdate16(udpCsum, srcPort, wanPort)
			binary.BigEndian.PutUint16(pkt[ihl+6:ihl+8], udpCsum)
		}
	}

	// Write new source port last (after checksum is patched).
	binary.BigEndian.PutUint16(pkt[ihl:ihl+2], wanPort)

	return nil
}

// ── incremental checksum helpers (RFC 1624) ───────────────────────────────
//
// All values are treated as network-byte-order numeric values consistent with
// how binary.BigEndian.Uint16/Uint32 reads them from a packet.

// csumUpdate16 removes old's contribution from csum and adds new_'s.
func CsumUpdate16(csum, old, new_ uint16) uint16 {
	// HC' = ~( ~HC + ~old + new )  (ones-complement arithmetic)
	sum := uint32(^csum) + uint32(^old) + uint32(new_)
	// Fold 32-bit result to 16 bits.
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// csumUpdate32 updates csum for a 32-bit field change (e.g. an IP address),
// treating the field as two consecutive big-endian 16-bit words.
func CsumUpdate32(csum uint16, old, new_ uint32) uint16 {
	csum = CsumUpdate16(csum, uint16(old>>16), uint16(new_>>16))
	csum = CsumUpdate16(csum, uint16(old), uint16(new_))
	return csum
}

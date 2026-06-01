//go:build linux

package utility

// FlowHash returns an FNV-1a hash of an IPv4 packet's flow identity: src+dst IP,
// protocol, and (for TCP/UDP) src+dst ports. Used to pin a flow to a fixed
// bonded tunnel (ECMP/LACP-style) so its packets never reorder across tunnels.
// Non-TCP/UDP or truncated packets hash on the 3-tuple they do have.
func FlowHash(ip []byte) uint32 {
	const offset32 = 2166136261
	const prime32 = 16777619
	h := uint32(offset32)
	mix := func(b byte) { h ^= uint32(b); h *= prime32 }

	if len(ip) < 20 || ip[0]>>4 != 4 {
		for i := 0; i < len(ip) && i < 20; i++ {
			mix(ip[i])
		}
		return h
	}
	for i := 12; i < 20; i++ { // src IP (12:16) + dst IP (16:20)
		mix(ip[i])
	}
	proto := ip[9]
	mix(proto)
	ihl := int(ip[0]&0x0f) * 4
	if (proto == 6 || proto == 17) && ihl >= 20 && len(ip) >= ihl+4 {
		mix(ip[ihl])     // src port hi
		mix(ip[ihl+1])   // src port lo
		mix(ip[ihl+2])   // dst port hi
		mix(ip[ihl+3])   // dst port lo
	}
	return h
}

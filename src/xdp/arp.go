//go:build linux

package xdp

import (
	"fmt"
	"net"
	"time"

	"github.com/vishvananda/netlink"
)

// resolveNextHopMAC finds the MAC address of the next hop for outbound packets
// egressing iface. If the ARP entry is stale or missing it probes the gateway and
// retries.
//
// A NIC with no gateway reachable through it (a directly-connected LAN — e.g. the
// secondary NIC a multi-NIC gateway bridges its clients onto) has NO single
// next-hop fallback: every on-link destination's MAC is resolved per-packet (from
// return traffic / the kernel ARP table). For such a NIC this returns (nil, nil) —
// the caller keeps gwMAC == nil and the LAN egress resolves per destination. This
// is deliberately NOT an error, so the datapath comes up on every real NIC.
func resolveNextHopMAC(iface *net.Interface, localIP net.IP) (net.HardwareAddr, error) {
	gwIP, err := gatewayIPForIface(iface.Index)
	if err != nil {
		return nil, fmt.Errorf("finding gateway for %s: %w", iface.Name, err)
	}
	if gwIP == nil {
		return nil, nil // on-link LAN NIC: no gateway fallback (per-destination resolution)
	}

	// Fast path: already in the neighbour cache.
	if mac, err := lookupNeighbourMAC(iface.Index, gwIP); err == nil {
		return mac, nil
	}

	// Slow path: trigger kernel ARP resolution by connecting a UDP socket.
	// connect() forces a route + neighbour lookup without sending any data.
	if err := probeARP(gwIP); err != nil {
		return nil, fmt.Errorf("ARP probe for gateway %s: %w", gwIP, err)
	}

	// Retry for up to 1 second.
	for range 5 {
		time.Sleep(200 * time.Millisecond)
		if mac, err := lookupNeighbourMAC(iface.Index, gwIP); err == nil {
			return mac, nil
		}
	}
	return nil, fmt.Errorf("ARP resolution timed out for gateway %s", gwIP)
}

// probeARP triggers kernel ARP resolution for ip without sending any data.
// connect() on a UDP socket forces the kernel to resolve the neighbour entry.
func probeARP(ip net.IP) error {
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: 1})
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// gatewayIPForIface returns the IPv4 gateway of the default route that egresses
// the given interface, or (nil, nil) if no default route uses this NIC (a
// directly-connected LAN with on-link delivery only). A netlink failure is a real
// error; the absence of a gateway is not.
func gatewayIPForIface(ifaceIndex int) (net.IP, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("listing routes: %w", err)
	}
	for _, r := range routes {
		if r.Gw == nil {
			continue
		}
		if r.LinkIndex != ifaceIndex {
			continue // a gateway reachable via a DIFFERENT NIC is not our next hop
		}
		// default route can be nil Dst OR 0.0.0.0/0
		if r.Dst == nil {
			return r.Gw, nil
		}
		ones, bits := r.Dst.Mask.Size()
		if ones == 0 && bits == 32 {
			return r.Gw, nil
		}
	}
	return nil, nil // no default route via this NIC — on-link LAN
}

// lookupNeighbourMAC looks up the MAC for ip in the kernel neighbour (ARP) table.
func lookupNeighbourMAC(ifaceIndex int, ip net.IP) (net.HardwareAddr, error) {
	neighs, err := netlink.NeighList(ifaceIndex, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("listing neighbours: %w", err)
	}
	for _, n := range neighs {
		if n.IP.Equal(ip) && len(n.HardwareAddr) > 0 {
			return n.HardwareAddr, nil
		}
	}
	return nil, fmt.Errorf("no ARP entry for %s on interface %d", ip, ifaceIndex)
}

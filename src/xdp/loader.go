//go:build linux

package xdp

import (
	"fmt"
	"net"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/quangtrieu1312/tmasqued/logger"
)

type XDPMode int

const (
	// XDPModeNative means the XDP program runs inside the NIC driver,
	// before the kernel network stack. Requires driver support (e.g. virtio_net,
	// i40e, mlx5). Zero-copy is *possible* in this mode (driver-dependent),
	// but copy mode also works.
	XDPModeNative XDPMode = iota

	// XDPModeGeneric means the XDP program runs inside the kernel stack
	// after the packet is already received — no driver support needed.
	// AF_XDP sockets MUST use copy mode here; zero-copy is unavailable.
	XDPModeGeneric
)

func (m XDPMode) String() string {
	if m == XDPModeNative {
		return "native"
	}
	return "generic"
}

type Loader struct {
	objs  MasqueXDPObjects
	link  link.Link
	mode  XDPMode
	iface *net.Interface
}

// Iface is the NIC this loader's XDP program is attached to.
func (l *Loader) Iface() *net.Interface { return l.iface }

// natRevPinPath is where the reverse-NAT table is pinned. Shared by every NIC's
// XDP program (it is keyed by the SNAT (wan_ip, ...) 5-tuple, so flows SNAT'd out
// different NICs to different source IPs coexist in one table) and persists across
// restarts so in-flight return flows survive a reload.
const natRevPinPath = "/sys/fs/bpf/nat_rev_table"

// loadSharedNatTable creates-or-loads the single pinned nat_rev_table that every
// per-NIC program shares.
func loadSharedNatTable(spec *ebpf.CollectionSpec) (*ebpf.Map, error) {
	if m, err := ebpf.LoadPinnedMap(natRevPinPath, nil); err == nil {
		return m, nil
	}
	ms, ok := spec.Maps["nat_rev_table"]
	if !ok {
		return nil, fmt.Errorf("nat_rev_table missing from BPF spec")
	}
	m, err := ebpf.NewMapWithOptions(ms.Copy(), ebpf.MapOptions{PinPath: "/sys/fs/bpf"})
	if err != nil {
		return nil, fmt.Errorf("creating pinned nat_rev_table: %w", err)
	}
	return m, nil
}

// LoadMultiNIC attaches a FRESH copy of the XDP program to every named interface,
// each with its OWN xsks_quic / xsks_fwd XSKMAPs — so two NICs' identical
// rx_queue_index values never collide in a shared map — all sharing the single
// pinned nat_rev_table (injected via MapReplacements, not re-created per NIC).
// Returns one Loader per NIC, same order as ifaceNames. On any failure every
// already-attached loader is detached before returning.
func LoadMultiNIC(ifaceNames []string) ([]*Loader, error) {
	spec, err := LoadMasqueXDP()
	if err != nil {
		return nil, fmt.Errorf("loading XDP spec: %w", err)
	}
	natMap, err := loadSharedNatTable(spec)
	if err != nil {
		return nil, err
	}
	var loaders []*Loader
	for _, name := range ifaceNames {
		l, err := loadOne(spec, natMap, name)
		if err != nil {
			// Non-fatal: a NIC that won't attach (e.g. a secondary NIC the driver
			// can't take XDP on) must NOT take down the whole datapath. Log+skip;
			// the caller verifies its required (QUIC-listen) NICs are present.
			if logger.ShouldLog(logger.WARN) {
				logger.Warn(fmt.Sprintf("XDP attach skipped on %s: %v", name, err))
			}
			continue
		}
		loaders = append(loaders, l)
	}
	if len(loaders) == 0 {
		return nil, fmt.Errorf("XDP attach failed on every NIC %v", ifaceNames)
	}
	return loaders, nil
}

// loadOne loads a fresh program + xsk-maps for one NIC (sharing natMap) and
// attaches it, preferring native (driver) mode, falling back to generic (SKB).
func loadOne(spec *ebpf.CollectionSpec, natMap *ebpf.Map, ifaceName string) (*Loader, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("interface %q not found: %w", ifaceName, err)
	}
	objs := MasqueXDPObjects{}
	// Copy the spec per NIC so each load starts from a clean, unconsumed spec;
	// inject the shared nat_rev_table; xsks_quic/xsks_fwd load fresh per NIC.
	if err := spec.Copy().LoadAndAssign(&objs, &ebpf.CollectionOptions{
		MapReplacements: map[string]*ebpf.Map{"nat_rev_table": natMap},
	}); err != nil {
		return nil, fmt.Errorf("loading XDP objects: %w", err)
	}
	l, err := link.AttachXDP(link.XDPOptions{
		Program:   objs.MasqueXdpProg,
		Interface: iface.Index,
		Flags:     link.XDPDriverMode,
	})
	if err == nil {
		return &Loader{objs: objs, link: l, mode: XDPModeNative, iface: iface}, nil
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("XDP native mode failed on %s (falling back to generic): %v", ifaceName, err))
	}
	l, err = link.AttachXDP(link.XDPOptions{
		Program:   objs.MasqueXdpProg,
		Interface: iface.Index,
		Flags:     link.XDPGenericMode,
	})
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attaching XDP to %s (native and generic both failed): %w", ifaceName, err)
	}
	return &Loader{objs: objs, link: l, mode: XDPModeGeneric, iface: iface}, nil
}

// Load attaches the XDP program to a single ifaceName (back-compat wrapper).
func Load(ifaceName string) (*Loader, error) {
	ls, err := LoadMultiNIC([]string{ifaceName})
	if err != nil {
		return nil, err
	}
	return ls[0], nil
}

// Mode returns which XDP attach mode is actually in use.
// Pass this to NewConn so it can create AF_XDP sockets with compatible flags.
func (l *Loader) Mode() XDPMode { return l.mode }

// XskMap returns the XSKMAP for QUIC so AF_XDP sockets can register themselves.
// conn.go's quicCh
func (l *Loader) XskQuicMap() *ebpf.Map {
	return l.objs.XsksQuic
}

// XskMap returns the XSKMAP for NAT-return traffic so AF_XDP sockets can register themselves.
// conn.go's fwdCh
func (l *Loader) XskFwdMap() *ebpf.Map {
	return l.objs.XsksFwd
}

// Close detaches this NIC's XDP program and frees its per-NIC BPF resources
// (program + xsks_quic/xsks_fwd). It deliberately does NOT close NatRevTable —
// that map is SHARED across every NIC's loader and is pinned, so it outlives any
// single NIC and survives a reload. (objs.Close() would close it, breaking the
// other NICs, so we close the per-NIC handles individually.)
func (l *Loader) Close() {
	if l.link != nil {
		l.link.Close()
	}
	if l.objs.MasqueXdpProg != nil {
		l.objs.MasqueXdpProg.Close()
	}
	if l.objs.XsksQuic != nil {
		l.objs.XsksQuic.Close()
	}
	if l.objs.XsksFwd != nil {
		l.objs.XsksFwd.Close()
	}
}

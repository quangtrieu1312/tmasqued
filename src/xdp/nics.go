//go:build linux

package xdp

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// IsRealNIC reports whether the named interface is backed by a real device on a
// bus (PCI/virtio), as opposed to a purely virtual interface (veth, bridge,
// tun/tap, docker0, lo). It uses the bus-info reported by `ethtool -i <iface>`,
// which is the canonical signal: a physical/virtio NIC reports an actual bus
// address (e.g. "0000:00:03.0"), while virtual interfaces report an empty /
// "tap" / "N/A" bus-info. If ethtool is unavailable it falls back to the
// /sys/class/net/<iface>/device symlink — the same backing-device signal that
// ethtool's bus-info is read from.
func IsRealNIC(name string) bool {
	out, err := exec.Command("ethtool", "-i", name).Output()
	if err != nil {
		_, serr := os.Stat("/sys/class/net/" + name + "/device")
		return serr == nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "bus-info:") {
			bus := strings.TrimSpace(strings.TrimPrefix(line, "bus-info:"))
			return bus != "" && bus != "N/A" && bus != "tap"
		}
	}
	// No bus-info line at all → not a real NIC.
	return false
}

// DetectRealNICs returns the names of the physical NICs the AF_XDP/XDP datapath
// should attach to: every UP link with a real bus address, excluding loopback
// and OUR OWN tm* datapath devices (tm0 main tun, tmvhost0 vhost TAP, ...).
// Order follows the interface index (stable across runs).
func DetectRealNICs() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	var out []string
	for _, ifc := range ifaces {
		name := ifc.Name
		if name == "lo" || strings.HasPrefix(name, "tm") {
			continue
		}
		if ifc.Flags&net.FlagUp == 0 {
			continue // a DOWN NIC carries no traffic — nothing to attach
		}
		if IsRealNIC(name) {
			out = append(out, name)
		}
	}
	return out, nil
}

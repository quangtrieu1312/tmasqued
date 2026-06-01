//go:build linux

package xdp

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ChannelInfo holds NIC channel configuration as reported by `ethtool -l`.
// MaxCombined is the hardware ceiling (queues the NIC can support);
// CurrentCombined is the soft limit AF_XDP can currently bind against.
// On clouds and virtual NICs MaxCombined often defaults < hardware — set
// it with `ethtool -L <iface> combined N` to free additional TX rings.
type ChannelInfo struct {
	MaxCombined     int
	CurrentCombined int
}

// reCombined matches the "Combined: N" lines in `ethtool -l` output. The
// command emits two sections (pre-set max + current), each containing one
// Combined line, so we expect 2 matches when the NIC supports channels.
var reCombined = regexp.MustCompile(`(?m)^Combined:\s+(\d+)`)

// GetChannels probes hardware channel capacity via `ethtool -l <iface>`.
// Returns the pre-set max and current "Combined" counts. Channels are
// Linux's term for per-queue (RX/TX) hardware resources; combined=N gives
// the driver N matched RX+TX queue pairs to spread interrupts across.
//
// Some virtual NICs (loopback, certain veth setups, some hyperv variants)
// report "n/a" instead of numbers — those return a parse error and
// callers should fall back to a single bucket.
func GetChannels(iface string) (ChannelInfo, error) {
	out, err := exec.Command("ethtool", "-l", iface).Output()
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("ethtool -l %s: %w", iface, err)
	}
	return parseEthtoolChannels(string(out))
}

func parseEthtoolChannels(s string) (ChannelInfo, error) {
	matches := reCombined.FindAllStringSubmatch(s, -1)
	if len(matches) < 2 {
		return ChannelInfo{}, fmt.Errorf("ethtool channels output: expected 2 Combined lines, got %d (NIC may not support channels):\n%s",
			len(matches), strings.TrimSpace(s))
	}
	maxC, err := strconv.Atoi(matches[0][1])
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("parse max combined: %w", err)
	}
	curC, err := strconv.Atoi(matches[1][1])
	if err != nil {
		return ChannelInfo{}, fmt.Errorf("parse current combined: %w", err)
	}
	return ChannelInfo{MaxCombined: maxC, CurrentCombined: curC}, nil
}

// SetCombinedChannels runs `ethtool -L <iface> combined <n>`. The driver
// destroys and recreates the queue rings on this call, briefly dropping
// the link — must run BEFORE binding AF_XDP sockets. Requires
// CAP_NET_ADMIN (have it: the container runs with the right caps).
func SetCombinedChannels(iface string, n int) error {
	cmd := exec.Command("ethtool", "-L", iface, "combined", strconv.Itoa(n))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ethtool -L %s combined %d: %w (output: %s)",
			iface, n, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// MaximizeChannels probes the hardware max combined channel count and, if
// the current setting is below it, raises the soft limit to the hardware
// max. Returns the final active count — this is the number of TX queues
// AF_XDP can bind, and the bucket count the datagram TX path should use
// for per-flow socket pinning.
//
// Must be called ONCE at startup, BEFORE [[xdp.NewConn]] binds sockets.
// Returns (1, err) if probing fails (virtual NICs etc.) so the caller can
// still proceed with a single bucket — i.e. failure here is non-fatal.
func MaximizeChannels(iface string) (int, error) {
	info, err := GetChannels(iface)
	if err != nil {
		return 1, fmt.Errorf("probe channels for %s: %w", iface, err)
	}
	if info.MaxCombined <= 1 {
		// NIC only supports one channel (loopback, some vmnics) — nothing to do.
		return max(info.CurrentCombined, 1), nil
	}
	if info.CurrentCombined >= info.MaxCombined {
		// Already maxed.
		return info.CurrentCombined, nil
	}
	if err := SetCombinedChannels(iface, info.MaxCombined); err != nil {
		// Best effort: keep going with the current setting.
		return info.CurrentCombined, fmt.Errorf("set channels to max=%d (was %d): %w",
			info.MaxCombined, info.CurrentCombined, err)
	}
	return info.MaxCombined, nil
}

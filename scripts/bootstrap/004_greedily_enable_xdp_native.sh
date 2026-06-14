#!/usr/bin/env bash
# Greedily prepare EVERY real NIC for XDP-native: disable gro/lro and cap the MTU to
# the virtio_net XDP-native limit (3506). The datapath attaches XDP to all real NICs
# (the WAN it also listens on, plus any LAN NIC it bridges clients onto), and a NIC
# stuck in GENERIC XDP corrupts return traffic: virtio delivers intra-host frames with
# a PARTIAL (pseudo-header-only) checksum that the XDP reverse-NAT's incremental csum
# update mangles, so the client drops every reply (measured: TcpInCsumErrors). NATIVE
# XDP makes virtio hand full checksums to the program, which only works at MTU <= 3506.
# So this must run for every NIC the datapath will touch, not just WAN_INTERFACE.
#
# The original MTU + offload states are saved per-NIC (guarded: never overwritten on a
# restart, so the first-boot originals survive) for GracefullyShutDown to restore.
source /etc/tmasqued/tmasqued.conf

pageSize=$(getconf PAGESIZE)
# dmesg complains that virtio_net only takes MTU <= 3506 for XDP native.
virtioXDPLimit=3506

# is_real_nic: a NIC backed by a real bus device (PCI/virtio) reports a non-empty
# bus-info via ethtool -i; virtual interfaces (veth, bridges, tun/tap) report
# empty / "tap" / "N/A". Mirrors xdp.IsRealNIC in the Go datapath.
is_real_nic() {
    local bus
    bus=$(ethtool -i "$1" 2>/dev/null | awk -F': ' '/^bus-info:/{print $2}')
    [ -n "$bus" ] && [ "$bus" != "N/A" ] && [ "$bus" != "tap" ] && [ "$bus" != "tun" ]
}

pin_nic() {
    local nic=$1

    # Save ORIGINAL offload states (only the flags we change) before disabling them.
    local offloadsOrigFile=/etc/tmasqued/${nic}.offloads.orig
    if [ ! -f "$offloadsOrigFile" ]; then
        ethtool -k "$nic" | awk '
            /^generic-receive-offload:/ {print "gro " $2}
            /^large-receive-offload:/  {print "lro " $2}
        ' > "$offloadsOrigFile"
    fi
    ethtool -K "$nic" gro off lro off 2>/dev/null

    # The page-size / 3506 MTU cap is a virtio_net XDP-native limitation (virtio_net
    # rejects XDP-native attach above 3506). Only scale DOWN virtio_net NICs; other
    # drivers may run XDP-native at a larger MTU, so don't needlessly cripple them.
    # (gro/lro above are disabled for EVERY real NIC — that's for csum correctness.)
    local drv
    drv=$(ethtool -i "$nic" 2>/dev/null | awk -F': ' '/^driver:/{print $2}')
    [ "$drv" = "virtio_net" ] || return

    local nicMTU
    nicMTU=$(cat /sys/class/net/$nic/mtu 2>/dev/null)
    [ -n "$nicMTU" ] || return

    # Save the ORIGINAL (pre-scale) MTU only when actually scaling DOWN, so a restart
    # (link already at/below the cap) does not overwrite the true first-boot original.
    local origFile=/etc/tmasqued/${nic}.wan_mtu.orig
    if [ "$nicMTU" -gt "$virtioXDPLimit" ]; then
        echo "$nicMTU" > "$origFile"
    fi
    if [ "$nicMTU" -gt "$pageSize" ]; then
        ip link set "$nic" mtu "$pageSize"
    fi
    if [ "$nicMTU" -gt "$virtioXDPLimit" ]; then
        ip link set "$nic" mtu "$virtioXDPLimit"
    fi
}

for nic in $(ls /sys/class/net); do
    case "$nic" in lo|tm*) continue;; esac
    # Skip DOWN links (the datapath skips them too) and non-real (virtual) interfaces.
    operstate=$(cat /sys/class/net/$nic/operstate 2>/dev/null)
    if [ "$operstate" != "up" ] && ! ip link show "$nic" 2>/dev/null | grep -q "state UP"; then
        continue
    fi
    is_real_nic "$nic" || continue
    pin_nic "$nic"
done

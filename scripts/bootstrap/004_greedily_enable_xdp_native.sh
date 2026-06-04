#!/usr/bin/env bash
source /etc/tmasqued/tmasqued.conf
ethtool -K $WAN_INTERFACE gro off lro off
wanMTU=$(ip link | grep $WAN_INTERFACE | grep -oP '(?<=(mtu ))[0-9]+')
pageSize=$(getconf PAGESIZE)
# dmesg complains that virtio_net only takes MTU <= 3506 for XDP native
virtioXDPLimit=3506
# Save the ORIGINAL (pre-scale) WAN MTU so GracefullyShutDown can restore it on a
# graceful exit. Only save when we are actually about to scale DOWN — on a restart
# where the link is already at/below the cap we must NOT overwrite the saved value
# (which still holds the true original from the first scale-down).
origFile=/etc/tmasqued/${WAN_INTERFACE}.wan_mtu.orig
if [ "$wanMTU" -gt "$virtioXDPLimit" ]; then
    echo "$wanMTU" > "$origFile"
fi
if [ $wanMTU -gt $pageSize ]; then
    ip link set $WAN_INTERFACE mtu $pageSize
fi
if [ $wanMTU -gt $virtioXDPLimit ]; then
    ip link set $WAN_INTERFACE mtu $virtioXDPLimit
fi

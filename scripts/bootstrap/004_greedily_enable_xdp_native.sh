#!/usr/bin/env bash
source /etc/tmasqued/tmasqued.conf
ethtool -K $WAN_INTERFACE gro off lro off
wanMTU=$(ip link | grep $WAN_INTERFACE | grep -oP '(?<=(mtu ))[0-9]+')
pageSize=$(getconf PAGESIZE)
# dmesg complains that virtio_net only takes MTU <= 3506 for XDP native
virtioXDPLimit=3506
if [ $wanMTU -gt $pageSize ]; then
    ip link set $WAN_INTERFACE mtu $pageSize
fi
if [ $wanMTU -gt $virtioXDPLimit ]; then
    ip link set $WAN_INTERFACE mtu $virtioXDPLimit
fi

#!/bin/bash
# Run the TUN GSO PoC and check dmesg for "bad gso" / virtio gso warnings on gw + target.
cd "$(dirname "$0")"; source ./fleet-2core.sh
TGT=alpine@198.18.4.65

echo "[setup] forwarding on gw"
$SSH "$GW_SSH" 'doas sysctl -w net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0 net.ipv4.conf.all.accept_local=1 net.ipv4.conf.default.accept_local=1 >/dev/null'
$SSH "$GW_SSH" 'doas iptables -C FORWARD -o eth0 -j ACCEPT 2>/dev/null || doas iptables -I FORWARD -o eth0 -j ACCEPT'

echo "[mark] dmesg watermark before run"
$SSH "$GW_SSH" 'doas dmesg | tail -1 | tr -d "\n"; echo " <-- gw last line"'
$SSH "$TGT"    'doas dmesg | tail -1 | tr -d "\n"; echo " <-- target last line"'

echo "[run] persistent PoC 5s (src=local 198.18.0.113 -> target 198.18.4.65, 10-seg super-frames)"
$SSH "$GW_SSH" 'doas /tmp/tunpoc 198.18.0.113 198.18.4.65 10 5' 2>&1 | tail -1
sleep 1

echo "===== GW dmesg: gso/virtio/csum warnings (last 15) ====="
$SSH "$GW_SSH" 'doas dmesg | grep -iE "bad gso|gso|virtio|csum|truncat|malform" | tail -15 || echo none'
echo "===== TARGET dmesg: gso/virtio/csum warnings (last 15) ====="
$SSH "$TGT" 'doas dmesg | grep -iE "bad gso|gso|virtio|csum|truncat|malform" | tail -15 || echo none'

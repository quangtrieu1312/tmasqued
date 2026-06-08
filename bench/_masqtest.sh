#!/bin/bash
# Clean WG-model test: non-local src IN (rp_filter=0 so it passes input), MASQUERADE OUT.
# Does kernel-forward + MASQUERADE deliver to the target? (vs the pre-SNAT/accept_local path that didn't.)
cd "$(dirname "$0")"; source ./fleet-2core.sh
TGT=alpine@198.18.4.65

echo "[setup] rp_filter=0 (all+default so fresh tun0 inherits), ip_forward, MASQUERADE 10.99.99.0/24, FORWARD accept"
$SSH "$GW_SSH" 'doas sysctl -w net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0 >/dev/null'
$SSH "$GW_SSH" 'doas iptables -t nat -C POSTROUTING -s 10.99.99.0/24 -o eth0 -j MASQUERADE 2>/dev/null || doas iptables -t nat -A POSTROUTING -s 10.99.99.0/24 -o eth0 -j MASQUERADE'
$SSH "$GW_SSH" 'doas iptables -C FORWARD -s 10.99.99.0/24 -j ACCEPT 2>/dev/null || doas iptables -I FORWARD -s 10.99.99.0/24 -j ACCEPT'
$SSH "$GW_SSH" 'doas iptables -C FORWARD -o eth0 -j ACCEPT 2>/dev/null || doas iptables -I FORWARD -o eth0 -j ACCEPT'

echo "[capture] target eth0 for post-MASQUERADE src=198.18.0.113"
$SSH "$TGT" 'doas pkill tcpdump 2>/dev/null; sleep 1; doas sh -c "timeout 9 tcpdump -ni eth0 -c 60 \"src host 198.18.0.113 and tcp and dst port 5201\" >/tmp/mq.txt 2>&1 &"'
sleep 1
echo "[run] PoC: non-local src 10.99.99.99 -> target, 10-seg GSO super-frames, 5s"
$SSH "$GW_SSH" 'doas /tmp/tunpoc 10.99.99.99 198.18.4.65 10 5' 2>&1 | tail -1
sleep 2
echo "=== conntrack entries (did NAT state form?) ==="
$SSH "$GW_SSH" 'doas conntrack -L 2>/dev/null | grep -c 198.18.4.65 || echo "(no conntrack tool)"'
echo "=== TARGET received (KEY) ==="
$SSH "$TGT" 'echo total=$(grep -c length /tmp/mq.txt); grep -oE "length [0-9]+" /tmp/mq.txt | sort | uniq -c | sort -rn | head -5'

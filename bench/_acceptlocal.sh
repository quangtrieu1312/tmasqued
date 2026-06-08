#!/bin/bash
# Controlled test: hold rp_filter=0, toggle accept_local, src=LOCAL (gw WAN IP) -> remote dst.
# Measures whether src=local packets reach the FORWARD chain (pass input/martian validation).
cd "$(dirname "$0")"; source ./fleet-2core.sh

fwd_count() { # read the "-o eth0 ACCEPT" rule packet count
  $SSH "$GW_SSH" 'doas iptables -L FORWARD -vn' | awk '/ACCEPT/ && / eth0 /{print $1; exit}'
}

$SSH "$GW_SSH" 'doas sysctl -w net.ipv4.ip_forward=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0 >/dev/null'
$SSH "$GW_SSH" 'doas iptables -C FORWARD -o eth0 -j ACCEPT 2>/dev/null || doas iptables -I FORWARD -o eth0 -j ACCEPT'

for AL in 0 1; do
  echo "===== rp_filter=0, accept_local=$AL, src=LOCAL(198.18.0.113) ====="
  $SSH "$GW_SSH" "doas sysctl -w net.ipv4.conf.all.accept_local=$AL net.ipv4.conf.default.accept_local=$AL >/dev/null"
  $SSH "$GW_SSH" 'doas iptables -Z FORWARD >/dev/null'
  $SSH "$GW_SSH" 'doas /tmp/tunpoc 198.18.0.113 198.18.4.65 10 50 >/dev/null 2>&1'
  echo "  FORWARD ACCEPT(out eth0) packet count after 50 injects = $(fwd_count)"
done
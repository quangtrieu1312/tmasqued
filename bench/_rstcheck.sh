#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7
# capture all traffic between gw WAN IP and target during an upload
$SSH "$GW_SSH" 'doas pkill tcpdump 2>/dev/null; sleep 1; doas sh -c "timeout 9 tcpdump -nei eth0 -c 60 host 198.18.4.65 and tcp and port 5201 >/tmp/rst.txt 2>&1 &"'
sleep 1
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 5 -P1 2>/dev/null | awk "/sender/{print \"up:\",\$7,\$8,\$9}"'
sleep 2
echo "=== flags histogram (gw<->target) ==="
$SSH "$GW_SSH" 'grep -oE "Flags \[[^]]*\]" /tmp/rst.txt | sort | uniq -c | sort -rn'
echo "=== RST count (any direction) ==="
$SSH "$GW_SSH" 'grep -c "Flags \[R" /tmp/rst.txt'
echo "=== returns from target (198.18.4.65 > 198.18.0.113) count ==="
$SSH "$GW_SSH" 'grep -c "198.18.4.65.* > 198.18.0.113" /tmp/rst.txt'
echo "=== first 8 lines ==="
$SSH "$GW_SSH" 'grep -oE "198.18.[0-9.]+ > 198.18.[0-9.]+: Flags \[[^]]*\]" /tmp/rst.txt | head -8'

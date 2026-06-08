#!/bin/bash
# Capture coalesced upload data segments at the target and count checksum-correct vs incorrect.
cd "$(dirname "$0")"; source ./fleet-2core.sh
TGT=alpine@198.18.4.65; C0=alpine@198.18.5.7
# clean-exit capture: stop after 120 frames (-c), verbose for cksum (-vv)
$SSH "$TGT" 'doas pkill tcpdump 2>/dev/null; doas sh -c "timeout 12 tcpdump -ni eth0 -vv -c 120 \"src host 198.18.0.113 and tcp and port 5201 and greater 200\" >/tmp/c2.txt 2>&1 &"'
sleep 1
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 8 -P1 2>/dev/null | awk "/sender/"'
sleep 2
echo "=== data segments captured ==="
echo "lines_with_cksum=$($SSH $TGT 'grep -c cksum /tmp/c2.txt')"
echo "cksum_correct=$($SSH $TGT 'grep -c "cksum.*correct" /tmp/c2.txt')"
echo "cksum_incorrect=$($SSH $TGT 'grep -c incorrect /tmp/c2.txt')"
echo "--- sample of incorrect (if any) ---"
$SSH "$TGT" 'grep incorrect /tmp/c2.txt | head -3'
echo "--- first 6 data-seg lengths+seq ---"
$SSH "$TGT" 'grep -oE "length [0-9]{3,}" /tmp/c2.txt | head -6'

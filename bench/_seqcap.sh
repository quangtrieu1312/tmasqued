#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
TGT=alpine@198.18.4.65; C0=alpine@198.18.5.7
$SSH "$TGT" 'doas pkill tcpdump 2>/dev/null; sleep 1; doas sh -c "timeout 10 tcpdump -ni eth0 -c 200 \"src host 198.18.0.113 and tcp and port 5201 and greater 100\" >/tmp/seq.txt 2>&1 &"'
sleep 1
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 7 -P1 2>/dev/null | awk "/sender/"'
sleep 2
echo "=== data-seg count + length histogram ==="
$SSH "$TGT" 'grep -oE "length [0-9]+$" /tmp/seq.txt | sort | uniq -c | sort -rn | head'
echo "=== arrival-order seq starts (first 40) ==="
$SSH "$TGT" 'grep -oE "seq [0-9]+:[0-9]+" /tmp/seq.txt | head -40 | sed "s/seq //"'

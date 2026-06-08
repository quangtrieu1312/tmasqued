#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
TGT=alpine@198.18.4.65
echo "total_pkts=$($SSH $TGT 'doas tcpdump -nr /tmp/u.pcap 2>/dev/null | wc -l')"
echo "bad_cksum=$($SSH $TGT 'doas tcpdump -nr /tmp/u.pcap -vv 2>/dev/null | grep -c incorrect')"
echo "data_segs_1364=$($SSH $TGT 'doas tcpdump -nr /tmp/u.pcap 2>/dev/null | grep -c "length 1364"')"
echo "zero_len=$($SSH $TGT 'doas tcpdump -nr /tmp/u.pcap 2>/dev/null | grep -c "length 0"')"
echo "--- length histogram ---"
$SSH $TGT 'doas tcpdump -nr /tmp/u.pcap 2>/dev/null | grep -oE "length [0-9]+$" | sort | uniq -c | sort -rn | head -10'
echo "--- first 10 data-seg sequence starts (arrival order) ---"
$SSH $TGT 'doas tcpdump -nr /tmp/u.pcap 2>/dev/null | grep "length 136" | head -10 | grep -oE "seq [0-9]+"'

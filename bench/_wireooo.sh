#!/bin/bash
# Isolate transport (client virtio TX + wire + gw kernel RX) from quic-go send logic.
# Plain UDP flood client->gw underlay; iperf3 reports out-of-order datagrams.
#  OOO high  -> transport reorders UDP (client virtio TX / wire / gw RX)
#  OOO ~0    -> transport clean -> the QUIC 8% reorder is in the client's quic-go send
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7
GWIP=198.18.0.113
echo "=== iperf3 UDP flood client -> gw underlay ($GWIP), jumbo-ish 3400B, ~850M ==="
$SSH "$GW_SSH" 'pkill -f "iperf3 -s -p 5202" 2>/dev/null; sleep 1; (iperf3 -s -p 5202 -1 >/tmp/uooo.txt 2>&1 &)'
sleep 1
$SSH "$C0" "iperf3 -u -c $GWIP -p 5202 -b 850M -l 3400 -t 6 2>/dev/null | tail -4"
sleep 1
echo "=== gw receiver report (look for OOO / Lost/Total) ==="
$SSH "$GW_SSH" 'grep -iE "datagrams|receiver|OOO|out-of-order" /tmp/uooo.txt | tail -4'

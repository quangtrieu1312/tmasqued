#!/bin/bash
# Measure the CLIENT's upload-send inner-TCP order via its STATISTIC log (dg-packer =
# the client packing its upload datagrams to the wire). If ~0% genuine, the client
# sends in order -> the 8% reorder enters at the gw RX (AF_XDP/virtio); if high, the
# client reorders on send.
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 6 -P1 >/dev/null 2>&1 &'
sleep 4
echo "=== client log: STATISTIC line (dg-packer = upload send order; pre-* = dl observers) ==="
$SSH "$C0" 'grep -i STATISTIC /tmp/tmq.log 2>/dev/null | tail -1'
echo "=== just the dg-packer + pre-send fields ==="
$SSH "$C0" 'grep -i STATISTIC /tmp/tmq.log 2>/dev/null | tail -1 | grep -oE "dg-packer: [^|]*|pre-send: [^|]*|pre-reseq: [^|]*"'
echo "=== log length ==="
$SSH "$C0" 'wc -l /tmp/tmq.log 2>/dev/null'

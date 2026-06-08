#!/bin/bash
set -u
cd "$(dirname "$0")"; source ./fleet.sh
echo "=== cn1 tunnel sanity: ping target through tun0 ==="
timeout 12 $SSH ubuntu@198.18.0.122 'ping -c3 -W2 198.18.5.80 | tail -3'
echo "=== cn1 solo TCP (control) ==="
timeout 20 $SSH ubuntu@198.18.0.122 'iperf3 -c 198.18.5.80 -p 5201 -t6 -O2 -P8 -f m 2>&1 | grep -E "SUM|error|refused" | tail -2'
echo "=== cn1 solo UDP -b1G -P2 (the failing pattern) ==="
timeout 20 $SSH ubuntu@198.18.0.122 'iperf3 -c 198.18.5.80 -p 5201 -t6 -O2 -u -b1G -P2 -f m 2>&1 | grep -E "SUM|error|refused|connect" | tail -3'
echo "=== cn1 solo UDP gentler -b300M -P2 ==="
timeout 20 $SSH ubuntu@198.18.0.122 'iperf3 -c 198.18.5.80 -p 5201 -t6 -O2 -u -b300M -P2 -f m 2>&1 | grep -E "SUM|error" | tail -2'
echo "=== is target iperf3 -s :5201 alive? ==="
timeout 8 $SSH "$TARGET_SSH" 'pgrep -af "iperf3 -s -p 5201" | head -1 || echo "NO server on 5201"'

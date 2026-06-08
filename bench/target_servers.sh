#!/bin/bash
# Launch SELF-HEALING iperf3 servers on EVERY target: each port in a respawn loop so a crash under
# the UDP -R flood restarts instantly. Loops over TARGETS_SSH (1 target on 8-core, 2 on 2-core).
set -u
cd "$(dirname "$0")"; source "${FLEET:-./fleet-8core.sh}"

for t in "${TARGETS_SSH[@]}"; do
  timeout 25 $SSH "$t" 'bash -s' <<'REMOTE'
pkill -9 -f "iperf3 -s" 2>/dev/null
pkill -9 -f "bench-loop" 2>/dev/null
sleep 1
for p in 5201 5202 5203 5204 5205 5206; do
  setsid sh -c "while true; do iperf3 -s -p $p >/dev/null 2>&1; sleep 0.2; done #bench-loop-$p" </dev/null >/dev/null 2>&1 &
done
sleep 2
echo "loop wrappers: $(pgrep -fc bench-loop)   iperf3 servers: $(pgrep -c iperf3)"
REMOTE
done

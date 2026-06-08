#!/bin/bash
# Prove the saturation hypothesis the way the user proposed: ping the server THROUGH the tunnel
# (a) baseline, (b) during a sustained UDP flood, (c) after the flood stops. If ping dies under
# load and recovers after, the tunnel is saturated (and recovers) — not permanently broken.
# cn1 only pings (does NOT flood, so its own mesh/SSH stays alive); 5 Alpines do the flooding.
set -u
cd "$(dirname "$0")"; source ./fleet.sh
CN1=ubuntu@198.18.0.122
GWTUN=100.64.0.1   # gwself: the tmasque SERVER's tunnel-side IP (pure tunnel, no forward)
ALP=("alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" "alpine@198.18.2.75 5204" \
     "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206")

pings(){ # $1 dst $2 label
  local r=$(timeout 14 $SSH $CN1 "ping -c6 -i0.3 -W2 $1 2>/dev/null | grep -E 'packet loss|rtt'")
  echo "    $2 ($1): ${r:-NO REPLY AT ALL}"
}
probe(){ echo "  server tunnel:"; pings $GWTUN "GW-tun"; echo "  target via forward:"; pings $TARGET "target"; }

echo "=== route check ==="; timeout 8 $SSH $CN1 "ip route get $GWTUN | head -1; ip route get $TARGET | head -1"
echo "=== (a) BASELINE — no flood ==="; probe
echo "=== (b) start 5-Alpine UDP flood (-b1G x2, saturate the single-queue tunnel) ==="
for cp in "${ALP[@]}"; do set -- $cp; timeout 30 $SSH "$1" "iperf3 -c $TARGET -p $2 -t22 -u -b1G -P2 >/dev/null 2>&1" & done
sleep 5
echo "    --- ping DURING saturation ---"; probe
wait
echo "=== (c) flood stopped — wait 4s, ping again (recovery?) ==="; sleep 4; probe

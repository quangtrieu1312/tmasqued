#!/bin/bash
# Profile the UPLOAD bound (2-core, jumbo, single client 8-stream 30s): is it client-CPU
# (QUIC crypto/GSO on 2 cores) or reorder at the target (server forward-TX reordering)?
# Captures: rate, CLIENT cpu, GW cpu, client inner-TCP retransmits (sender), target OOO-queue (receiver).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
set -- ${CLIENTS[0]}; C0="$1"; C0_IP="$2"; TGT_IP="$8"; PORT="$9"
DUR=30

$SSH "$TGT_IP" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null | grep -q \":\$p \" || iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1; sleep 1

cpu_busy(){ # ssh-dest  -> "sum_of_busy_core_equivs n_cores" over $DUR
  $SSH "$1" "LC_ALL=C mpstat -P ALL 1 $DUR 2>/dev/null" | awk '
    /^Average:/ && $2 ~ /^[0-9]+$/ { n++; sum+=(100-$NF)/100 } END{ printf "%.2f %d", sum, n }'
}

# prime nstat baselines (resets per-user delta file)
$SSH "$C0"     "nstat -n >/dev/null 2>&1" ; $SSH "$TGT_IP" "nstat -n >/dev/null 2>&1"

echo "### UPLOAD profile: single-client 8-stream ${DUR}s jumbo"
# start CPU samplers (background) + the flow
cpu_busy "$C0"     > /tmp/up_ccpu  &  CC=$!
cpu_busy "$GW_SSH" > /tmp/up_gcpu  &  GC=$!
$SSH "$C0" "iperf3 -c $TGT_IP -p $PORT -t $DUR -O2 -P8 -f m 2>/dev/null | grep -E 'SUM.*receiver'" > /tmp/up_rate &  IPF=$!
wait $IPF; RATE=$(cat /tmp/up_rate)
wait $CC; wait $GC
CCPU=$(cat /tmp/up_ccpu); GCPU=$(cat /tmp/up_gcpu)

# nstat deltas since prime
CLIENT_TCP=$($SSH "$C0"     "nstat 2>/dev/null | grep -iE 'TcpRetransSegs|TcpOutSegs|TcpExtTCPLostRetransmit|TcpExtTCPSpuriousRTOs'")
TARGET_TCP=$($SSH "$TGT_IP" "nstat 2>/dev/null | grep -iE 'TcpExtTCPOFOQueue|TcpExtTCPOFODrop|TcpInSegs|TcpExtTCPSACKReorder|TcpExtTCPRenoReorder|TcpExtTCPTSReorder'")

echo "  RATE      : $RATE"
echo "  CLIENT cpu: $CCPU  (busy core-equiv of n cores)"
echo "  GW     cpu: $GCPU"
echo "  --- CLIENT (upload sender) inner-TCP ---"; printf '%s\n' "$CLIENT_TCP" | sed 's/^/    /'
echo "  --- TARGET (upload receiver) reorder/OOO ---"; printf '%s\n' "$TARGET_TCP" | sed 's/^/    /'
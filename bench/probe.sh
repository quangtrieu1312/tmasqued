#!/bin/bash
# Reusable single-client reorder/throughput probe (2-core, jumbo). Reuse after each trial.
# Usage: P=1 DUR=15 bash probe.sh "label"   (P = iperf parallel streams; P=1 = clean per-flow)
# Prints per-direction: rate(Mbit) | sender-retransmit% | receiver OFO-queue.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
set -- ${CLIENTS[0]}; C0="$1"; C0SU="$5"; TGT="alpine@$8"; TGT_IP="$8"; PORT="$9"
DUR=${DUR:-15}; P=${P:-1}; LABEL="${1:-probe}"

# ensure client0 tmasque is up + target routes via tunX (restart if not)
if ! $SSH "$C0" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" 2>/dev/null; then
  $SSH "$C0" "doas sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; for t in \$(ip -br link show|grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  for i in $(seq 1 12); do $SSH "$C0" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" 2>/dev/null && break; sleep 3; done
fi
$SSH "$TGT" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1

echo "=== $LABEL  (P=$P, ${DUR}s) ==="
for dir in up down; do
  flag=""; [ "$dir" = down ] && flag="-R"
  $SSH "$C0" "nstat -n >/dev/null 2>&1"; $SSH "$TGT" "nstat -n >/dev/null 2>&1"
  out=$($SSH "$C0" "iperf3 -c $TGT_IP -p $PORT -t $DUR -O2 -P$P $flag -f m 2>/dev/null")
  rate=$(printf '%s' "$out" | awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)} END{print r}')
  # sender = client on up, target on down
  if [ "$dir" = up ]; then SND="$C0"; RCV="$TGT"; else SND="$TGT"; RCV="$C0"; fi
  snd=$($SSH "$SND" "nstat 2>/dev/null|awk '/TcpRetransSegs/{rt=\$2}/TcpOutSegs/{ot=\$2}END{if(ot>0)printf \"%.1f%% (%d/%d)\",100*rt/ot,rt,ot;else print \"-\"}'")
  ofo=$($SSH "$RCV" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.0f%% (%d/%d)\",100*o/i,o,i;else print o}'")
  printf "  %-4s rate=%-6s Mbit | sender-retr=%-18s | recv-OFO=%s\n" "$dir" "${rate:-NA}" "$snd" "$ofo"
done
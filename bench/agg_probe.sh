#!/bin/bash
# 2-client AGGREGATE upload to the shared target (ports 5201/5202), P streams each.
# Reports each client's receiver Gbit + the sum (gateway aggregate forward), + target OFO + gw CPU.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
P=${P:-8}; DUR=${DUR:-15}; LABEL="${1:-agg}"
C0=$(set -- ${CLIENTS[0]}; echo $1); C1=$(set -- ${CLIENTS[1]}; echo $1)
TGT=alpine@198.18.4.65; TGT_IP=198.18.4.65
ensure(){ $SSH "$1" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" || { $SSH "$1" "doas sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1; for i in $(seq 1 10); do $SSH "$1" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && break; sleep 3; done; }; }
ensure "$C0"; ensure "$C1"
$SSH "$TGT" "for p in 5201 5202; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
rate(){ printf '%s' "$1" | awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}'; }
$SSH "$TGT" "nstat -n >/dev/null 2>&1"
$SSH "$GW_SSH" "LC_ALL=C mpstat -P ALL 1 $DUR 2>/dev/null" >/tmp/agg_gw &
GP=$!
$SSH "$C0" "iperf3 -c $TGT_IP -p 5201 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/agg0 &
$SSH "$C1" "iperf3 -c $TGT_IP -p 5202 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/agg1 &
wait
r0=$(rate "$(cat /tmp/agg0)"); r1=$(rate "$(cat /tmp/agg1)")
ofo=$($SSH "$TGT" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.0f%%\",100*o/i;else print 0}'")
gw=$(awk '/^Average:/ && $2 ~ /^[0-9]+$/ {n++; s+=(100-$NF)/100} END{printf "%.2f/%d", s, n}' /tmp/agg_gw)
printf "### %s (P=%s/client): c0=%s + c1=%s = AGG %.0f Mbit (%.2f G) | target-OFO=%s | gw=%s cores\n" \
  "$LABEL" "$P" "$r0" "$r1" "$(awk "BEGIN{print $r0+$r1}")" "$(awk "BEGIN{print ($r0+$r1)/1000}")" "$ofo" "$gw"
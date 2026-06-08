#!/bin/bash
# CONTROL: does the client->target leg reorder NON-tunnel (direct) traffic? If direct also shows
# high target-OFO, the reorder is the underlay/target-virtio, not tmasque's datapath.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
set -- ${CLIENTS[0]}; C0="$1"; TGT="alpine@$8"; TGT_IP="$8"; PORT="$9"
$SSH "$TGT" "for p in 5201 5202; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
echo "### DIRECT control: client -> target over fabric (no tunnel)"
$SSH "$C0" "doas ip route replace $TGT_IP dev eth0; ip route get $TGT_IP | head -1"
for P in 1 8; do
  $SSH "$C0" "nstat -n >/dev/null 2>&1"; $SSH "$TGT" "nstat -n >/dev/null 2>&1"
  out=$($SSH "$C0" "iperf3 -c $TGT_IP -p $PORT -t 12 -O2 -P$P -f m 2>/dev/null")
  rate=$(printf '%s' "$out" | awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r}')
  ofo=$($SSH "$TGT" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.0f%% (%d/%d)\",100*o/i,o,i;else print o}'")
  printf "  direct up P=%-2s rate=%-6s Mbit | target recv-OFO=%s\n" "$P" "${rate:-NA}" "$ofo"
done
echo "### restore client tun route (restart tmasque client0)"
$SSH "$C0" "doas sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; for t in \$(ip -br link show|grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
for i in $(seq 1 12); do $SSH "$C0" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" 2>/dev/null && { echo "  client0 tun route restored"; break; }; sleep 3; done
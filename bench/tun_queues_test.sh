#!/bin/bash
# Test whether client TUN_QUEUES (default NumCPU=2) is the UPLOAD-reorder source.
# TUN_QUEUES is read from the CONFIG FILE (/etc/tmasque/tmasque.conf), not env — so set it there.
# Compares =2 (default) vs =1 (strict per-flow order), single-client up/down, with retransmit + OFO.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
set -- ${CLIENTS[0]}; C0="$1"; TGT="alpine@$8"; TGT_IP="$8"; PORT="$9"; DUR=20
CONF=/etc/tmasque/tmasque.conf

set_conf(){ $SSH "$C0" "doas sh -c 'sed -i /^TUN_QUEUES=/d $CONF; echo TUN_QUEUES=$1 >> $CONF'"; }
clear_conf(){ $SSH "$C0" "doas sh -c 'sed -i /^TUN_QUEUES=/d $CONF'"; }
kill_c0(){ $SSH "$C0" 'doas sh -c '\''for d in /proc/[0-9]*; do [ "$(readlink $d/exe 2>/dev/null)" = /usr/local/bin/tmasque ] && kill -9 ${d#/proc/} 2>/dev/null; done; sleep 1; for t in $(ip -br link show|grep -oE "^tun[0-9]+"); do ip link del $t 2>/dev/null; done'\''' >/dev/null 2>&1; }
start_c0(){ $SSH "$C0" "doas sh -c 'modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1; }
ready_c0(){ local i; for i in $(seq 1 12); do $SSH "$C0" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 3; done; }
verify(){ $SSH "$C0" 'doas sh -c '\''for d in /proc/[0-9]*; do [ "$(readlink $d/exe 2>/dev/null)" = /usr/local/bin/tmasque ] || continue; echo -n "tun-fds="; ls -l $d/fd 2>/dev/null|grep -c /dev/net/tun; break; done'\'''; }

$SSH "$TGT" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1

for q in 2 1; do
  echo "########## client TUN_QUEUES=$q (config file) ##########"
  set_conf "$q"; kill_c0; sleep 3; start_c0; ready_c0
  echo "  verify: TUN_QUEUES=$q -> $(verify|tr '\n' ' ')(expect tun-fds=$q)"
  for dir in up down; do
    flag=""; [ "$dir" = down ] && flag="-R"
    $SSH "$C0" "nstat -n >/dev/null 2>&1"; $SSH "$TGT" "nstat -n >/dev/null 2>&1"
    rate=$($SSH "$C0" "iperf3 -c $TGT_IP -p $PORT -t $DUR -O2 -P8 $flag -f m 2>/dev/null|grep 'SUM.*receiver'")
    cl=$($SSH "$C0" "nstat 2>/dev/null|grep -iE 'TcpRetransSegs|TcpOutSegs|TCPOFOQueue'|tr '\n' ' '")
    tg=$($SSH "$TGT" "nstat 2>/dev/null|grep -iE 'TCPOFOQueue|TcpInSegs'|tr '\n' ' '")
    echo "  $dir: ${rate}"
    echo "     client: $cl"
    echo "     target: $tg"
  done
done
clear_conf; kill_c0; sleep 2; start_c0; ready_c0; echo "### restored client0 config (TUN_QUEUES line removed -> default)"
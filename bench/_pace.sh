#!/bin/bash
# Sweep eth0 egress pacing (fq) on the FORWARD_KERNEL_TX path; report rate/retr/OFO together.
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7; TGT=alpine@198.18.4.65
run() { # $1=label  $2=tc-qdisc-args
  $SSH "$GW_SSH" "doas tc qdisc replace dev eth0 root $2 2>/dev/null"
  $SSH "$TGT" "nstat -z >/dev/null 2>&1"
  out=$($SSH "$C0" "iperf3 -c 198.18.4.65 -p 5201 -t 8 -O 2 -P1 2>/dev/null | awk '/sender/{print \$7\" \"\$8\" retr=\"\$9}'")
  ofo=$($SSH "$TGT" "nstat 2>/dev/null | awk '/TcpExtTCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.0f%%\",100*o/i}'")
  printf "%-22s up=%-26s OFO=%s\n" "$1" "$out" "$ofo"
}
run "pfifo_fast(no pace)" "pfifo_fast"
run "fq (flow-paced)"     "fq"
run "fq maxrate 2gbit"    "fq maxrate 2gbit"
run "fq maxrate 1500mbit" "fq maxrate 1500mbit"
run "fq maxrate 1200mbit" "fq maxrate 1200mbit"
run "fq maxrate 1000mbit" "fq maxrate 1000mbit"
$SSH "$GW_SSH" "doas tc qdisc replace dev eth0 root pfifo_fast 2>/dev/null"

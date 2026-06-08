#!/bin/bash
# On the 8-core gw (FORWARD_TUN_NAPI), sweep reseq window/maxage; report GRO-input order + target OFO/throughput.
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh
C0=ubuntu@198.18.0.122; T2=ubuntu@198.18.5.80
setenv() { # $1=window $2=maxage_us
  $SSH "$GW_SSH" "cd ~/tmasqued
    sed -i '/FORWARD_UPLOAD_RESEQ_WINDOW/d; /FORWARD_UPLOAD_RESEQ_MAXAGE_US/d' docker-compose.yml
    sed -i \"s/      - FORWARD_UPLOAD_RESEQ=1/      - FORWARD_UPLOAD_RESEQ=1\n      - FORWARD_UPLOAD_RESEQ_WINDOW=$1\n      - FORWARD_UPLOAD_RESEQ_MAXAGE_US=$2/\" docker-compose.yml
    sudo docker compose up -d >/dev/null 2>&1"
  sleep 8
}
rd() { $SSH "$GW_SSH" "wget -qO- http://127.0.0.1:6060/debug/vars 2>/dev/null | tr , '\n' | grep -E 'upload_postreseq_(ooo|total)' | grep -oE '[0-9]+' | tr '\n' ' '"; }
run() { # $1=label $2=window $3=maxage_us
  setenv "$2" "$3"
  read bo bt <<< "$(rd)"; $SSH "$T2" "nstat -z >/dev/null 2>&1"
  up=$($SSH "$C0" "iperf3 -c 198.18.5.80 -p 5201 -t 8 -O 2 -P1 2>/dev/null | awk '/sender/{print \$7\" \"\$8\" r=\"\$9}'")
  read ao at <<< "$(rd)"
  ofo=$($SSH "$T2" "nstat 2>/dev/null | awk '/TcpExtTCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.1f%%\",100*o/i}'")
  pr=$(awk "BEGIN{d=$at-$bt; if(d>0)printf \"%.1f%%\",100*($ao-$bo)/d; else print \"-\"}")
  printf "%-26s up=%-22s recvOFO=%-7s GRO-input-ooo=%s\n" "$1" "$up" "$ofo" "$pr"
}
run "win=64 age=5ms (base)"    64   5000
run "win=512 age=20ms"         512  20000
run "win=2048 age=50ms"        2048 50000
echo "(WG=7.3% / AF_PACKET=42.6% / TUN_NAPI-base=21.6%)"

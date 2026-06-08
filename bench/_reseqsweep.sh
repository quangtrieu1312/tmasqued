#!/bin/bash
# Sweep upload-reseq window/maxAge. Usage: _reseqsweep.sh <window> <maxage_us>
cd "$(dirname "$0")"; source ./fleet-2core.sh
W="$1"; M="$2"; C0=alpine@198.18.5.7
echo "===== reseq window=$W maxAge=${M}us ====="
$SSH "$GW_SSH" "cd ~/tmasqued
  sed -i '/^FORWARD_TUN_GSO=/d;/^FORWARD_TUN_VHOST=/d' tmasqued.conf; printf 'FORWARD_TUN_GSO=true\nFORWARD_TUN_VHOST=false\n' >> tmasqued.conf
  sed -i 's/, FORWARD_UPLOAD_RESEQ_WINDOW=[0-9]*//; s/, FORWARD_UPLOAD_RESEQ_MAXAGE_US=[0-9]*//' docker-compose.yml
  sed -i \"s/FORWARD_UPLOAD_RESEQ=1/FORWARD_UPLOAD_RESEQ=1, FORWARD_UPLOAD_RESEQ_WINDOW=$W, FORWARD_UPLOAD_RESEQ_MAXAGE_US=$M/\" docker-compose.yml
  ${GW_SUDO:-doas} docker compose down >/dev/null 2>&1
  ${GW_SUDO:-doas} ethtool -L $GW_IF combined ${GW_RSS_COMBINED:-2} 2>/dev/null
  ${GW_SUDO:-doas} docker compose up -d >/dev/null 2>&1
  sleep 9
  ${GW_SUDO:-doas} sh -c 'iptables -C FORWARD -i tun+ -j ACCEPT 2>/dev/null || iptables -I FORWARD -i tun+ -j ACCEPT; iptables -C FORWARD -o eth0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -o eth0 -j ACCEPT'
  C=\$(${GW_SUDO:-doas} docker ps -q|head -1); D=\"${GW_SUDO:-doas} docker exec \$C tmasquectl\"; for r in cn1 cn2 cn3 cn4; do \$D role assign \$r ttgt >/dev/null 2>&1; done"
# probe (probe.sh ensures client0 tunneled)
P=1 DUR=10 bash probe.sh "reseq-w${W}-m${M}" 2>&1 | grep -E "up|down"
# ss -ti mid-flow
$SSH "$C0" "iperf3 -c 198.18.4.65 -p 5201 -t 6 -P1 >/dev/null 2>&1 &"
sleep 3
$SSH "$C0" "ss -ti dst 198.18.4.65 2>/dev/null | grep -A1 5201 | grep -oE 'cwnd:[0-9]+|rtt:[0-9.]+/[0-9.]+|retrans:[0-9/]+|rto:[0-9]+|lost:[0-9]+|delivery_rate [0-9A-Za-z]+' | tr '\n' ' '; echo"
sleep 4

#!/bin/bash
# Sweep FORWARD_TUN_GSO_MAXSEGS (host-TSO super-frame seg cap). Usage: _segsweep.sh <segs>
cd "$(dirname "$0")"; source ./fleet-2core.sh
S="$1"; C0=alpine@198.18.5.7
$SSH "$GW_SSH" "cd ~/tmasqued
  sed -i '/^FORWARD_TUN_GSO=/d;/^FORWARD_TUN_VHOST=/d' tmasqued.conf; printf 'FORWARD_TUN_GSO=true\nFORWARD_TUN_VHOST=false\n' >> tmasqued.conf
  sed -i 's/, FORWARD_TUN_GSO_MAXSEGS=[0-9]*//' docker-compose.yml
  sed -i \"s/FORWARD_UPLOAD_RESEQ=1/FORWARD_UPLOAD_RESEQ=1, FORWARD_TUN_GSO_MAXSEGS=$S/\" docker-compose.yml
  ${GW_SUDO:-doas} docker compose down >/dev/null 2>&1
  ${GW_SUDO:-doas} ethtool -L eth0 combined 2 2>/dev/null
  ${GW_SUDO:-doas} docker compose up -d >/dev/null 2>&1
  sleep 9
  ${GW_SUDO:-doas} sh -c 'iptables -C FORWARD -i tun+ -j ACCEPT 2>/dev/null || iptables -I FORWARD -i tun+ -j ACCEPT; iptables -C FORWARD -o eth0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -o eth0 -j ACCEPT'
  C=\$(${GW_SUDO:-doas} docker ps -q|head -1); for r in cn1 cn2 cn3 cn4; do ${GW_SUDO:-doas} docker exec \$C tmasquectl role assign \$r ttgt >/dev/null 2>&1; done"
rd() { $SSH "$GW_SSH" "wget -qO- http://127.0.0.1:6060/debug/vars 2>/dev/null | tr , \"\n\" | grep -E \"dg_rcvin_(genuine|total)\" | grep -oE \"[0-9]+\" | tr \"\n\" \" \""; }
# warm tunnel via probe, then measure
P=1 DUR=4 bash probe.sh "warm-seg$S" >/dev/null 2>&1
read bg bt <<< "$(rd)"
up=$($SSH "$C0" "iperf3 -c 198.18.4.65 -p 5201 -t 8 -P1 2>/dev/null | awk '/sender/{print \$7,\$8}'")
read ag at <<< "$(rd)"
dg=$((ag-bg)); dt=$((at-bt)); pct=$(awk "BEGIN{if($dt>0)printf \"%.1f\",100*$dg/$dt; else print 0}")
cpu=$($SSH "$GW_SSH" "top -bn1 2>/dev/null | grep -iE '^CPU' | head -1")
echo "MAXSEGS=$S  up=$up  | dg_rcvin=${pct}%  | $cpu"

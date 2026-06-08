#!/bin/bash
# 5 implementations x 5 rows @ rss(combined=8). Rows: 1up-P1, 1up-P8, half, allup, alldown.
# Impls: wgk (kernel WG), wgu (userspace wireguard-go), tmq-xdp, tmq-vhost, tmq-gso.
# Per-phase SMOKE gate (skip measure if the stack didn't come up). Restores gso/rss at end.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
TGT=198.18.5.80; DUR="${DUR:-12}"; PORT=51820; WGNET=10.9.0; GWWG=$WGNET.1
OUT=/tmp/regimeall.out
SSHO="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=8"
log(){ echo "$(date +%H:%M:%S) $*" | tee -a "$OUT"; }
recv(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }
sretr(){ awk '/sender/{r=$(NF-1)}END{print r+0}' "$1"; }
DESTS=(); SUS=(); for c in "${CLIENTS[@]}"; do set -- $c; DESTS+=("$1"); SUS+=("$5"); done
PORTS=(5201 5202 5203 5204 5205 5206)

ensure_srv(){ $SSHO "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1; }
reset_srv(){ $SSHO "$TARGET_SSH" "pkill -9 iperf3 2>/dev/null; sleep 2; for p in 5201 5202 5203 5204 5205 5206; do iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1; sleep 1; }
run_set(){ local P=$1; shift; local specs=("$@") s
  $SSHO "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/ra_gw &
  for s in "${specs[@]}"; do set -- $s; local idx=$1 dir=$2 R=""; [ "$dir" = d ] && R="-R"
    $SSHO "${DESTS[$idx]}" "iperf3 -c $TGT -p ${PORTS[$idx]} -t $DUR -O2 -P$P $R -f m 2>/dev/null" >/tmp/ra_$idx & done
  wait; local tot=0 rt=0
  for s in "${specs[@]}"; do set -- $s; tot=$(awk "BEGIN{print $tot+$(recv /tmp/ra_$1)}"); rt=$((rt+$(sretr /tmp/ra_$1))); done
  GW=$(awk '/^Average:/&&$2=="all"{printf "%.1f",8*(100-$NF)/100}' /tmp/ra_gw); AGG=$(awk "BEGIN{printf \"%.2f\",$tot/1000}"); RETR=$rt; }
smoke(){ reset_srv; run_set 4 "0 u"; awk "BEGIN{exit !($AGG>0.05)}" && return 0; sleep 6; reset_srv; run_set 4 "0 u"; awk "BEGIN{exit !($AGG>0.05)}"; }
measure_phase(){ local lbl=$1; reset_srv
  run_set 1 "0 u";                               log "ROW	$lbl	1up_P1	$AGG	$GW	$RETR"
  run_set 8 "0 u";                               log "ROW	$lbl	1up_P8	$AGG	$GW	$RETR"
  run_set 8 "0 u" "1 u" "2 u" "3 d" "4 d" "5 d"; log "ROW	$lbl	half	$AGG	$GW	$RETR"
  run_set 8 "0 u" "1 u" "2 u" "3 u" "4 u" "5 u"; log "ROW	$lbl	allup	$AGG	$GW	$RETR"
  run_set 8 "0 d" "1 d" "2 d" "3 d" "4 d" "5 d"; log "ROW	$lbl	alldown	$AGG	$GW	$RETR"; }
wg_wait_hs(){ local h=0 k; for k in $(seq 1 12); do h=$($SSHO "$GW_SSH" "sudo wg show wg0 latest-handshakes 2>/dev/null|awk '{print \$2}'|grep -c '[1-9]'" 2>/dev/null); [ "${h:-0}" -ge 5 ] && break; sleep 2; done; log "    wg handshakes=${h:-0}/6"; }

# robust kill+device-clean — works on procps AND busybox (busybox pkill -x is unreliable;
# pkill -f self-matches the ssh shell). Kill wireguard-go+tmasque by /proc/*/exe BEFORE
# deleting wg0 (deleting the device under a live wireguard-go leaves it spinning).
killclean(){ $SSHO "$1" "$2 sh -c 'for p in /proc/[0-9]*; do case \"\$(readlink \$p/exe 2>/dev/null)\" in */wireguard-go|*/tmasque) kill -9 \${p#/proc/} 2>/dev/null;; esac; done; sleep 1; for w in \$(ip -br link 2>/dev/null|grep -oE \"^wg[0-9]+\"); do ip link del \$w 2>/dev/null; done; for t in \$(ip -br link 2>/dev/null|grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done; while ip rule del table 9000 2>/dev/null; do :; done; ip route flush table 9000 2>/dev/null; true'" >/dev/null 2>&1; }
# 0 if host is bare: no wireguard-go/tmasque proc, no wg0, no tun[0-9], no table-9000 rule
is_bare(){ local n=$($SSHO "$1" "$2 sh -c 'n=0; for p in /proc/[0-9]*; do case \"\$(readlink \$p/exe 2>/dev/null)\" in */wireguard-go|*/tmasque) n=\$((n+1));; esac; done; ip link show wg0 >/dev/null 2>&1 && n=\$((n+1)); ip -br link 2>/dev/null|grep -qE \"^tun[0-9]+\" && n=\$((n+1)); ip rule list 2>/dev/null|grep -q \"lookup 9000\" && n=\$((n+1)); echo \$n'" 2>/dev/null); [ "${n:-9}" = 0 ]; }
teardown(){
  # GW: compose down fires predown (removes tmasque's own iptables); then strip WG-added rules
  $SSHO "$GW_SSH" "cd ~/tmasqued && sudo docker compose down >/dev/null 2>&1; while sudo iptables -t nat -D POSTROUTING -o $GW_IF -j MASQUERADE 2>/dev/null; do :; done; while sudo iptables -D FORWARD -i wg0 -j ACCEPT 2>/dev/null; do :; done; while sudo iptables -D FORWARD -o wg0 -j ACCEPT 2>/dev/null; do :; done; true"
  killclean "$GW_SSH" sudo &
  local i=0; for d in "${DESTS[@]}"; do killclean "$d" "${SUS[$i]}" & i=$((i+1)); done; wait; sleep 1
  # SANITY GATE: verify every host bare; re-kill dirty ones once; log the verdict
  local hosts=("$GW_SSH" "${DESTS[@]}") sus=(sudo "${SUS[@]}") dirty=""
  i=0; for d in "${hosts[@]}"; do is_bare "$d" "${sus[$i]}" || dirty="$dirty $d"; i=$((i+1)); done
  if [ -n "$dirty" ]; then
    i=0; for d in "${hosts[@]}"; do case " $dirty " in *" $d "*) killclean "$d" "${sus[$i]}";; esac; i=$((i+1)); done; sleep 1
    dirty=""; i=0; for d in "${hosts[@]}"; do is_bare "$d" "${sus[$i]}" || dirty="$dirty $d"; i=$((i+1)); done
  fi
  [ -n "$dirty" ] && log "    ⚠️ NOT BARE after teardown:$dirty" || log "    teardown clean (all bare)"; }

wg_setmtu(){ $SSHO "$GW_SSH" "sudo ip link set wg0 mtu 8000" 2>/dev/null; local i=0; for d in "${DESTS[@]}"; do $SSHO "$d" "${SUS[$i]} ip link set wg0 mtu 8000" >/dev/null 2>&1; i=$((i+1)); done; }
wgk_build(){ $SSHO "$GW_SSH" "sudo ip link set $GW_IF mtu 9000; sudo ethtool -L $GW_IF combined 8 2>/dev/null"
  bash setup_wg.sh >/dev/null 2>&1
  $SSHO "$GW_SSH" "sudo iptables -t nat -C POSTROUTING -o $GW_IF -j MASQUERADE 2>/dev/null||sudo iptables -t nat -A POSTROUTING -o $GW_IF -j MASQUERADE; sudo iptables -C FORWARD -i wg0 -j ACCEPT 2>/dev/null||sudo iptables -I FORWARD -i wg0 -j ACCEPT; sudo iptables -C FORWARD -o wg0 -j ACCEPT 2>/dev/null||sudo iptables -I FORWARD -o wg0 -j ACCEPT"
  wg_setmtu; wg_wait_hs; }

wgu_build(){ local mk='wireguard-go wg0'
  $SSHO "$GW_SSH" "sudo ip link set $GW_IF mtu 9000; sudo ethtool -L $GW_IF combined 8 2>/dev/null; sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1; which wireguard-go>/dev/null 2>&1 || sudo apt-get install -y -q wireguard-go >/dev/null 2>&1; sudo sh -c '[ -f /etc/wg-gw.key ]||{ wg genkey|tee /etc/wg-gw.key|wg pubkey>/etc/wg-gw.pub; }'"
  local i=0; for d in "${DESTS[@]}"; do local su=${SUS[$i]}
    case "$d" in alpine@*) $SSHO "$d" "which wireguard-go>/dev/null 2>&1 || $su apk add wireguard-go >/dev/null 2>&1" &;; *) $SSHO "$d" "which wireguard-go>/dev/null 2>&1 || $su apt-get install -y -q wireguard-go >/dev/null 2>&1" &;; esac; i=$((i+1)); done; wait
  local GWPUB=$($SSHO "$GW_SSH" "sudo cat /etc/wg-gw.pub"); local CPUB=(); i=0
  for d in "${DESTS[@]}"; do local su=${SUS[$i]}; $SSHO "$d" "$su sh -c '[ -f /etc/wg-cl.key ]||{ wg genkey|tee /etc/wg-cl.key|wg pubkey>/etc/wg-cl.pub; }'"; CPUB[$i]=$($SSHO "$d" "$su cat /etc/wg-cl.pub"); i=$((i+1)); done
  $SSHO "$GW_SSH" "sudo sh -c 'ip link del wg0 2>/dev/null; pkill -x wireguard-go 2>/dev/null; sleep 1; $mk; sleep 1; wg set wg0 listen-port $PORT private-key /etc/wg-gw.key; ip addr add $GWWG/24 dev wg0 2>/dev/null; ip link set wg0 up; ip link set wg0 mtu 8000; iptables -t nat -C POSTROUTING -o $GW_IF -j MASQUERADE 2>/dev/null||iptables -t nat -A POSTROUTING -o $GW_IF -j MASQUERADE; iptables -C FORWARD -i wg0 -j ACCEPT 2>/dev/null||iptables -I FORWARD -i wg0 -j ACCEPT; iptables -C FORWARD -o wg0 -j ACCEPT 2>/dev/null||iptables -I FORWARD -o wg0 -j ACCEPT'"
  i=0; for d in "${DESTS[@]}"; do $SSHO "$GW_SSH" "sudo wg set wg0 peer ${CPUB[$i]} allowed-ips $WGNET.$((i+2))/32"; i=$((i+1)); done
  i=0; for d in "${DESTS[@]}"; do local su=${SUS[$i]}; local cip=$WGNET.$((i+2))
    $SSHO "$d" "$su sh -c 'ip link del wg0 2>/dev/null; pkill -x wireguard-go 2>/dev/null; sleep 1; $mk; sleep 1; wg set wg0 private-key /etc/wg-cl.key peer $GWPUB endpoint $GW:$PORT allowed-ips $TGT/32,$GWWG/32 persistent-keepalive 15; ip addr add $cip/24 dev wg0 2>/dev/null; ip link set wg0 up; ip link set wg0 mtu 8000; ip route replace $TGT dev wg0'"; i=$((i+1)); done
  sleep 5; wg_wait_hs; }

tmq_build(){ local mode=$1 extra="" gso=false vhost=false
  # FORWARD_TUN_GSO/VHOST are config-file options now (written into tmasqued.conf below),
  # NOT env vars. FORWARD_UPLOAD_RESEQ is still env-driven (os.Getenv), kept in compose.
  [ "$mode" = gso ]   && { extra=$'      - FORWARD_UPLOAD_RESEQ=1'; gso=true; }
  [ "$mode" = vhost ] && { extra=$'      - FORWARD_UPLOAD_RESEQ=1'; vhost=true; }
  $SSHO "$GW_SSH" "cat > ~/tmasqued/docker-compose.yml <<'YAML'
services:
  tmasqued:
    build: .
    cap_add: [NET_ADMIN, NET_RAW, SYS_ADMIN]
    privileged: true
    network_mode: host
    environment:
      - GOTRACEBACK=all
${extra}
    devices:
      - /dev/net/tun:/dev/net/tun
      - /dev/vhost-net:/dev/vhost-net
    volumes:
      - ./certs:/etc/tmasqued/certs
      - ./ca:/etc/tmasqued/ca
      - ./tmasqued.conf:/etc/tmasqued/tmasqued.conf
      - db:/etc/tmasqued/data
      - /sys/fs/bpf:/sys/fs/bpf
    ulimits:
      memlock: {soft: -1, hard: -1}
    command: /run.sh
volumes:
    db:
YAML
  cd ~/tmasqued; sed -i '/^FORWARD_TUN_GSO=/d;/^FORWARD_TUN_VHOST=/d' tmasqued.conf; printf 'FORWARD_TUN_GSO=%s\nFORWARD_TUN_VHOST=%s\n' $gso $vhost >> tmasqued.conf; sudo ethtool -L $GW_IF combined 8 2>/dev/null; sudo docker compose up -d --build >/dev/null 2>&1; sleep 16"
  # FORWARD ACCEPT (tun+ for GSO-via-main-tun, tmvhost0 for vhost) is installed by tmasque's own postup/001_snat.sh (self-managed)
  local i=0; for d in "${DESTS[@]}"; do local su=${SUS[$i]}; $SSHO "$d" "$su sh -c 'modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1 & i=$((i+1)); done; wait
  local t=0; while [ $t -lt 20 ]; do local up=0; for d in "${DESTS[@]}"; do $SSHO "$d" "ip route get $TGT 2>/dev/null|grep -qE 'dev tun[0-9]+'" && up=$((up+1)); done; [ $up -ge 6 ] && break; sleep 3; t=$((t+1)); done
  log "    tmq-$mode clients up=$up/6"; }

log "=== REGIME-ALL start: 5 impls x 5 rows @ rss, DUR=$DUR (WG first, clean clients) ==="
for m in gso xdp vhost; do
  log ">>> PHASE tmq-$m"; teardown
  case $m in xdp) tmq_build default;; *) tmq_build "$m";; esac
  measure_phase "tmq-$m"
done
log "=== restoring standing gso/rss ==="; teardown; tmq_build gso
log "=== REGIME-ALL DONE ==="

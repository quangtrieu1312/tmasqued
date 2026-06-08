#!/bin/bash
# GSO toggle experiment (2-core fleet): single-client upload+download at jumbo,
# client GSO ON (default) vs OFF (QUIC_GO_DISABLE_GSO=1), with server reseq-OOO
# and dg-packer-retr deltas per run. Server uses AF_XDP (no GSO) — only the client toggles.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
REGIME=rss; MTU=9000; RUNS=2

C0_DEST=$(set -- ${CLIENTS[0]}; echo $1)   # alpine@198.18.5.7 (the 1up/1down client)

kill_one() { $SSH "$1" "doas sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; for t in \$(ip -br link show | grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done'" >/dev/null 2>&1; }
start_one() { # dest  envstr
  $SSH "$1" "doas sh -c 'modprobe tun 2>/dev/null; setsid env $2 /usr/local/bin/tmasque >/tmp/tmq-bench.log 2>&1 </dev/null &'" >/dev/null 2>&1; }

bring_up() { # envstr ("" = GSO on, "QUIC_GO_DISABLE_GSO=1" = off)
  local env="$1" c dest tgt tries
  for c in "${CLIENTS[@]}"; do dest=$(set -- $c; echo $1); kill_one "$dest"; done; sleep 3
  for c in "${CLIENTS[@]}"; do dest=$(set -- $c; echo $1); start_one "$dest" "$env"; done
  for c in "${CLIENTS[@]}"; do
    dest=$(set -- $c; echo $1); tgt=$(set -- $c; echo $8); tries=0
    while [ $tries -lt 12 ]; do
      timeout 6 $SSH "$dest" "ip route get $tgt 2>/dev/null | grep -qE 'dev tun[0-9]+'" && break
      tries=$((tries+1)); sleep 3
    done
  done
}

verify_env() { # echo whether QUIC_GO_DISABLE_GSO is in the client process env
  local pid env
  pid=$($SSH "$C0_DEST" "for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && echo \${d#/proc/} && break; done")
  [ -z "$pid" ] && { echo "    [verify] no tmasque pid on $C0_DEST"; return; }
  env=$($SSH "$C0_DEST" "doas tr '\0' '\n' < /proc/$pid/environ 2>/dev/null | grep -i GSO")
  echo "    [verify] $C0_DEST pid=$pid GSO-env='${env:-<unset>}'"
}

counters() { # echo "preOOO preTot pkRetr pkTot" from the latest gateway stats line
  $SSH "$GW_SSH" "C=\$(doas docker ps -q | head -1); doas docker logs --tail 100 \$C 2>&1 | grep pre-reseq | tail -1" \
    | grep -oE 'ooo=[0-9]+/[0-9]+|dg-packer: genuine=[0-9]+/[0-9]+ \([0-9.]+%\) retr=[0-9]+' \
    | grep -oE '[0-9]+' | tr '\n' ' '
}

echo "### Configuring GW: tmasque regime=$REGIME mtu=$MTU"
bash stack.sh gw_tmasque_config "$REGIME" "$MTU"

for mode in ON OFF; do
  env=""; [ "$mode" = OFF ] && env="QUIC_GO_DISABLE_GSO=1"
  echo; echo "############## GSO $mode  (client env: '${env:-<none>}') ##############"
  bring_up "$env"; verify_env
  for scen in 1up 1down; do
    echo "--- GSO $mode  $scen ---"
    for r in $(seq 1 $RUNS); do
      pre=$(counters)
      row=$(OUT="" bash measure.sh tmasque "$REGIME" "$MTU" tcp "$scen" 2>/dev/null)
      post=$(counters)
      agg=$(printf '%s' "$row" | cut -f6); cores=$(printf '%s' "$row" | cut -f7)
      # deltas
      read po pt pr pkt <<<"$pre"; read qo qt qr qkt <<<"$post"
      dooo=$(( ${qo:-0}-${po:-0} )); dtot=$(( ${qt:-0}-${pt:-0} ))
      dretr=$(( ${qr:-0}-${pr:-0} )); dpkt=$(( ${qkt:-0}-${pkt:-0} ))
      ooopct="n/a"; [ "$dtot" -gt 0 ] && ooopct=$(awk "BEGIN{printf \"%.2f\",100*$dooo/$dtot}")
      retrpct="n/a"; [ "$dpkt" -gt 0 ] && retrpct=$(awk "BEGIN{printf \"%.2f\",100*$dretr/$dpkt}")
      printf "  run%s: agg=%s G  cores=%s/2  pre-reseq-OOO=%s%% (%s/%s)  dg-packer-retr=%s%% (%s/%s)\n" \
        "$r" "${agg:-NA}" "${cores:-NA}" "$ooopct" "$dooo" "$dtot" "$retrpct" "$dretr" "$dpkt"
    done
  done
done
echo; echo "### DONE"
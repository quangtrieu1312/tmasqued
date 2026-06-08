#!/bin/bash
# Isolate the none/1500 UDP DOWNLOAD collapse: fresh tunnels, then solo->2->6 client download.
set -u
cd "$(dirname "$0")"; source ./fleet.sh

echo "### fresh client bringup (none/1500 already configured) ###"
bash stack.sh tmasque_up_all | tail -1

run_dl() { # label  "ssh1 port1" ["ssh2 port2" ...]
  echo "--- $1 ---"; shift
  local pids=() tmp=$(mktemp -d) i=0
  for cp in "$@"; do
    set -- $cp
    $SSH "$1" "iperf3 -c $TARGET -p $2 -t6 -O2 -u -b1G -P2 -R -f m" >"$tmp/$i" 2>&1 &
    pids+=($!); i=$((i+1))
  done
  wait
  i=0; for cp in "$@"; do
    local r=$(grep -E '\[SUM\].*receiver' "$tmp/$i" | tail -1)
    local err=$(grep -iE 'error|refused|timed out|unable' "$tmp/$i" | head -1)
    echo "  client$i: ${r:-NO-SUM}  ${err:+ERR=$err}"
    i=$((i+1))
  done
  rm -rf "$tmp"
}

# cn1 solo, then cn1+alp1, then all 6
run_dl "SOLO cn1 (.122) UDP -R" "ubuntu@198.18.0.122 5201"
sleep 5
run_dl "2-client (cn1+alp1) UDP -R" "ubuntu@198.18.0.122 5201" "alpine@198.18.0.113 5202"
sleep 5
run_dl "6-client UDP -R" \
  "ubuntu@198.18.0.122 5201" "alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" \
  "alpine@198.18.2.75 5204" "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206"

#!/bin/bash
# Reproduce the none/1500 6-client UDP-download collapse with full instrumentation:
# spin (core pegged) vs starvation (idle) vs control-plane (no data crossing GW).
set -u
cd "$(dirname "$0")"; source ./fleet.sh
tmp=$(mktemp -d)

echo "### fresh bringup (none/1500) ###"; bash stack.sh tmasque_up_all | tail -1

# GW: tmasque server pid + ens3 tx bytes BEFORE
read TXB0 RXB0 < <(timeout 8 $SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/statistics/tx_bytes /sys/class/net/$GW_IF/statistics/rx_bytes | tr '\n' ' '")
echo "ens3 tx0=$TXB0 rx0=$RXB0"

echo "### launch 6-client UDP -R flood (-b1G -P2 -t25) ###"
i=0
for cp in "ubuntu@198.18.0.122 5201" "alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" \
          "alpine@198.18.2.75 5204" "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206"; do
  set -- $cp
  timeout 40 $SSH "$1" "iperf3 -c $TARGET -p $2 -t25 -O2 -u -b1G -P2 -R -f m" >"$tmp/c$i" 2>&1 &
  i=$((i+1))
done

sleep 8   # let it reach steady-state collapse
echo "### GW per-core CPU during collapse (mpstat 1x6) ###"
timeout 20 $SSH "$GW_SSH" "mpstat -P ALL 1 6 | awk '/Average:/'" | tee "$tmp/mpstat"
echo "### GW container CPU + tmasque process state ###"
timeout 12 $SSH "$GW_SSH" 'C=$(sudo docker ps -q|head -1); sudo docker stats --no-stream --format "container_cpu={{.CPUPerc}} mem={{.MemUsage}}" $C; echo "tmasque procs:"; sudo docker exec $C sh -c "ps -o pid,stat,pcpu,comm 2>/dev/null | grep -iE \"tmasque|masque|PID\"" | head'
echo "### one client iperf3 output (hang point) ###"
sleep 20; wait
head -25 "$tmp/c0"

# ens3 delta AFTER
read TXB1 RXB1 < <(timeout 8 $SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/statistics/tx_bytes /sys/class/net/$GW_IF/statistics/rx_bytes | tr '\n' ' '")
echo "### ens3 byte delta over the run (did data cross the GW?) ###"
echo "tx_delta=$(( (TXB1-TXB0)/1000000 )) MB   rx_delta=$(( (RXB1-RXB0)/1000000 )) MB"
echo "### per-client [SUM] (if any) ###"
for n in 0 1 2 3 4 5; do echo "  c$n: $(grep -E '\[SUM\].*receiver' "$tmp/c$n" 2>/dev/null | tail -1 || echo NO-SUM)"; done
rm -rf "$tmp"

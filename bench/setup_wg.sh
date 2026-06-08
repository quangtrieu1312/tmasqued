#!/bin/bash
# Build a CLEAN WireGuard gateway mesh for the current fleet.
#   gw wg0 = 10.9.0.1 ; client i wg0 = 10.9.0.(i+2)
#   each client routes ITS target via wg0 (AllowedIPs = target/32 + gw/32); gw MASQUERADEs.
set -u
cd "$(dirname "$0")"; source "${FLEET:-./fleet-8core.sh}"
WGNET=10.9.0; GWWG=$WGNET.1; PORT=51820
case "$GW_SSH" in alpine@*) GSU=doas;; *) GSU=sudo;; esac

echo "[wg-setup] gw $GW_SSH deps + key"
$SSH "$GW_SSH" "$GSU sh -c '
  modprobe wireguard 2>/dev/null
  sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1
  [ -f /etc/wg-gw.key ] || { wg genkey | tee /etc/wg-gw.key | wg pubkey > /etc/wg-gw.pub; }
'"
GWPUB=$($SSH "$GW_SSH" "$GSU cat /etc/wg-gw.pub")

echo "[wg-setup] client keys"
declare -a CPUB
i=0
for c in "${CLIENTS[@]}"; do
  set -- $c; dest="$1"; su="$5"
  $SSH "$dest" "$su sh -c 'modprobe wireguard 2>/dev/null; [ -f /etc/wg-cl.key ] || { wg genkey | tee /etc/wg-cl.key | wg pubkey > /etc/wg-cl.pub; }'"
  CPUB[$i]=$($SSH "$dest" "$su cat /etc/wg-cl.pub")
  i=$((i+1))
done

echo "[wg-setup] gw wg0 up + peers + MASQUERADE"
$SSH "$GW_SSH" "$GSU sh -c '
  ip link del wg0 2>/dev/null; ip link add wg0 type wireguard
  wg set wg0 listen-port $PORT private-key /etc/wg-gw.key
  ip addr add $GWWG/24 dev wg0; ip link set wg0 up
  iptables -t nat -C POSTROUTING -o $GW_IF -j MASQUERADE 2>/dev/null || iptables -t nat -A POSTROUTING -o $GW_IF -j MASQUERADE
  # docker sets FORWARD policy DROP -> explicitly allow wg0 <-> WAN forwarding (no-op on hosts that already ACCEPT)
  iptables -C FORWARD -i wg0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -i wg0 -j ACCEPT
  iptables -C FORWARD -o wg0 -j ACCEPT 2>/dev/null || iptables -I FORWARD -o wg0 -j ACCEPT
'"
i=0
for c in "${CLIENTS[@]}"; do
  $SSH "$GW_SSH" "$GSU wg set wg0 peer ${CPUB[$i]} allowed-ips $WGNET.$((i+2))/32"
  i=$((i+1))
done

echo "[wg-setup] client wg0 up + route target"
i=0
for c in "${CLIENTS[@]}"; do
  set -- $c; dest="$1"; su="$5"; tgt="$8"
  cip=$WGNET.$((i+2))
  $SSH "$dest" "$su sh -c '
    ip link del wg0 2>/dev/null; ip link add wg0 type wireguard
    wg set wg0 private-key /etc/wg-cl.key peer $GWPUB endpoint $GW:$PORT allowed-ips $tgt/32,$GWWG/32 persistent-keepalive 15
    ip addr add $cip/24 dev wg0; ip link set wg0 up
    ip route replace $tgt dev wg0
  '"
  i=$((i+1))
done

sleep 3
echo "[wg-setup] verify handshakes"
$SSH "$GW_SSH" "$GSU wg show wg0 latest-handshakes | awk '{print \$2}' | grep -c '[1-9]'" | sed 's/^/  peers handshaked: /'
echo "[wg-setup] done"

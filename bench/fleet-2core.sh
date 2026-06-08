#!/bin/bash
# 2-core small-VPS environment: 5 Alpine 2-core VMs.
#   gateway  = 198.18.0.113 (tmasqued / WireGuard server)
#   clients  = 198.18.5.7, 198.18.2.75   (VPN clients = iperf3 clients)
#   targets  = 198.18.4.65, 198.18.3.140 (plain iperf3 sinks, reached through the tunnel)
GW=198.18.0.113
GW_SSH=alpine@198.18.0.113
GW_IF=eth0
ENVNAME=2core

# Each client: "SSH_DEST IP CORES OS SUDO WANIF TARGET_SSH TARGET_IP PORT" — 1 target each.
# NOTE: 198.18.3.140 is network-isolated (unreachable) -> only 4 usable Alpines, so both clients
# share one target (198.18.4.65) on distinct ports (like the 8-core fleet's single-target setup).
CLIENTS=(
  "alpine@198.18.5.7  198.18.5.7  2 alpine doas eth0 alpine@198.18.4.65  198.18.4.65  5201"
  "alpine@198.18.2.75 198.18.2.75 2 alpine doas eth0 alpine@198.18.4.65  198.18.4.65  5202"
)
TARGETS_SSH=("alpine@198.18.4.65")
TARGET=198.18.4.65
TARGET_SSH=alpine@198.18.4.65

SSH="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=6"
GW_RSS_COMBINED=2
GW_SUDO=doas

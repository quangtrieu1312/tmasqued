#!/bin/bash
# 8-core gateway environment. Sourced via FLEET=fleet-8core.sh.
GW=198.18.0.130
GW_SSH=ubuntu@198.18.0.130
GW_IF=ens3
ENVNAME=8core

# Each client: "SSH_DEST IP CORES OS SUDO WANIF TARGET_SSH TARGET_IP PORT"
# 6 clients (1x 4-core ubuntu + 5x 2-core alpine), all -> the single 4-core target on its own port.
CLIENTS=(
  "ubuntu@198.18.0.122 198.18.0.122 4 ubuntu sudo ens3 ubuntu@198.18.5.80 198.18.5.80 5201"
  "alpine@198.18.0.113 198.18.0.113 2 alpine doas eth0 ubuntu@198.18.5.80 198.18.5.80 5202"
  "alpine@198.18.5.7   198.18.5.7   2 alpine doas eth0 ubuntu@198.18.5.80 198.18.5.80 5203"
  "alpine@198.18.2.75  198.18.2.75  2 alpine doas eth0 ubuntu@198.18.5.80 198.18.5.80 5204"
  "alpine@198.18.4.65  198.18.4.65  2 alpine doas eth0 ubuntu@198.18.5.80 198.18.5.80 5205"
  "alpine@198.18.3.140 198.18.3.140 2 alpine doas eth0 ubuntu@198.18.5.80 198.18.5.80 5206"
)
# unique targets (ssh dest) for server setup
TARGETS_SSH=("ubuntu@198.18.5.80")
# single-target convenience (used by stack.sh routing/provisioning on this env)
TARGET=198.18.5.80
TARGET_SSH=ubuntu@198.18.5.80

SSH="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=6"
GW_RSS_COMBINED=8
GW_SUDO=sudo

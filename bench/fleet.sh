#!/bin/bash
# tmasqued benchmark fleet inventory (2026-06 reprovisioned testbed, wt0 mesh).
# Sourced by the other harness scripts. Bash arrays (laptop shell is zsh; always run these via bash).

GW=198.18.0.130
GW_SSH=ubuntu@198.18.0.130
GW_IF=ens3

TARGET=198.18.5.80
TARGET_SSH=ubuntu@198.18.5.80
TARGET_IF=ens3

# 6 clients: index 0 = strong 4c ubuntu (cn1), 1..5 = 2c alpine.
# Each entry: "<ssh> <ip> <cores> <os> <sudo> <wanif>"
CLIENTS=(
  "ubuntu@198.18.0.122 198.18.0.122 4 ubuntu sudo ens3"
  "alpine@198.18.0.113 198.18.0.113 2 alpine doas eth0"
  "alpine@198.18.5.7   198.18.5.7   2 alpine doas eth0"
  "alpine@198.18.2.75  198.18.2.75  2 alpine doas eth0"
  "alpine@198.18.4.65  198.18.4.65  2 alpine doas eth0"
  "alpine@198.18.3.140 198.18.3.140 2 alpine doas eth0"
)
# target listen ports, one per client
PORTS=(5201 5202 5203 5204 5205 5206)

SSH="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=6"

# helper: run a command on every client (field 1 = ssh dest)
client_ssh() { echo "$1" | awk '{print $1}'; }
client_ip()  { echo "$1" | awk '{print $2}'; }
client_sudo(){ echo "$1" | awk '{print $5}'; }

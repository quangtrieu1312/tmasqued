#!/usr/bin/env bash
# Remove everything postup/001_snat.sh installed (symmetric teardown). Loop the -D so
# any accumulated duplicates (from a prior SIGKILL'd run) are fully cleared.
while iptables -t nat -D POSTROUTING ! -o tun+ -j MASQUERADE 2>/dev/null; do :; done
for spec in "-o tun+" "-i tun+" "-i tmvhost0" "-o tmvhost0"; do
    while iptables -D FORWARD $spec -j ACCEPT 2>/dev/null; do :; done
done

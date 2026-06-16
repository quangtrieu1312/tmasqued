#!/usr/bin/env bash
# Remove the FORWARD ACCEPT rules installed by postup/002_enable_forward.sh (symmetric
# teardown). Loop the -D so any accumulated duplicates are fully cleared.
# (accept_local needs no teardown: the tm0 device is deleted on shutdown, taking its
# per-device sysctl with it.)
# tm+ covers our main tun (tm0) AND the vhost TAP (tmvhost0). The legacy tun+/tunvhost0
# specs are also torn down so an upgrade over a live box (old-name rules still installed)
# leaves no stale ACCEPT behind.
for spec in "-o tm+" "-i tm+" "-o tun+" "-i tun+" "-i tmvhost0" "-o tmvhost0" "-i tunvhost0" "-o tunvhost0"; do
    while iptables -D FORWARD $spec -j ACCEPT 2>/dev/null; do :; done
done

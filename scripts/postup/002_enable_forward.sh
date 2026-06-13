#!/usr/bin/env bash
# FORWARD ACCEPT, both directions, for the datapath devices (docker leaves the FORWARD
# policy at DROP, so without these forwarding is silently dropped). Install only —
# teardown lives in predown/002_enable_forward.sh.
#   tm+   covers EVERY datapath device of OURS: the main tun "tm0" (download -o tm+, and
#         the GSO forward path's coalesced super-frame writes -i tm+) AND the vhost
#         forward path's dedicated TAP "tmvhost0" — both "tm"-prefixed so this one
#         wildcard matches them (no device-specific rules needed). A coexisting VPN's
#         tun*/wg* is NOT matched here; its own FORWARD policy governs it.
for spec in "-o tm+" "-i tm+"; do
    iptables -I FORWARD 1 $spec -j ACCEPT
done

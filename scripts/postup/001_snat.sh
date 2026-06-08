#!/usr/bin/env bash
# SNAT + FORWARD rules for the datapath. Idempotent (clear stale copies first, then
# install) so a SIGKILL'd previous run that skipped predown doesn't accumulate rules.
# Rules for not-yet-present devices (e.g. tmvhost0) are inert until the device appears,
# so they can be installed unconditionally — no need to read the forward-mode config here.

# masquerade everything leaving the box that is NOT a tun (i.e. out the WAN)
iptables -t nat -D POSTROUTING ! -o tun+ -j MASQUERADE 2>/dev/null
iptables -t nat -I POSTROUTING 1 ! -o tun+ -j MASQUERADE

# FORWARD ACCEPT, both directions, for:
#   tun+      download (WAN->client, -o tun+) AND upload-forward (client->WAN, -i tun+,
#             used by the GSO forward path that writes coalesced super-frames into the
#             main tun for the kernel to ip_forward)
#   tmvhost0  the vhost forward path's dedicated TAP (not tun+-named)
for spec in "-o tun+" "-i tun+" "-i tmvhost0" "-o tmvhost0"; do
    while iptables -D FORWARD $spec -j ACCEPT 2>/dev/null; do :; done
    iptables -I FORWARD 1 $spec -j ACCEPT
done

#!/usr/bin/env bash
# SNAT: masquerade everything leaving the box that is NOT one of OUR tunnels (tm+),
# i.e. out the WAN, a LAN NIC, or a coexisting VPN's interface. Install only — teardown
# lives in predown/001_snat.sh.
iptables -t nat -I POSTROUTING 1 ! -o tm+ -j MASQUERADE

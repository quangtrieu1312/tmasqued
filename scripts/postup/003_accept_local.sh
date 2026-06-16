#!/usr/bin/env bash
# The forward path SNATs inner packets to the GW's own WAN IP, so they arrive on our main
# tun "tm0" sourced from a local address. The kernel martian-drops such locally-sourced
# packets (TCP fails, ICMP happens to survive) unless accept_local is enabled — and only
# tm0 needs it, since that's where the forwarded inner packets ingress. Write the proc
# file directly (sysctl -w isn't always present in the runtime image).
echo 1 > /proc/sys/net/ipv4/conf/tm0/accept_local

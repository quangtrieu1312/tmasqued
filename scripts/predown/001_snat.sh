#!/usr/bin/env bash
# Remove the SNAT installed by postup/001_snat.sh (symmetric teardown). Loop the -D so
# any accumulated duplicates (from a prior SIGKILL'd run) are fully cleared. The legacy
# `! -o tun+` form is also torn down so an upgrade over a live box (old-name rule still
# installed) leaves no stale MASQUERADE behind.
while iptables -t nat -D POSTROUTING ! -o tm+  -j MASQUERADE 2>/dev/null; do :; done
while iptables -t nat -D POSTROUTING ! -o tun+ -j MASQUERADE 2>/dev/null; do :; done

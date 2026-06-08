<!-- ⚠️ SCRATCH / DO NOT PUBLISH ⚠️  Delete before merging to master. -->

# WG vs tmasque regime (SCRATCH) — 2026-06-08, kernel 6.17, stable testbed

5 rows @ rss(combined=8), `iperf3 -P8 -t8 -O2` (P1 for the first row), 6 clients (1×4-core Ubuntu +
5×2-core Alpine) → single 4-core target. Cell = **agg Gbit · summed sender-Retr**. Measured AFTER a
full VM restart that made the fabric stable (direct client→target 24 G / client→GW 9.8 G, low retr).
WG MTU=8000 (path can't carry 9000); tmasque inner=3422.

| row | WG-kernel | WG-userspace | tmq-gso | tmq-xdp | tmq-vhost |
|---|--|--|--|--|--|
| 1 up P1 | 0.00 · 839 | 0.00 · 25k | 0.00 · 11.8k | 0.00 · 10k | 0.00 · 9.6k |
| 1 up P8 | 0.71 · 1.4k | 0.27 · 10.8k | 0.40 · 146k | 0.34 · 83k | 0.25 · 122k |
| half (3u+3d) | 1.40 · 20k | 1.06 · 145k | 2.28 · 215k | 2.66 · 96k | 2.47 · 263k |
| all up | 1.17 · 36k | 0.79 · 75k | 1.13 · 185k | 1.05 · 106k | 4.86 · 497k |
| all down | 1.43 · 20k | 0.99 · 1.2k | 4.36 · 11k | 1.09 · 11k | 4.65 · 23k |

**wgu (wireguard-go userspace) RESOLVED** — the orchestrator's `wgu_build` has a multi-client sequencing
bug (handshakes 0/6, hung). The **manual** per-client setup works: `wireguard-go wg0` → `wg set` →
`ip addr/up/mtu 8000/route`, add each peer on the GW, ping to trigger → 6/6 handshakes, measured above.
wgu < wgk (userspace crypto slower + single-threaded: 0.27 vs 0.71 single-flow upload, higher retr).

## Findings
- **Upload loss IS the tmasque datapath, not the testbed.** WG-kernel upload retr = 1.4k–36k (loss-free,
  as WireGuard should be); tmasque upload retr = 83k–497k on the *same* clean testbed → tmasque's
  per-flow upload loss wall is real and datapath-intrinsic.
- **Single-flow upload: WG wins** (0.71 G clean vs tmasque 0.25–0.40 G lossy).
- **Download: tmasque-gso (4.36) ≈ tmasque-vhost (4.65) > WG (1.43) > tmasque-xdp (1.09)**, all low-retr.
- **vhost = worst loss** (allup 497k retr) — singleton TX-vring reorder.
- Absolute throughput is testbed-capped (~1–5 G: GW Neutron port ~10 G + per-client crypto + 4-core
  target), so these are NOT peak numbers; the loss pattern + relative ordering are the signal.

## Hard-won methodology (cost ~the whole session; see memory)
1. **iperf3 "server busy"** from hung connections silently returns 0.00 → restart iperf3 servers before
   each phase (`reset_srv`). This masked everything for hours.
2. **No smoke-gate on a loss-limited testbed** — single-client upload smoke is ~0 even for a working
   stack, so a throughput gate wrongly rejects valid stacks. Measure all; judge by Retr + handshakes.
3. **Fabric stability is per-VM host placement** — the lossy leg was client→GW (the GW's host); only a
   GW restart cleared it. Always verify client→GW retr (not just client→target) before benchmarking.
4. **WG MTU ≤ 8000** (the .0↔.5 path drops 9000); **vhost FORWARD needs `tmvhost0`** ACCEPT.

<!-- END SCRATCH -->

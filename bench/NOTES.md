# Benchmark labeling notes (for the final MATRIX doc)

## Direct baseline
- Label as **"Direct — RSS-enabled testbed, single config"**.
- Direct traffic is client→target over the fabric and **does NOT traverse the GW**, so the GW NIC
  regime (none/RPS/RSS) is irrelevant to it. We measured it ONCE, on the RSS-enabled testbed (the
  default/headline config), purely as a **wire/medium-quality reference** — to show the underlay is
  excellent (50–62 G TCP) and is not the bottleneck. We deliberately do **not** sweep direct across
  regimes/MTUs-as-a-funnel; the MTU axis for direct is only MSS/datagram-size clamping.
- So in the matrix, direct is one column/section, not a 3-regime block.

## VPN stacks (WireGuard, tmasque)
- Full sweep: regime {none, RPS, RSS} × MTU {9000, 1500} × proto {TCP, UDP} × scenario
  {1up, 1down, half-up/half-down, all-up, all-down}.
- regime = GW NIC: none = Combined=1 + RPS off; RPS = Combined=1 + rps_cpus=ff; RSS = Combined=8.
- tmasque @9000: outer packet capped at 3506 (virtio XDP-native limit) → inner 3422; WG uses full 8920.
- GW CPU reported as busy core-equivalents out of 8 (sum of per-core (100−idle)/100).

## UDP methodology + the worst-corner collapse (document this)
- UDP cells use a FIXED offered rate `-b1G -P2` per client = up to 2 G/client, ~12 G for a 6-client
  `allup`/`alldown`. That is far above any single tunnel's capacity, so UDP cells are a STRESS probe,
  not a clean-rate sweep — every multi-client UDP cell shows high loss by construction.
### ⚠ KNOWN ISSUE: tmasque UDP-flood saturation (worst corner = none + 1500 + UDP + multi-client)
tmasque's multi-client UDP records 0.00/NA at this corner. Honest status — what's proven vs suspected:
- **PROVEN — the tunnel saturates and recovers (not a crash):** ping THROUGH the tunnel during a
  5-client UDP flood → **66% loss to the GW tun endpoint, 100% loss to the target**; baseline and
  +4 s after the flood stops → **0% loss, normal RTT**. So under UDP flood the tunnel becomes
  unusable for everything (ICMP, control, other flows) and fully recovers when load drops.
- **PROVEN — NOT a CPU spin / mutex / deadlock:** GW **57% idle on every core** during the collapse and
  **5–6 GB still forwarded** through the GW in 25 s. The datapath keeps moving data; individual
  flows/ping starve.
- **SPECULATION (not proven, do not assert):** the exact reason iperf3 reports 0.00 (UDP handshake
  can't establish?). The fwmark test REFUTED the "control TCP dies over the tunnel" theory (routing
  cn1's control direct over the fabric — mangle confirmed 9 pkts via ens3 — did NOT fix it).
- **THE REAL GAP (known issue): WireGuard handles this corner, tmasque does not.** WG degraded to
  ~1.1 G heavy-loss but kept reporting; tmasque saturates so hard even ICMP dies. *Suspected* cause:
  tmasque's single-queue userspace QUIC-datagram path has no fair-queueing/AQM, so one UDP flood
  monopolizes the xsk and starves everything — SUSPECTED, not proven. WG is also stronger on UDP
  generally (rss/9000: WG 9.4 G@21% vs tmasque 6.5 G@40%).
- tmasque is fine at solo 1.38 G, 2-client 1.54 G, `-b300M` 0.036% loss, and all TCP.
- **Highest UDP carried:** direct 11.7 G@2.4% ; WireGuard 9.4 G@21% ; tmasque 6.5 G@40% (all rss/9000).
  Clean low-loss carry at the weak corner ≈ 0.6 G@0.036% (solo `-b300M`).

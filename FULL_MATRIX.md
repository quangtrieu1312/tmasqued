# Full benchmark matrix — RSS vs RPS vs none, per scenario/MTU/protocol

Regime = gateway NIC config: **none** = `Combined=1` (single RX queue, no RPS); **RPS** = `Combined=1` + `rps_cpus=ff`; **RSS** = `Combined=8` (8 HW RX queues, 1/core). 5 scenarios × {9000,1500} WAN MTU × {TCP,UDP} × {none,RPS,RSS}, on the 8-core gateway + 6 clients (1× 4-core + 5× 2-core) → a 4-core target. Each TCP cell = `iperf3 -P8 -t10 -O2`; agg = Σ the participating clients' receiver Gbit/s; `cores` = GW busy core-equivalents of 8.

## How to read a cell

Cells pack throughput, gateway CPU and (for UDP) loss, separated by `·`:

- **TCP** `7.78 ·6.48` = **7.78 Gbit/s** · **6.48 of 8** gateway cores busy.
- **UDP** `7.07 ·5.74 ·41.2%` = 7.07 Gbit/s · 5.74/8 cores · **41.2% loss**.
- **Direct rows are special:** just `42.83` (TCP) or `2.00 ·0.0%` (UDP, throughput·loss) — **no `·cores`**, because direct traffic never touches the gateway.
- `collapse` = the UDP-flood known-issue cell (see Notes).
- **Columns = NIC regime** (none / RPS / RSS). **Rows = scenario:** `1 upload`/`1 download` = one client (iperf3 default = upload; `-R` = download); `half` = 3 up + 3 down; `all upload`/`all download` = all 6 clients one way.

## Direct — RSS-enabled testbed, single config (wire/medium reference)

Direct is client→target over the fabric — it **does NOT traverse the gateway** (gw≈0), so the regime is irrelevant to it. Measured once, on the RSS testbed, purely to show the underlay is excellent and not the bottleneck. (MTU axis = MSS/datagram-size clamp.) Cells = throughput (TCP) or throughput·loss% (UDP) — no `·cores`.

| scenario | TCP 9000 | TCP 1500 | UDP 9000 | UDP 1500 |
|---|--:|--:|--:|--:|
| 1 upload | 42.83 | 52.27 | 2.00 ·0.0% | 2.00 ·0.2% |
| 1 download | 50.12 | 47.12 | 2.00 ·0.0% | 2.00 ·0.0% |
| half (3 up + 3 down) | 60.12 | 61.49 | 10.27 ·14.2% | 3.75 ·31.3% |
| all upload | 61.58 | 60.17 | 8.45 ·29.7% | 2.22 ·81.3% |
| all download | 51.03 | 51.89 | 11.71 ·2.4% | 7.40 ·0.2% |

## WireGuard (kernel wg0)

Throughput **Gbit/s** · gateway **cores busy /8**
(UDP adds **· loss%**; UDP is a fixed `-b1G×2`/client offered-rate stress probe — see note ¹).

### TCP — WAN 9000 (inner 8920)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 4.73 ·3.11 | 4.24 ·2.66 | 6.08 ·3.25 |
| 1 download | 5.39 ·2.25 | 4.55 ·2.36 | 5.56 ·3.46 |
| half (3 up + 3 down) | 4.43 ·3.52 | 4.11 ·2.76 | 12.46 ·6.81 |
| all upload | 4.35 ·3.01 | 4.69 ·2.96 | 7.78 ·6.48 |
| all download | 4.26 ·3.48 | 4.35 ·3.08 | 9.57 ·5.65 |

### TCP — WAN 1500 (inner 1416/1420)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 1.42 ·2.69 | 1.45 ·3.11 | 1.39 ·2.75 |
| 1 download | 1.59 ·2.04 | 1.47 ·2.02 | 1.56 ·2.16 |
| half (3 up + 3 down) | 1.64 ·3.11 | 1.74 ·3.17 | 8.25 ·7.86 |
| all upload | 1.61 ·3.41 | 1.50 ·2.48 | 6.39 ·7.93 |
| all download | 1.52 ·3.13 | 1.39 ·2.93 | 8.82 ·7.79 |

### UDP (offered `-b1G×2`/client) — WAN 9000 (inner 8920)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 2.00 ·1.81 ·0.0% | 2.00 ·0.87 ·0.1% | 2.00 ·2.16 ·0.0% |
| 1 download | 2.00 ·0.85 ·0.0% | 1.98 ·1.92 ·1.1% | 2.00 ·1.86 ·0.0% |
| half (3 up + 3 down) | 5.42 ·2.91 ·45.8% | 6.10 ·3.06 ·49.2% | 9.45 ·5.57 ·21.3% |
| all upload | 4.37 ·3.00 ·45.2% | 6.51 ·2.86 ·45.7% | 7.07 ·5.74 ·41.2% |
| all download | 6.81 ·2.44 ·31.6% | 4.93 ·2.32 ·50.0% | 9.44 ·4.70 ·21.3% |

### UDP (offered `-b1G×2`/client) — WAN 1500 (inner 1416/1420)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 1.65 ·2.81 ·6.8% | 1.63 ·3.03 ·11.0% | 1.66 ·3.03 ·5.9% |
| 1 download | 1.11 ·2.35 ·44.0% | 1.25 ·2.96 ·37.0% | 1.42 ·2.83 ·29.0% |
| half (3 up + 3 down) | 2.61 ·3.01 ·54.4% | 1.57 ·3.51 ·77.8% | 3.50 ·7.73 ·51.0% |
| all upload | 1.57 ·0.00 ·79.0% | 1.61 ·1.29 ·73.0% | 2.49 ·7.94 ·73.5% |
| all download | 1.10 ·2.84 ·66.4% | 1.57 ·3.03 ·72.8% | 2.84 ·5.55 ·60.3% |

## tmasque (AF-XDP / QUIC-MASQUE)

Throughput **Gbit/s** · gateway **cores busy /8**
(UDP adds **· loss%**; UDP is a fixed `-b1G×2`/client offered-rate stress probe — see note ¹).

### TCP — WAN 9000 underlay (outer capped 3506 → inner 3422)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 2.74 ·3.39 | 2.82 ·3.52 | 3.19 ·3.25 |
| 1 download | 2.52 ·3.54 | 2.53 ·3.48 | 2.62 ·3.11 |
| half (3 up + 3 down) | 2.73 ·4.04 | 2.72 ·3.92 | 7.98 ·6.23 |
| all upload | 2.77 ·3.76 | 3.06 ·3.85 | 7.81 ·6.09 |
| all download | 2.69 ·3.92 | 2.85 ·4.20 | 7.73 ·6.24 |

### TCP — WAN 1500 (inner 1416/1420)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 1.34 ·3.36 | 1.25 ·3.03 | 1.70 ·3.24 |
| 1 download | 1.19 ·3.24 | 1.21 ·3.38 | 1.28 ·3.05 |
| half (3 up + 3 down) | 1.32 ·3.92 | 1.37 ·4.08 | 4.19 ·6.38 |
| all upload | 1.43 ·3.74 | 1.39 ·3.81 | 3.96 ·6.06 |
| all download | 1.24 ·4.03 | 1.19 ·3.49 | 4.18 ·6.34 |

### UDP (offered `-b1G×2`/client) — WAN 9000 underlay (outer capped 3506 → inner 3422)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 1.95 ·2.21 ·2.2% | 1.98 ·2.22 ·0.9% | 1.99 ·2.51 ·0.7% |
| 1 download | 1.99 ·2.25 ·0.4% | 1.99 ·2.41 ·0.3% | 2.00 ·2.23 ·0.2% |
| half (3 up + 3 down) | 3.07 ·3.43 ·70.2% | 4.98 ·3.45 ·58.7% | 5.17 ·6.04 ·56.5% |
| all upload | 2.84 ·3.07 ·79.0% | 4.83 ·3.23 ·62.6% | 4.52 ·6.09 ·62.8% |
| all download | 3.19 ·3.57 ·66.8% | 3.22 ·3.72 ·64.6% | 6.51 ·5.34 ·40.3% |

### UDP (offered `-b1G×2`/client) — WAN 1500 (inner 1416/1420)
| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |
|---|--:|--:|--:|
| 1 upload | 1.46 ·3.21 ·27.0% | 1.41 ·2.94 ·30.0% | 1.73 ·3.16 ·13.0% |
| 1 download | 1.41 ·3.19 ·28.0% | 1.43 ·3.32 ·26.0% | 1.61 ·2.97 ·20.0% |
| half (3 up + 3 down) | 1.31 ·3.39 ·82.4% | 2.32 ·3.34 ·69.5% | 3.42 ·6.32 ·60.7% |
| all upload | 1.29 ·3.74 ·84.8% | 1.30 ·3.85 ·91.5% | 3.42 ·5.99 ·72.2% |
| all download | **collapse**¹ | 2.38 ·3.42 ·69.2% | 2.82 ·5.08 ·57.8% |

## Why RSS, not RPS

Read down the `none → RPS → RSS` columns for the multi-client rows (`half`, `all upload`, `all download`). On this 8-core box:

- **`none` (Combined=1) funnels both** — a single RX queue caps WireGuard at ~4.4 G and tmasque at ~2.8 G (TCP 9000), and more clients don't help. Note the gateway is **not** CPU-pegged (~3–4 of 8 cores busy): the limit is the single queue's serial RX/dispatch, not a maxed core.
- **RPS does not un-funnel either, here.** RPS only re-spreads the kernel RX *softirq* across cores, and that helps only when the softirq core is the bottleneck — at ~4.4 G on this fast box it isn't, so WireGuard stays ~flat (`all upload` 4.35 → 4.69). tmasque's AF-XDP datapath **bypasses the kernel softirq entirely**, so RPS can *never* move it, on any box. _(On an earlier, slower softirq-bound gateway where WG's RX core was pegged at 100%, RPS did 2× kernel-WireGuard — the classic result, just not reproduced on this faster hardware.)_
- **RSS is the actual un-funnel — for both (~2×).** 8 hardware RX queues IRQ-pinned per core: WG's softirq spreads natively, and tmasque binds **one `xsk` per RX queue** (eBPF redirects by `ctx->rx_queue_index`), so distinct client flows hash to distinct queues → cores (~6 of 8 busy). Single-client rows don't benefit — one flow → one queue.

## 2-core gateway — detail (small VPS, single RX queue)

Older testbed: 5 VMs (AMD EPYC-Rome, **2 vCPU each**, 9000 B links, single NIC RX queue). One gateway (tmasqued *or* WireGuard), two clients, two `iperf3` targets reached *through* the tunnel. The 2-vCPU gateway is the shared bottleneck. `(gw N)` = gateway CPU% on the 2-vCPU box.

### TCP — jumbo tunnel (inner ~3398 B)
| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up | 21.5 (gw 0) | **3.35** (gw 82) | 2.05 (gw 66) |
| 1 client, down | 21.5 (gw 0) | 1.85 (gw 65) | 1.75 (gw 61) |
| 2 clients, up (each) | 20.0 / 20.4 | 0.74 / 1.82 | **1.38 / 1.38** |
| 2 clients, down (each) | 20.4 / 20.3 | 0.57 / 1.25 | 0.98 / 0.88 |

### TCP — standard tunnel (inner ~1500 B)
| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up | 22.1 | 1.66 | 1.17 |
| 1 client, down | 22.5 | 1.81 | 0.87 |
| 2 clients, up (each) | 21.1 / 21.6 | 0.87 / 0.75 | 0.66 / 0.69 |
| 2 clients, down (each) | 20.2 / 20.1 | 0.77 / 1.16 | 0.46 / 0.47 |

**Fairness flips with MTU:** at jumbo tmasque splits the gateway evenly across two clients (1.38/1.38 = 2.76) and edges WG, which starves one (0.74/1.82 = 2.56); at 1500 it reverses (WG 0.87/0.75 = 1.62 balanced, tmasque 0.66/0.69 = 1.35).

### Client-to-client (both endpoints are VPN clients; gateway relays), inner 1500
| direction | WireGuard | tmasque |
|---|--:|--:|
| TCP up | **1.29** | 0.83 |
| TCP down | **1.22** | 0.89 |

WG relays spoke↔spoke in-kernel; tmasque decaps from one client and **re-encapsulates into the other's QUIC tunnel** (double the userspace work), so it trails. _(2-core UDP runs on this older testbed predate the clean method and were unreliable — omitted.)_

## Notes

**¹ UDP is an offered-rate stress probe, not a clean-rate sweep.** Each client offers `-b1G -P2` (up to ~12 G for a 6-client run) regardless of capacity, so every multi-client UDP cell shows heavy loss by construction. The headline UDP number is the **peak carried**, below.

**Peak UDP carried:** **direct 11.7 G@2.4% · WireGuard 9.4 G@21% · tmasque 6.5 G@40%** (all RSS/9000). Clean low-loss carry ≈ 0.6 G@0.036% (solo `-b300M`).

**⚠ Known issue — UDP-flood saturation (the `collapse`/0.00 cell, none+1500+UDP+multi-client).** *Proven:* under the flood the tmasque tunnel saturates — ping through it hits 66% loss to the GW and 100% to the target — and **fully recovers within ~4 s** of the load stopping (not a crash). The GW is **not** spinning/deadlocked (57% idle, still forwards 5–6 GB). *Suspected (not proven):* the single-queue userspace QUIC-datagram path has no fair-queueing/AQM, so one UDP flood monopolizes the xsk and starves everything (ping, control, other flows) — and the exact reason iperf3 then logs 0.00 is unverified. *The real gap:* **WireGuard degrades gracefully here (kept ~1.1 G); tmasque does not.** Tracked as a known issue, not dismissed. (tmasque is fine at solo/2-client, gentler rates, and all TCP.)

**tmasque's "9000" is really 3506.** `virtio_net` rejects an XDP-native attach above a 3506 B link MTU, so on the 9000 underlay tmasque's outer packet is capped at 3506 (inner 3422) while WireGuard runs the full 8920.

**Why UDP is bounded (`-b1G×2`, not `-b0`).** `iperf3 -u -b0` is a flood, not a measurement: UDP has no congestion control, so it emits as fast as the (single-threaded, CPU-bound) sender allows. Through a tunnel an unmetered flood overruns the datagram queue → heavy loss, and on the QUIC tunnel it drops the connection outright. A bounded offered rate keeps the test meaningful; a true UDP ceiling test would sweep `-b<rate>` and report loss at each step.

**Scope.** Cloud virtio NICs on one controlled tenant network (no public-internet leg) — absolute numbers are environment-specific; the comparisons (WG vs tmasque, regime, MTU) are the point. The 8-core run's single 4-core target caps aggregate at ~9–10 G. Treat ±10–15% as noise.

# Benchmarks — direct vs WireGuard vs tmasque

Two testbeds:
1. **Multi-queue / RSS (8-core gateway, 6 clients)** — the main result: the **total throughput a single
   gateway forwards across many concurrent clients**, with the NIC's multi-queue RSS enabled, and the
   **no-RPS / RPS / RSS** progression that explains it.
2. **2-vCPU gateway (single RX queue, 1–2 clients)** — an earlier **per-connection** study (what one or
   two clients get through a small/cheap gateway), kept below.

## How to read this

**The layers.** Traffic is wrapped twice on its way through the tunnel:

```
app data
  └─ inner IP packet        ← rides the TUN device; its size = the "inner MTU"
       └─ QUIC/UDP packet    ← the "outer" packet; crosses the WAN between client and gateway
```

The client wraps (app → inner → outer). The **gateway unwraps each inner packet on upload
and re-wraps it on download** — so the VPN's CPU cost is paid **per inner packet** (one
AEAD + datagram-queue op each). A **bigger inner MTU = fewer packets = less gateway CPU per
byte**, which is why a jumbo inner MTU roughly **doubles** the VPNs' throughput but does
nothing for **direct** (no per-packet userspace tax to amortize).

**Reading a cell.** Throughput in **Gbit/s**; `(gw N)` is the gateway **core-equivalents busy
out of 8** during the run (e.g. `gw 5.7` = 5.7 of 8 cores, ~71% aggregate; `gw 0` = the gateway
isn't on the path). `agg` = sum of all clients' `iperf3` *receiver* rates. `direct` = no-VPN baseline.

---

# 1. Multi-queue (RSS) — 6-client server aggregate

## Environment

8 cloud VMs (OpenStack / KVM, **virtio-net**, kernel 6.x, **9000 B jumbo** underlay). The NIC has
**RSS multi-queue enabled** — `ethtool -l` shows `Combined: 8`, i.e. **8 hardware RX queues, each
IRQ-pinned to its own core** (`/proc/interrupts`: `virtio-input.0..7` → cpu0..7).

| role | VMs | vCPU | runs |
|---|---|--:|---|
| **gateway** | 1 | **8** | tmasqued container (XDP-native, 8 `xsk` / 8 TX buckets) **or** kernel WireGuard (`wg0` + MASQUERADE) |
| **target** (separate, non-VPN) | 1 | **4** | plain `iperf3 -s` sink, reached *through* the tunnel |
| **clients** | 6 | **1× 4-core (Ubuntu) + 5× 2-core (Alpine)** | tmasque client **or** WireGuard peer |

Baseline is **kernel WireGuard** (`wireguard.ko`, no `wireguard-go`/`boringtun`). Both VPNs ran on the
exact same boxes/MTUs; only the gateway software and the client tunnel differ.

## How each cell is measured

Per client, run concurrently (each client → its own port on the target / gateway):

```sh
# TCP upload  (one of the 6 clients):
iperf3 -c <target> -p <port> -t 10 -O 2 -P 8           # 8 streams, 10 s, first 2 s discarded
# TCP download: add -R
iperf3 -c <target> -p <port> -t 10 -O 2 -P 8 -R
# UDP: bounded rate (NOT -b0 — see UDP note), 2 streams so iperf3 emits a [SUM] line
iperf3 -c <target> -p <port> -u -b 1G -P 2
```

- **iperf3 direction:** `iperf3 -c` sends **upload** (client → server) **by default**; adding **`-R`** reverses it to **download** (server → client). So an *upload* row = clients pushing to the target; a *download* row = the target pushing to the clients.
- **Scenarios:** `all upload`/`all download` = all 6 clients in one direction at once; `half (3 up + 3 down)` = 3 clients upload + 3 download concurrently; `1 upload`/`1 download` = a single client.
- `agg` = sum of the participating clients' receiver Gbit/s. Gateway CPU sampled with `mpstat -P ALL` over the run.
- **Don't use `-b 0` for UDP** — an unmetered flood overruns the datagram queue and *collapses the QUIC
  tunnel* (the connection drops; clients with a low reconnect budget then exit). We cap at `-b 1G ×2`.

### What "p2p" means

`p2p` runs the **`iperf3 -s` server on the gateway itself**, bound to the gateway's **tunnel-inner IP**
(tmasque `100.64.0.1`, WireGuard `10.0.0.1`); clients `iperf3 -c <that IP>`. Traffic is
decrypted/decapsulated and **delivered locally at the gateway** — it never forwards out to an external
target. This isolates the **tunnel-terminate** cost from the **forwarding** cost.

For tmasque, p2p requires a `gwself` `100.64.0.1/32` resource assigned to the client roles so the daemon
**locally delivers** the inner packet to its own TUN. That local-delivery path goes through the **kernel
TUN** (by design — it's not the AF_XDP forward path), which is why tmasque's p2p is much slower than its
forward path, and far slower than WireGuard's in-kernel re-inject. **p2p is a datapath probe, not the
real VPN use case** (which is forward, client→gateway→target).

## Full per-regime matrix (fresh run — supersedes the curated tables that were here)

Complete grid: **{none, RPS, RSS} × {1up, 1down, half, allup, alldown} × {9000, 1500} × {TCP, UDP}** for direct / WireGuard / tmasque, GW cores per cell. (Also mirrored in `matrix-results/MATRIX_FULL.md`.)

### How to read a cell

Each cell packs throughput, gateway CPU, and (for UDP) loss, separated by `·`:

- **TCP cell** `7.78 ·6.48` → **7.78 Gbit/s** aggregate throughput · **6.48 of 8** gateway cores busy.
- **UDP cell** `7.07 ·5.74 ·41.2%` → 7.07 Gbit/s · 5.74/8 cores · **41.2% packet loss**.
- **Direct cells** show throughput only (TCP) or throughput + loss (UDP) — no CPU, because direct traffic never touches the gateway (`gw ≈ 0`).
- `collapse` = the UDP-flood known-issue cell (see *Peak UDP & the UDP-flood known issue*).

Reading the rest of the grid:
- **Columns = NIC regime:** `none` (Combined=1, single RX queue, no RPS) · `RPS` (Combined=1 + `rps_cpus=ff`) · `RSS` (Combined=8, 8 HW queues).
- **Rows = scenario:** `1 upload`/`1 download` = one client (iperf3 default = upload; `-R` = download); `half (3 up + 3 down)` = 3 clients each way; `all upload`/`all download` = all 6 clients one way. Aggregate = Σ the participating clients' receiver Gbit/s.
- **Throughput** is `iperf3 -P8` (8 parallel streams) per client; **GW cores** = busy core-equivalents of 8 (Σ per-core `(100−idle)/100` from `mpstat -P ALL`). Higher throughput at *lower* cores = more efficient.

## Direct — RSS-enabled testbed, single config (wire/medium reference)

Direct is client→target over the fabric — it **does NOT traverse the gateway** (gw≈0), so the regime is irrelevant to it. Measured once, on the RSS testbed, purely to show the underlay is excellent and not the bottleneck. (MTU axis = MSS/datagram-size clamp.)

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


### Peak UDP & the UDP-flood known issue

**Peak UDP carried:** **direct 11.7 G@2.4% · WireGuard 9.4 G@21% · tmasque 6.5 G@40%** (all RSS/9000). Clean low-loss carry ≈ 0.6 G@0.036% (solo `-b300M`).

**⚠ Known issue — UDP-flood saturation (the `collapse`/0.00 cell, none+1500+UDP+multi-client).** *Proven:* under the flood the tmasque tunnel saturates — ping through it hits 66% loss to the GW and 100% to the target — and **fully recovers within ~4 s** of the load stopping (not a crash). The GW is **not** spinning/deadlocked (57% idle, still forwards 5–6 GB). *Suspected (not proven):* the single-queue userspace QUIC-datagram path has no fair-queueing/AQM, so one UDP flood monopolizes the xsk and starves everything (ping, control, other flows) — and the exact reason iperf3 then logs 0.00 is unverified. *The real gap:* **WireGuard degrades gracefully here (kept ~1.1 G); tmasque does not.** Tracked as a known issue, not dismissed. (tmasque is fine at solo/2-client, gentler rates, and all TCP.)

## The three regimes — no-RPS vs RPS vs RSS (server aggregate, 8-core gateway)

The headline above is the **RSS** row. The other two regimes (measured on a single-RX-queue gateway,
4 clients = one 4-core + three 2-core, jumbo) show *why* RSS is the unlock and why tmasque needed it:

| gateway aggregate, upload | WireGuard | tmasque | gateway CPU |
|---|--:|--:|---|
| **1 RX queue** (`Combined=1`), no RPS | ~3.4 G | ~3.4 G | **one core 100%** (RX softirq / xsk-drain), other 7 idle |
| **1 RX queue** + **RPS on**           | **~7.3 G** | ~3.0 G *(unchanged)* | WG spreads to ~3.4/8; tmasque stays on ~1 RX core |
| **8 RX queues** (`Combined=8`, **RSS**), 6 clients | **~9.7 G** | **~8.1 G** | both ~5–6/8, no core pegged |

- **The funnel.** One RX queue → every frame on **one core's softirq**, capping the 8-core box at
  ~3.4 G for *either* VPN (WG's kernel decrypt or tmasque's single AF_XDP RX ring drained by one
  goroutine). More clients don't help — the other cores stay idle.
- **RPS rescues WireGuard, not tmasque.** RPS re-hashes the kernel RX softirq across cores → WG
  **3.4 → 7.3 G**. tmasque's AF_XDP datapath **bypasses the softirq RPS acts on**, so RPS is inert
  (~3.0 G). The kernel datapath has a kernel knob; the kernel-bypass datapath needs the hardware.
- **RSS removes it for both.** 8 hardware RX queues IRQ-pinned per core: WG's softirq spreads natively
  (no RPS), and tmasque binds **one `xsk` per RX queue** (the eBPF redirects by `ctx->rx_queue_index`),
  so distinct client flows hash to distinct queues→cores. Both clear ~8–10 G with ~2–3 cores to spare.

## Reading the RSS results

- **tmasque can't actually use the 9000 underlay — its outer MTU is clamped to 3506.** `virtio_net`
  rejects an **XDP-native** attach when the link MTU is > **3506** (`src/main.go` `xdpNativeMaxMTU`,
  mirrored in `scripts/bootstrap/004`), so on the 9000 jumbo fabric tmasque's *outer/WAN* packet is
  pinned to 3506 → inner tun MTU **3422**, while WireGuard's `wg0` runs the full **8920**. So the
  "WAN 9000" column compares tmasque at a **~2.5× smaller packet** against WG at full jumbo.
- **At a jumbo inner MTU, tmasque ≈ kernel WireGuard on the forward path** (all-up 8.1 vs ~7.9–9.7) —
  the 3422-byte inner MTU amortizes the per-packet userspace tax. It reaches this *despite* the smaller
  outer packet above; closing the rest needs XDP multi-buffer / a larger UMEM frame (or a non-virtio NIC).
- **At a 1500 inner MTU, WireGuard leads** (all-up 6.2 vs 4.0; all-down 8.8 vs 4.1) — its in-kernel
  datapath is more per-packet-efficient at the small MTU. It also **costs more gateway CPU** to get
  there (WG ~7.4/8 vs tmasque ~5.8/8): WG is doing more work per byte but has the cores to spend.
- **p2p (terminate at gateway): WireGuard wins by a lot** (21 G vs 2.6 G @ 9000). WG re-injects the
  decrypted packet in-kernel; tmasque's terminate-at-gateway uses the deliberately-slow kernel-TUN
  local-delivery path. This says nothing about the **forward** path, which is what a gateway VPN does.
- **Neither VPN is underlay-bound** — direct is 50–67 G (the cloud virtio jumbo fabric is far above
  10 G). tmasque's gateway idles ~2 cores on forward; the limiters are the single 4-core target's RX
  and **per-flow inner-TCP loss (~2 G/flow)**, not the gateway. Saturating the 8-core gateway would
  need more/stronger distinct client flows than this 6-box fleet provides.

---

# 2. Earlier testbed — 2-vCPU gateway, single RX queue (per-connection)

5 VMs (AMD EPYC-Rome, **2 vCPU each**, kernel 6.12, 9000 B links, single NIC RX queue). One is the VPN
gateway (tmasqued container *or* WireGuard server); two are clients; two are `iperf3` targets reached
*through* the tunnel. Measurement: `iperf3 -P8 -t10 -O2` (8 streams, 10 s, first 2 s discarded); `-R`
for download; 2-client rows run both clients at once to different targets. The **2-vCPU gateway is the
shared bottleneck** here — these isolate per-client behaviour, not the multi-queue server ceiling above.

## TCP — jumbo tunnel (inner MTU ~3398 B) — the design point

| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up         | 21.5 (gw 0) | **3.35** (gw 82) | 2.05 (gw 66) |
| 1 client, down       | 21.5 (gw 0) | 1.85 (gw 65) | 1.75 (gw 61) |
| 2 clients, up (each) | 20.0 / 20.4 (gw 1) | 0.74 / 1.82 (gw 77) | **1.38 / 1.38** (gw 71) |
| 2 clients, down(each)| 20.4 / 20.3 (gw 1) | 0.57 / 1.25 (gw 68) | 0.98 / 0.88 (gw 65) |

## TCP — standard tunnel (inner MTU ~1500 B)

| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up         | 22.1 | 1.66 | 1.17 |
| 1 client, down       | 22.5 | 1.81 | 0.87 |
| 2 clients, up (each) | 21.1 / 21.6 | 0.87 / 0.75 | 0.66 / 0.69 |
| 2 clients, down(each)| 20.2 / 20.1 | 0.77 / 1.16 | 0.46 / 0.47 |

**The fairness result flips between the two MTUs.** At jumbo, tmasque splits the 2-vCPU gateway evenly
across the two clients (1.38 / 1.38 = 2.76 agg) and edges WireGuard, which starves one client
(0.74 / 1.82 = 2.56). At a 1500 inner MTU it's the opposite: WireGuard is balanced and leads on
aggregate (0.87 / 0.75 = 1.62) while tmasque trails (0.66 / 0.69 = 1.35).

## Client-to-client (both endpoints are VPN clients; the gateway relays), inner MTU 1500

| direction | WireGuard | tmasque |
|---|--:|--:|
| TCP up   | **1.29** | 0.83 |
| TCP down | **1.22** | 0.89 |

WireGuard relays spoke↔spoke in-kernel; tmasque must decap from one client and **re-encapsulate into the
other client's QUIC tunnel** (double the userspace work), so it trails here.

---

## UDP — why we bound the rate

`iperf3 -u -b 0` is a **flood, not a throughput measurement**. UDP has no congestion control, so `-b 0`
emits as fast as the sender's CPU allows with zero backpressure; iperf3's UDP sender is single-threaded
and **CPU-bound generating packets** (which is why even *direct* reads higher at jumbo than at 1500 —
fewer, larger packets = less sender CPU). Pushed through a tunnel the unmetered flood overruns the
datagram queue → heavy loss, and on the QUIC tunnel it **drops the connection outright**. The RSS UDP
table above therefore uses a **bounded** `-b 1G ×2` per client; it's a relative check at a fixed offered
rate, not a max-throughput ceiling. A proper UDP ceiling test would sweep `-b <rate>` and report loss at
each step.

## Scope / caveats — read before quoting

- **Mixed testbeds.** Section 1 is an 8-core multi-queue gateway with 6 clients; Section 2 is a 2-vCPU
  single-queue gateway with 1–2 clients. The no-RPS/RPS rows of the three-regime table are 4-client.
  Compare *within* a section; the cross-regime story is about **who un-funnels under RPS vs RSS**, not a
  controlled absolute ladder.
- **One run per cell** (no error bars); a couple of cells show client/target hiccups (noted inline, e.g.
  the WG@9000 cn1 flow). Treat ±10–15% as noise.
- **Cloud virtio underlay**, controlled lab path on one tenant network, no public-internet leg. Absolute
  numbers are environment-specific — treat them as a *relative* direct-vs-WireGuard-vs-tmasque comparison.
- Several tmasque datapath constants (buffer sizes, datagram-queue depths, pacing floor) were tuned
  empirically and would need revisiting at much higher rates.


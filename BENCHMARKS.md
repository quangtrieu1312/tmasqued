# Benchmarks — direct vs WireGuard vs tmasque

The full data behind the headline table in the [README](README.md#performance).

## Environment

5 VMs (AMD EPYC-Rome, **2 vCPU each**, kernel 6.12). One is the VPN gateway (runs the `tmasqued`
container *or* the WireGuard server); two are clients; two are `iperf3` target servers each client
reaches *through* the tunnel. Routed path between subnets is ~1500 B.

Per cell: `iperf3 -P8 -t10 -O2` (8 streams, 10 s, first 2 s discarded); `-R` for download; the
2-client cases run both clients concurrently to different targets. `gw` = gateway CPU% during the
run (the shared bottleneck for any VPN here).

## TCP (Gbit/s, with gateway CPU%)

The headline table is at a **jumbo inner MTU** (tmasque derives ~3398 B). For the **VPNs** a jumbo
inner MTU roughly **doubles** throughput vs the 1500 table below — it amortizes per-packet cost on
the CPU-bound gateway (the routed path is ~1500 B, so the jumbo *outer* fragments, but the gateway
does far fewer userspace/QUIC ops per byte). **Direct** is path-limited either way, so its two
tables match.

| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up         | 21.5 (gw 0) | **3.35** (gw 82) | 2.05 (gw 66) |
| 1 client, down       | 21.5 (gw 0) | 1.85 (gw 65) | 1.75 (gw 61) |
| 2 clients, up (each) | 20.0 / 20.4 (gw 1) | 0.74 / 1.82 (gw 77) | **1.38 / 1.38** (gw 71) |
| 2 clients, down(each)| 20.4 / 20.3 (gw 1) | 0.57 / 1.25 (gw 68) | 0.98 / 0.88 (gw 65) |

1500-MSS (throughput only):

| case | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up         | 22.1 | 1.66 | 1.17 |
| 1 client, down       | 22.5 | 1.81 | 0.87 |
| 2 clients, up (each) | 21.1 / 21.6 | 0.87 / 0.75 | 0.66 / 0.69 |
| 2 clients, down(each)| 20.2 / 20.1 | 0.77 / 1.16 | 0.46 / 0.47 |

**The fairness result inverts at 1500.** Here WireGuard is balanced and leads on aggregate
(2-client up 0.87 / 0.75 = 1.62) while tmasque is lower (0.66 / 0.69 = 1.35) — the opposite of the
jumbo table, where tmasque splits evenly (1.38 / 1.38 = 2.76) and WG starves one client (0.74 /
1.82 = 2.56). tmasque's even-split + aggregate edge is **specific to the jumbo inner MTU** it's
designed around; at the ~1500 B path MTU the kernel datapath wins.

## Client-to-client (both endpoints are VPN clients; the gateway relays), MTU 1500

| direction | WireGuard | tmasque |
|---|--:|--:|
| TCP up   | **1.29** | 0.83 |
| TCP down | **1.22** | 0.89 |

WireGuard relays spoke↔spoke in-kernel; tmasque must decap from one client and **re-encapsulate
into the other client's QUIC tunnel** (double the userspace work), so it trails here. Getting this
working at all required a server fix — an `ipToTunChan` reconnect race was silently black-holing
peer delivery until guarded.

## UDP

Driven with `iperf3 -u -b 0` (unlimited). Direct sustains ~4.4 Gbit/s (jumbo) / ~2.3 Gbit/s (1500).
Through a tunnel, an unlimited UDP flood saturates the link and **starves iperf3's own in-band TCP
control channel**, so the tunnelled UDP cells are erratic (heavy loss, some fail to complete) for
*both* WireGuard and tmasque — a stress artifact, not a clean rate, so they aren't tabulated as
headline numbers.

## Scope / caveats — read before quoting

- **2-vCPU VMs with a single NIC RX queue.** Both VPNs are gateway-CPU-bound and ingest on one
  core; absolute numbers reflect that ceiling, not the protocols' best case. On hardware with
  working multi-queue RSS, expect both to scale up.
- **Several tmasque datapath constants** (buffer sizes, datagram-queue depths, pacing floor) were
  tuned empirically and would need revisiting at much higher rates.
- Controlled lab path on one tenant network; no public-internet leg. Absolute numbers are
  environment-specific — treat them as a *relative* direct-vs-WG-vs-tmasque comparison.

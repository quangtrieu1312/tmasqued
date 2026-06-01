# Benchmarks — direct vs WireGuard vs tmasque

The full data behind the headline table in the [README](README.md#performance).

## How to read this

**The layers.** Traffic is wrapped twice on its way through the tunnel:

```
app data
  └─ inner IP packet        ← rides the TUN device; its size = the "inner MTU"
       └─ QUIC/UDP packet    ← the "outer" packet; crosses the WAN between client and gateway
```

The client wraps (app → inner → outer). The **gateway unwraps each inner packet on upload
and re-wraps it on download** — so the VPN's CPU cost is paid **per inner packet** (one
AEAD + datagram-queue op each). Therefore a **bigger inner MTU = fewer packets = less
gateway CPU per byte**, which is why a jumbo inner MTU roughly **doubles** the VPNs'
throughput but does nothing for **direct** (no per-packet userspace tax to amortize).

**MTU vs MSS.** MTU is the whole IP packet; MSS is the TCP payload only (`MSS = MTU − 40`
for IPv4/TCP). The two TCP tables below simply run the tunnel at two different MTUs.

**Fit the path — don't fragment.** The tunnel sizes its *outer* packet to the path MTU
(PMTUD is pinned, not dynamic). An outer packet too big for the path is **dropped**, not
fragmented — IPv6 routers may not fragment at all, and the IPv4 path sets DF — so a jumbo
inner MTU only works when the path genuinely carries the larger outer packet. (This is why
the server derives its packet size from the link MTU; an over-large pinned size breaks the
handshake rather than degrading gracefully.)

**Reading a cell.** Each cell is throughput in **Gbit/s**; `(gw N)` is the gateway **CPU%**
during the run. A pair `x / y` is the **two clients running concurrently** (to separate
targets). `direct` = the no-VPN baseline.

## Environment

5 VMs (AMD EPYC-Rome, **2 vCPU each**, kernel 6.12, 9000 B links). One is the VPN gateway
(runs the `tmasqued` container *or* the WireGuard server); two are clients; two are `iperf3`
targets, each reached *through* the tunnel. Measurement: `iperf3 -P8 -t10 -O2` (8 streams,
10 s, first 2 s discarded); `-R` for download; the 2-client rows run both clients at once to
different targets. The gateway is the shared bottleneck for either VPN here.

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

**The fairness result flips between the two MTUs.** At jumbo, tmasque splits the gateway
evenly across the two clients (1.38 / 1.38 = 2.76 agg) and edges WireGuard, which starves one
client (0.74 / 1.82 = 2.56). At a 1500 inner MTU it's the opposite: WireGuard is balanced and
leads on aggregate (0.87 / 0.75 = 1.62) while tmasque trails (0.66 / 0.69 = 1.35). tmasque's
even-split + aggregate edge is **specific to the jumbo inner MTU it's designed around**; at
the standard MTU the kernel datapath wins.

## Client-to-client (both endpoints are VPN clients; the gateway relays), inner MTU 1500

| direction | WireGuard | tmasque |
|---|--:|--:|
| TCP up   | **1.29** | 0.83 |
| TCP down | **1.22** | 0.89 |

WireGuard relays spoke↔spoke in-kernel; tmasque must decap from one client and **re-encapsulate
into the other client's QUIC tunnel** (double the userspace work), so it trails here. Getting
this working at all required a server fix — an `ipToTunChan` reconnect race was silently
black-holing peer delivery until guarded.

## UDP

`iperf3 -u -b 0` is a **flood, not a throughput measurement**, so we don't tabulate it. UDP has
no congestion control, so `-b 0` (no rate cap) just emits as fast as it can with zero backpressure;
the number you get is whatever survived the (large) loss. Worse, iperf3's UDP sender is
single-threaded and **CPU-bound generating packets** — that's why even *direct* reads ~4.4 Gbit/s
at jumbo but only ~2.3 Gbit/s at 1500 B (more, smaller packets = more sender CPU): it's measuring
the sender process, not the link. Pushed through a tunnel, the unmetered flood overruns the
datagram queue → heavy packet loss, and some runs don't even complete — for *both* WireGuard and
tmasque. (Reorder is irrelevant here: UDP has no CC and no retransmit, so out-of-order packets are
just counted, not punished — that only hurts TCP.) A meaningful UDP test would instead sweep `-b <rate>` and report the loss at
each step; we haven't, so there are no headline UDP numbers.

## Scope / caveats — read before quoting

- **2-vCPU VMs with a single NIC RX queue.** Both VPNs are gateway-CPU-bound and ingest on one
  core; the absolute numbers reflect that ceiling, not the protocols' best case. On hardware with
  more cores / working multi-queue RSS, expect both to scale up.
- **Several tmasque datapath constants** (buffer sizes, datagram-queue depths, pacing floor) were
  tuned empirically and would need revisiting at much higher rates.
- Controlled lab path on one tenant network; no public-internet leg. Absolute numbers are
  environment-specific — treat them as a *relative* direct-vs-WireGuard-vs-tmasque comparison.

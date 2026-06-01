# tmasqued — MASQUE VPN server (AF_XDP datapath)

`tmasqued` is the server side of a userspace VPN built on **MASQUE** (IP-over-HTTP/3,
[RFC 9484 CONNECT-IP](https://datatracker.ietf.org/doc/rfc9484/)). It terminates QUIC
connections from clients, assigns each a `/32` and a set of routes, and forwards their
traffic to the WAN — over a **kernel-bypass AF_XDP datapath**, **benchmarked head-to-head
against kernel WireGuard and a no-VPN direct baseline on a multi-Gbit/s testbed**
(single- and multi-client, both directions, at 1500 and jumbo MTU; see *Performance* for the
headline and [BENCHMARKS.md](BENCHMARKS.md) for the full matrix and honest scope).

> Client counterpart: [`tmasque`](https://github.com/quangtrieu1312/tmasque).
> Umbrella repo (setup, certs, management): [`masque-vpn`](https://github.com/quangtrieu1312/masque-vpn).

**Highlights**
- Userspace **MASQUE/QUIC VPN** (RFC 9484 CONNECT-IP) with a **kernel-bypass AF_XDP + eBPF
  datapath** and **reverse NAT done entirely in XDP** — the hot path never traverses the kernel
  network stack.
- On 2-vCPU VMs it lands **within low-single-digit Gbit/s of kernel WireGuard**, and — at the
  jumbo inner MTU it's built for — splits the gateway **evenly across concurrent clients** where
  WireGuard starves one.
- Traced a single-stream throughput **collapse to an AF_XDP-TX / ACK-clock timing root cause**
  and fixed it (bulk on AF_XDP, sparse feedback ACKs back through the kernel) — restoring
  single-stream download to parity.

---

## Why it's interesting

A conventional VPN like WireGuard lives in the kernel. Doing the same thing in
**userspace over QUIC** normally pays for it in throughput — every packet crosses the
user/kernel boundary, runs through the QUIC state machine, and rides an HTTP/3 datagram.
`tmasqued` closes that gap with two moves: it pulls packets off the NIC with **AF_XDP**
(kernel stack bypassed, and the return NAT runs in the XDP program itself), and it runs the
QUIC layer as a **CC-off datagram relay** so the inner TCP's own congestion control governs
the flow (no "TCP-over-TCP" collapse). How each works is in *Data plane & performance
engineering* below; how it measures up is in *Performance*.

---

## Performance

Head-to-head against a **no-VPN direct baseline** and **kernel WireGuard** on a 5-VM EPYC-Rome
(2 vCPU) testbed. Each cell is `iperf3 -P8 -t10` (**8 streams from one client**); 2-client cases
run two clients concurrently to separate targets. `gw` = gateway CPU%. The 1500-MTU table,
**client-to-client**, UDP, and full caveats: **[BENCHMARKS.md](BENCHMARKS.md)**.

### TCP at a jumbo inner MTU (Gbit/s, with gateway CPU%)

| case (8 streams) | direct | WireGuard | tmasque |
|---|--:|--:|--:|
| 1 client, up         | 21.5 (gw 0) | **3.35** (gw 82) | 2.05 (gw 66) |
| 1 client, down       | 21.5 (gw 0) | 1.85 (gw 65) | 1.75 (gw 61) |
| 2 clients, up (each) | 20.0 / 20.4 (gw 1) | 0.74 / 1.82 (gw 77) | **1.38 / 1.38** (gw 71) |
| 2 clients, down(each)| 20.4 / 20.3 (gw 1) | 0.57 / 1.25 (gw 68) | 0.98 / 0.88 (gw 65) |

**Reading it:**
- **A jumbo inner MTU ~doubles VPN throughput** (tmasque 1-client up 1.17→2.05, WG 1.66→3.35 vs
  the 1500 table) — it's tmasque's design point; direct is unaffected. (Why: see BENCHMARKS.md.)
- **At jumbo, tmasque is fairer under concurrency** — two clients split the gateway *evenly*
  (1.38 / 1.38, agg 2.76) and edge WG (0.74 / 1.82, agg 2.56), which starved one client. Per-flow-
  affine datagram pacing spreads load. **Honest caveat:** at a 1500 inner MTU this flips — WG is
  balanced and leads on aggregate (see BENCHMARKS.md); the edge is specific to the jumbo regime.
- **WireGuard is faster on a 1-client upload** (3.35 vs 2.05) and **direct is ~6–10×** either VPN —
  both are gateway-CPU-bound on 2 vCPU behind a **single NIC RX queue** (the real ceiling). A
  separate *single-flow* download collapse (war story below) was an AF_XDP-TX ACK-clock bug, fixed.

**Takeaway:** at its jumbo design point this userspace QUIC/MASQUE tunnel matches kernel
WireGuard's order of magnitude and is fairer across concurrent clients; at a 1500 inner MTU the
kernel datapath leads. Both sit well behind a direct path.

---

## Architecture

```
                                    tmasqued (server)
              +----------------------------------------------------------------------+
              |  xdp.c (eBPF/XDP) runs on every inbound frame and XDP_REDIRECTs      |
              |  it to an AF_XDP socket:  :443/QUIC -> QUIC xsk (upload);            |
              |  NAT-return -> in-kernel DNAT, fwd xsk (download)                    |
              |                                                                      |
client ------>+   :443 QUIC xsk -> connect-ip decap                                  |
(upload)      |                                      |                               |
              |                       dst == wanAddr / serverTunIP ?                 |
              |                        +-------------+-------------+                 |
              |                        |                           |                 |
              |                  yes (local)                 no (forward)            |
              |                        v                           v                 |
              |                    TUN dev                 SNAT (userspace)          |
              |                  (kernel deliver)                  |                 |
              |                                                  v                   |
              |                                          forward TX -----------------+-->  WAN
              |                                            bulk:  sendmmsg / XSK TX  |    (targets)
              |                                            sparse ACKs:  kernel sock |
              |                                                                      |
client <------+   QUIC TX  <-  re-encap     <-  in-kernel DNAT (xdp.c) <-------------+--  WAN
(download)    |     the DNAT'd inner IP is re-encapsulated as a                      |    (targets)
              |     QUIC DATAGRAM, sent down the client's own tunnel                 |
              +----------------------------------------------------------------------+
                control plane:  SQLite + Unix-socket mgmt API  (clients <-> roles <-> resources)
```

**Control plane.** A client's identity is the **Common Name of its mTLS cert** (= its DB
id). On connect, the server resolves the client's *roles → resources (CIDR prefixes)* and
advertises those as CONNECT-IP routes. You manage clients, roles, and resources with the
**`tmasquectl`** CLI (below) — the raw HTTP API and DB internals are documented in
[`src/README.md`](src/README.md) for contributors.

**Data plane.** See below.

---

## Data plane & performance engineering

| Piece | What & why |
|---|---|
| **AF_XDP ingest** | An eBPF/XDP program (`src/xdp/xdp.c`) classifies inbound frames at the NIC and `XDP_REDIRECT`s them to `xsk` sockets — `:443` → QUIC socket, NAT-return → forward socket. Busy-poll (`XDP_RX_POLL_MS=0`) removes vCPU deschedule/wake jitter that otherwise starves a single low-rate flow. |
| **In-kernel reverse NAT** | The **return path (DNAT)** runs entirely in the XDP program: it looks up a reverse-NAT BPF map, rewrites the destination IP+port back to the client, fixes the IP/TCP/UDP checksums incrementally (RFC 1624), and redirects to the AF_XDP socket — no kernel-stack traversal, no conntrack. The forward SNAT runs in userspace and populates that map. |
| **CC disabled, pacer-gated** | Post-handshake `SendMode = SendAny`; the cwnd controller is bypassed and a floored pacer is the sole send gate. Inner-TCP loss can't drag the tunnel rate to zero. |
| **IP-in-QUIC-DATAGRAM** | connect-ip context-0 framing over QUIC datagrams (unreliable). Forked quic-go gives a ring-buffered, drop-on-full datagram TX path (vs upstream's blocking single-slot queue). |
| **MTU / datagram budget** | `InitialPacketSize` pinned so the datagram payload budget fits the tunnel MTU — a misconfigured budget silently swallowed `DatagramTooLargeError` and produced **0** download until fixed. |
| **Inner-TCP buffer tuning** | The tunnel's added RTT enlarges the inner BDP; `tcp_wmem`/`tcp_rmem` are raised at bootstrap so a single inner stream isn't `sndbuf`-limited. |

### The single-stream-download fix (favorite war story)

A single-stream **download** collapsed to ~5–10% of WireGuard — with **no loss, no ECN, no
reorder, an open window, and *lower* RTT than WG**, so every "where's the throttle?" probe came
up empty. Root cause: the *sparse* return ACKs were going out **AF_XDP TX**, whose per-packet
timing jitter (no kernel qdisc) disturbed the **remote sender's ACK clock**, so it under-paced.
Fix: route just the small (<128 B) ACKs through a **kernel raw socket**, keep bulk on AF_XDP →
back to parity. Lesson: **AF_XDP wins for bulk; the kernel wins for sparse feedback packets.**

---

## Forked dependencies (`lib/`, git submodules)

| Submodule | Forked for |
|---|---|
| `quic-go` | CC-off dataplane (`SendMode`), pacer floor/burst knobs, ring-buffered drop-on-full DATAGRAM TX, datagram-queue use-after-free fix, instrumentation (expvar). |
| `connect-ip-go` | IP-packet (context-0) framing tuned for the datagram datapath. |
| `xdp` | `XDP_USE_NEED_WAKEUP` support on the TX path. |
| `water` | TUN with `IFF_VNET_HDR` + GSO/GRO offload split. |

---

## Build & run

```sh
cp tmasqued.conf.template tmasqued.conf      # fill in WAN_INTERFACE, TUNNEL_IP, CLIENT_CIDR, SANs…
sudo docker compose up --build -d            # builds the binary + eBPF, bootstraps certs, starts
```

Requires Linux, Docker, `NET_ADMIN` + `NET_RAW`, `/dev/net/tun`, and a NIC/driver that
supports XDP (native preferred; generic mode works). First boot auto-generates the server
and client CAs (Ed25519) and runs DB migrations.

Key tuning env (compose): `XDP_RX_POLL_MS=0` (busy-poll), `TUNNEL_PACING_FLOOR_MBIT`,
`XDP_NEED_WAKEUP`, `TUN_QUEUES`.

---

## Administration — `tmasquectl`

Everything is managed by **name** through one CLI run inside the container; run it with
no arguments for the full command list. The model is *clients → roles → resources (CIDR
routes)*: a client gets the union of the routes granted to its roles.

```sh
ctl() { sudo docker compose exec tmasqued tmasquectl "$@"; }

ctl client create alice                  # new client + its cert bundle + a default role "alice"
ctl resource create corp 10.0.0.0/8      # a named route
ctl role create eng                      # a role
ctl role assign eng corp                 # grant the route to the role
ctl client assign alice eng              # give the role to the client (multiple ok: ... eng ops)
ctl client resources alice               # what alice can now reach
ctl client rename alice alice2           # (also renames her default role)
ctl client delete alice2                 # (also deletes her default role)
```

Names are unique, so creating a duplicate is rejected. The cert bundle for a client is
written under `certs/client/<name>/`. For the raw HTTP API and DB internals, see
[`src/README.md`](src/README.md).

---

## Repository layout

```
src/
  main.go            QUIC listener, per-client tunChan map, forward consumer (SNAT + TX)
  xdp/               AF_XDP conn, eBPF loader, RX dispatch (QUIC vs NAT-return), session table
  utility/           ForwardBatch (AF_XDP/kernel TX split), SNAT, reseq, GSO helpers
  config db domain migration repository service   control plane (mTLS identity → routes)
lib/                 forked submodules (quic-go, connect-ip-go, xdp, water)
scripts/             cert bootstrap, gen_client, SNAT post-up/pre-down
Dockerfile docker-compose.yml
```

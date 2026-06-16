# tmasqued — MASQUE VPN server (AF_XDP datapath)

`tmasqued` is the server side of a userspace VPN built on **MASQUE** (IP-over-HTTP/3,
[RFC 9484 CONNECT-IP](https://datatracker.ietf.org/doc/rfc9484/)). It terminates QUIC
connections from clients, assigns each a `/32` and a set of routes, and forwards their
traffic to the WAN — over a **kernel-bypass AF_XDP datapath**, **benchmarked head-to-head
against kernel WireGuard and a no-VPN direct baseline** (single- and multi-client, both
directions, at 1500 and jumbo MTU). See *Performance* below for the headline, and
[**tmasque-bench**](https://github.com/quangtrieu1312/tmasque-bench) for the full per-regime grid and honest scope.

> Client counterpart: [`tmasque`](https://github.com/quangtrieu1312/tmasque).
> Umbrella repo (setup, certs, management): [`masque-vpn`](https://github.com/quangtrieu1312/masque-vpn).

---

## Why it's interesting

A conventional VPN like WireGuard lives in the kernel. Doing the same thing in
**userspace over QUIC** normally pays for it in throughput — every packet crosses the
user/kernel boundary, runs through the QUIC state machine, and rides an HTTP/3 datagram.
`tmasqued` closes that gap with two moves: it pulls packets off the NIC with **AF_XDP**
(kernel stack bypassed, and the return NAT runs in the XDP program itself), and it runs the
QUIC layer as a **CC-off datagram relay** so the inner TCP's own congestion control governs
the flow (no "TCP-over-TCP" collapse). The *Architecture* diagram below shows how the pieces
fit; how it measures up is in *Performance*.

---

## Performance

How much aggregate traffic the gateway forwards, at the production **RSS** (multi-queue) setting.
The full matrix — both directions, all three NIC-steering settings, single- and multi-client — is
in **[tmasque-bench](https://github.com/quangtrieu1312/tmasque-bench)**.

**How to read:** every cell is **aggregate goodput in Gbit/s** — the useful TCP rate the receivers
actually get, summed over all clients — for an **upload** run at **RSS** (higher is better). Columns:

- **tmasque** — this VPN (AF_XDP datapath, shipped `gso` default)
- **wg-k** / **wg-u** — kernel WireGuard / userspace WireGuard (`wireguard-go`)
- **ovpn-k** / **ovpn-u** — OpenVPN with the in-kernel DCO module / its userspace data channel
- **c→GW** / **GW→t** — the raw network legs with every VPN *off* (client→gateway / gateway→target); reference ceilings (16-core only)
- **`—`** — not measured (OpenVPN wasn't run on the 2-core set)

**16-core gateway, 7 clients:**

| inner MTU | c→GW | GW→t | tmasque | wg-k | wg-u | ovpn-k | ovpn-u |
|---|--:|--:|--:|--:|--:|--:|--:|
| 1500         | 64.1 | 90.9 | 5.0 | 9.5  | 2.3 | 2.0 | 0.4 |
| 9000 (jumbo) | 62.4 | 87.0 | 9.5 | 22.6 | 5.2 | 6.3 | 2.2 |

**2-core gateway, 2 clients:**

| inner MTU | tmasque | wg-k | wg-u | ovpn-k | ovpn-u |
|---|--:|--:|--:|--:|--:|
| 1500         | 2.0 | 2.4 | 4.8 | 1.2 | 0.3 |
| 9000 (jumbo) | 3.5 | 5.7 | 8.9 | 3.7 | 1.0 |

tmasque's AF_XDP-native path caps the jumbo *outer* MTU at 3506 (inner 3422 — `virtio_net` won't
attach XDP above 3506), vs kernel WireGuard's full 8920.

**Takeaway:** on the 16-core box **kernel WireGuard leads at RSS** (it spreads across cores natively),
while tmasque reaches ~10 G at jumbo and beats userspace WireGuard and both OpenVPN variants — all far
below the ~62 G raw client→GW underlay, so the gap is userspace/crypto overhead, not the fabric. On
the 2-core box tmasque trails both WireGuard variants (userspace WireGuard is fastest) — that small
box is client/CPU-bound, not server-bound. Per-flow rate is inner-TCP-loss-bound; the
single-RX-queue funnel and per-scenario breakdown are in tmasque-bench.

> **RX-queue scaling.** tmasque binds **one AF_XDP `xsk` per NIC RX queue** (the eBPF redirects by
> `ctx->rx_queue_index`), so with a multi-queue NIC (RSS) distinct client flows hash to distinct
> queues → cores — the same way kernel WireGuard's softirq spreads. With a single RX queue both
> serialize through one ring; RPS can't help tmasque (its AF_XDP path bypasses the kernel softirq).
> The full none/rps/rss progression is in [tmasque-bench](https://github.com/quangtrieu1312/tmasque-bench).

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
              |                                                    v                 |
              |                                              forward TX -------------+-->  WAN
              |                                            bulk:  GSO into main tun  |    (targets)
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

## The single-stream-download fix (postmortem)

A single-stream **download** collapsed to ~5–10% of WireGuard — no loss, no ECN, no reorder, an
open window, and *lower* RTT than WG, so every "where's the throttle?" probe came up empty. We sent
the return ACKs over **AF_XDP TX** both batched-with-`flush()` and send-immediately — neither
helped — and tried a homegrown pacer (a `flush()` timer), which eased the symptom but **dragged
down the other benchmarks**. What worked: route just the small (<128 B) ACKs through a **kernel raw
socket**, so the kernel **qdisc** paces only the feedback while bulk stays on AF_XDP. We never fully
isolated the mechanism — pacing is the suspect — but this restored parity with no collateral damage.
Lesson: **AF_XDP for bulk; the kernel path for sparse feedback.**

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

Requires Linux, Docker, `/dev/net/tun`, and a NIC/driver that supports XDP (native preferred;
generic mode works). The compose runs the container **privileged** (`NET_ADMIN` + `NET_RAW` +
`SYS_ADMIN` — for the TUN device, the raw-socket ACK path, and eBPF/AF_XDP map management). First boot auto-generates the server
and client CAs (Ed25519) and runs DB migrations.

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

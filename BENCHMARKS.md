# Benchmarks — direct vs WireGuard vs tmasque

**Max throughput a VPN gateway forwards** (client → gateway → external target), measured with `iperf3`,
for two gateway sizes: a big **8-core** server and a small **2-core** VPS. The full per-regime
(none / RPS / RSS), per-scenario grid with CPU is in **[FULL_MATRIX.md](FULL_MATRIX.md)**.

## How it's measured

Traffic is wrapped twice on its way through the tunnel:

```
app data
  └─ inner IP packet      ← rides the TUN device; its size is the "inner MTU"
       └─ QUIC/UDP packet  ← the "outer" packet on the WAN, between client and gateway
```

The gateway unwraps every inner packet on upload and re-wraps it on download, so a VPN's CPU cost is paid
**per inner packet** — a bigger inner MTU = fewer packets = less CPU per byte. That's why a jumbo inner
MTU roughly **doubles** VPN throughput but does nothing for direct (no per-packet tax to amortize).

```
forward  (what these tables measure — the real gateway-VPN path):
    client ──tunnel──▶ gateway ──(decrypt + SNAT)──▶ target   ← iperf3 -s here

p2p  (tunnel-terminate — shown for contrast, NOT measured here):
    client ──tunnel──▶ gateway   ← iperf3 -s on the gateway's own tunnel IP;
                                   the packet is delivered locally, never forwarded out
```

- `iperf3 -P8 -t10 -O2` (8 parallel streams). **Upload** = client→target (iperf3's default); **download** = add `-R` (target→client).
- **Max** = best aggregate for that direction (8-core: multi-queue **RSS**, 6 clients; 2-core: single RX queue, 1–2 clients).
- **tmasque's "9000" is really 3506.** `virtio_net` caps an XDP-native attach at a 3506-byte link MTU, so tmasque's outer packet is 3506 (inner 3422) while WireGuard uses the full 8920.
- **UDP is offered-rate-bounded** (`-b1G×2`/client) — figures are "carried under a fixed offered load," not a hard ceiling (a `-b0` flood just collapses the tunnel; see FULL_MATRIX).

## 8-core gateway (multi-queue / RSS) — max Gbit/s

| MTU 9000 | TCP ↑ | TCP ↓ | UDP ↑ | UDP ↓ |
|---|--:|--:|--:|--:|
| direct (no VPN) | 61.6 | 51.0 | 8.5 | 11.7 |
| WireGuard | 7.8 | 9.6 | 7.1 | 9.4 |
| tmasque (→ 3506) | 7.8 | 7.7 | 4.5 | 6.5 |

| MTU 1500 | TCP ↑ | TCP ↓ | UDP ↑ | UDP ↓ |
|---|--:|--:|--:|--:|
| direct | 60.2 | 51.9 | 2.2 | 7.4 |
| WireGuard | 6.4 | 8.8 | 2.5 | 2.8 |
| tmasque | 4.0 | 4.2 | 3.4 | 2.8 |

## 2-core gateway (single RX queue, small VPS) — max Gbit/s

TCP only — this older small-gateway testbed predates the clean UDP method and its UDP runs were
unreliable (omitted). Per-client breakdown + client-to-client are in FULL_MATRIX. **direct** here is a
**single client's** wire speed (it doesn't traverse the gateway); the VPN rows are the gateway's best
aggregate (often a single client already saturates the 2-core gateway).

| MTU jumbo (≤ 3506) | TCP ↑ | TCP ↓ |
|---|--:|--:|
| direct (1 client) | 21.5 | 21.5 |
| WireGuard | 3.4 | 1.9 |
| tmasque | 2.8 | 1.9 |

| MTU 1500 | TCP ↑ | TCP ↓ |
|---|--:|--:|
| direct (1 client) | 22.1 | 22.5 |
| WireGuard | 1.7 | 1.9 |
| tmasque | 1.4 | 0.9 |

## Bottom line

- **At jumbo, tmasque ≈ WireGuard** on the 8-core gateway (~8 G TCP either way); **at 1500 WireGuard leads** — its in-kernel datapath is more per-packet-efficient at small MTU.
- **Neither is underlay-bound** — direct is 50–62 G. The limits are the gateway NIC's RX-queue → core mapping (*Why RSS, not RPS* in FULL_MATRIX) and the single 4-core target's RX queue.
- **On a 2-core VPS the gateway CPU is the wall** — both VPNs land at ~1–3 G; a jumbo inner MTU helps most.

Scope, the none/RPS/RSS regime grid, the UDP-flood known issue, and CPU figures: **[FULL_MATRIX.md](FULL_MATRIX.md)**.

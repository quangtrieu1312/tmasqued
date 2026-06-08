<!-- ⚠️ SCRATCH / DO NOT PUBLISH ⚠️
     Temporary 6.17 re-baseline capture. NOT for master. Delete before merging. -->

# 6.17 re-baseline (SCRATCH — do not publish)

Testbed: **Ubuntu 24.04.4 / kernel 6.17.0-35-generic** (was 6.8.0). On 6.17 virtio_net supports
AF_XDP **zero-copy** (merged v6.11); tmasque binds native `flags=0`, so xsk RX/TX is now true
zero-copy DMA instead of the 1-copy fallback used on 6.8. No code change (the bind already
defaulted to ZC-if-available).

Captured 2026-06-08. Stack = **tmasque**, forward = **GSO** (`FORWARD_TUN_GSO=1`), TCP,
**WAN 9000 (inner 3422)**, **`iperf3 -P8 -t15 -O2`**, 6 clients (1×4-core Ubuntu + 5×2-core Alpine)
→ single 4-core target .5.80. Cell = **agg Gbit · GW core-equivalents /8**.

> ⚠️ **Fabric incident (read this):** the FIRST 6.17 run looked like a regression (all-up rss 4.22,
> 1-up 1.6). Root cause was a **degraded OpenStack/KVM underlay** — raw no-VPN client→target was
> **1.7 G with 188k retransmits** (vs the ~50 G baseline). Restarting the VMs cleared it (raw
> client→target recovered to **9.8–27 G, retr → ~200**). The numbers below are the **post-restart,
> healthy-fabric** run. The GW VM itself was always clean (0% CPU steal, AES-NI, kvm-clock).

## tmasque (GSO forward), TCP 9000, P8 — kernel 6.17, HEALTHY fabric
| scenario | none (comb1,rps00) | rps (comb1,rps=ff) | rss (comb8) |
|---|--:|--:|--:|
| 1 upload   | 3.23 · 3.4 | 3.15 · 0.0 | 3.20 · 0.1 |
| 1 download | 1.67 · 2.1 | 1.77 · 2.2 | 1.69 · 2.2 |
| half (3u+3d) | 5.19 · 4.4 | 5.31 · 4.3 | **8.31 · 5.7** |
| all upload | 4.65 · 4.6 | 4.65 · 4.5 | **8.66 · 5.7** |
| all download | 3.20 · 3.7 | 2.94 · 3.4 | 6.57 · 4.8 |

### Reference — published 6.8 table (tmasque XDP, TCP 9000, FULL_MATRIX.md)
| scenario | 6.8 none | 6.8 rps | 6.8 rss |
|---|--:|--:|--:|
| 1 upload   | 2.74 | 2.82 | 3.19 |
| 1 download | 2.52 | 2.53 | 2.62 |
| half       | 2.73 | 2.72 | 7.98 |
| all upload | 2.77 | 3.06 | 7.81 |
| all download | 2.69 | 2.85 | 7.73 |

## Read
- **6.17 ≈ or > 6.8 on a healthy fabric.** rss all-up **8.66 vs 7.81**, rss half **8.31 vs 7.98**;
  none all-up **4.65 vs 2.77** and none half **5.19 vs 2.73** (the single-queue funnel regime, where
  zero-copy removing the per-frame copy should help most). NOT the regression the broken-fabric run
  suggested.
- **Caveats remain:** still not a fully controlled A/B (different forward path vs the 6.8 default-path
  table; fleet rebooted/re-provisioned). Treat ±10–15% as noise. The none-regime gain is suggestive
  of the ZC win but unproven without a same-path 6.8 boot to compare.
- 1-download is the soft cell (~1.7 G) — download = GW QUIC-outer-TX through the shared single
  AF_XDP TX ring; doesn't RSS-scale the way forward/GSO does.

## Loss vs reorder instrumentation (Retr + UDP OOO) — ★ KEY FINDING

Instrumented probe (rss/gso, P8 TCP / P2-b800M UDP):
| cell | agg | Retr (loss) | OFO | UDP OOO |
|---|--:|--:|--:|--:|
| TCP all-up   | 1.23 G | **533,830** | 59.2% | — |
| TCP all-down | 3.91 G | 23,923 | **0.0%** | — |
| UDP all-up   | 4.60 G | — | — | **0 of 5.3M** |

**There is NO genuine reorder.** UDP out-of-order = 0 (confirmed twice). OFO% tracks Retr exactly
(high Retr → high OFO on up; ~0 Retr → 0% OFO on down). So every "OFO" number in this repo's history
is **loss-induced**, not reordering. The reorder-mitigation work (RESEQ, reorder-safe parallelism)
was treating a symptom of loss.

**CORRECTION (2026-06-08): the loss is the DATAPATH, not the fabric. My "lossy direct fabric" tests were
a measurement error.** The client policy-routes target-bound traffic through `tun0` (`.5.80` is a tunnel
resource), so `iperf3 -c .5.80` with tmasque UP measured the VPN, not the fabric. PROVEN on .122:
tmasque up → route `dev tun0`, iperf3 3.18 G; tmasque killed → route `dev ens3`, iperf3 **41.2 G**. The
two GENUINE direct tests (tmasque down) were both clean (26.9 G, 41.2 G). **The fabric is fine (~41 G).**

⇒ VPN throughput IS variable (8.66 G matrix vs 1.23 G probe minutes apart) — source unresolved, but it is
NOT the raw TCP fabric. The "per-flow inner-TCP loss wall" STANDS as a real datapath property. And
`UDP OOO = 0` (twice) confirms the OFO is **loss-induced, not reorder**. To measure true direct fabric:
kill tmasque on the client, or test from the GW (no tunnel on its own traffic), or hit a non-resource dest.

## Method note
- Earlier "GW 0.74/8 cores" ZC-win claim was a measurement artifact (mpstat overlapped deploy churn).
  Clean P8 GW load is ~5.7/8 in rss.
- OOO% in these tables = target `nstat TCPOFOQueue/TcpInSegs` — which **conflates loss-induced OFO
  with genuine reorder**. The instrumented section above uses Retr (loss) + UDP OOO (reorder) to
  separate them.

<!-- END SCRATCH — delete this file before merging to master -->

#!/usr/bin/env python3
# Collate direct/wg/tmasque tsv -> matrix-results/MATRIX_FULL.md
import os
BENCH=os.path.dirname(os.path.abspath(__file__))
RES=os.path.join(BENCH,"results")
OUT=os.path.join(os.path.dirname(BENCH),"FULL_MATRIX.md")

def load(name):
    d={}
    p=os.path.join(RES,name)
    if not os.path.exists(p): return d
    for line in open(p):
        f=line.rstrip("\n").split("\t")
        if len(f)<8 or f[0]=="stack": continue
        stack,regime,mtu,proto,scen,agg,gw,loss=f[:8]
        d[(regime,mtu,proto,scen)]=(agg,gw,loss)
    return d

SC=["1up","1down","half","allup","alldown"]
SCL={"1up":"1 upload","1down":"1 download","half":"half (3 up + 3 down)","allup":"all upload","alldown":"all download"}

def cell(d,regime,mtu,proto,scen,udp=False):
    v=d.get((regime,mtu,proto,scen))
    if not v: return "–"
    agg,gw,loss=v
    if agg=="COLLAPSE": return "**collapse**¹"
    s=f"{agg} ·{gw}"
    if udp and loss not in("-","",None): s+=f" ·{loss}%"
    return s

def vpn_tables(name,d):
    out=[f"## {name}\n",
         "Throughput **Gbit/s** · gateway **cores busy /8**" ,
         "(UDP adds **· loss%**; UDP is a fixed `-b1G×2`/client offered-rate stress probe — see note ¹).\n"]
    for proto,plabel,udp in (("tcp","TCP",False),("udp","UDP (offered `-b1G×2`/client)",True)):
        for mtu in ("9000","1500"):
            mlabel=mtu
            if name.startswith("tmasque") and mtu=="9000": mlabel="9000 underlay (outer capped 3506 → inner 3422)"
            elif mtu=="9000": mlabel="9000 (inner 8920)"
            else: mlabel="1500 (inner 1416/1420)"
            out.append(f"### {plabel} — WAN {mlabel}")
            out.append("| scenario | none (1 RX q) | RPS (1 q + rps) | RSS (8 q) |")
            out.append("|---|--:|--:|--:|")
            for scen in SC:
                row=[SCL[scen]]
                for regime in ("none","rps","rss"):
                    row.append(cell(d,regime,mtu,proto,scen,udp))
                out.append("| "+" | ".join(row)+" |")
            out.append("")
    return "\n".join(out)

direct=load("direct.tsv"); wg=load("wg.tsv"); tmq=load("tmasque.tsv")

L=[]
L.append("# Full benchmark matrix — RSS vs RPS vs none, per scenario/MTU/protocol")
L.append("")
L.append("Regime = gateway NIC config: **none** = `Combined=1` (single RX queue, no RPS); "
         "**RPS** = `Combined=1` + `rps_cpus=ff`; **RSS** = `Combined=8` (8 HW RX queues, 1/core). "
         "5 scenarios × {9000,1500} WAN MTU × {TCP,UDP} × {none,RPS,RSS}, on the 8-core gateway + "
         "6 clients (1× 4-core + 5× 2-core) → a 4-core target. Each TCP cell = `iperf3 -P8 -t10 -O2`; "
         "agg = Σ the participating clients' receiver Gbit/s; `cores` = GW busy core-equivalents of 8.")
L.append("")
# how to read a cell
L.append("## How to read a cell")
L.append("")
L.append("Cells pack throughput, gateway CPU and (for UDP) loss, separated by `·`:")
L.append("")
L.append("- **TCP** `7.78 ·6.48` = **7.78 Gbit/s** · **6.48 of 8** gateway cores busy.")
L.append("- **UDP** `7.07 ·5.74 ·41.2%` = 7.07 Gbit/s · 5.74/8 cores · **41.2% loss**.")
L.append("- **Direct rows are special:** just `42.83` (TCP) or `2.00 ·0.0%` (UDP, throughput·loss) — "
         "**no `·cores`**, because direct traffic never touches the gateway.")
L.append("- `collapse` = the UDP-flood known-issue cell (see Notes).")
L.append("- **Columns = NIC regime** (none / RPS / RSS). **Rows = scenario:** `1 upload`/`1 download` = "
         "one client (iperf3 default = upload; `-R` = download); `half` = 3 up + 3 down; "
         "`all upload`/`all download` = all 6 clients one way.")
L.append("")
# direct
L.append("## Direct — RSS-enabled testbed, single config (wire/medium reference)")
L.append("")
L.append("Direct is client→target over the fabric — it **does NOT traverse the gateway** (gw≈0), so the "
         "regime is irrelevant to it. Measured once, on the RSS testbed, purely to show the underlay is "
         "excellent and not the bottleneck. (MTU axis = MSS/datagram-size clamp.) "
         "Cells = throughput (TCP) or throughput·loss% (UDP) — no `·cores`.")
L.append("")
L.append("| scenario | TCP 9000 | TCP 1500 | UDP 9000 | UDP 1500 |")
L.append("|---|--:|--:|--:|--:|")
for scen in SC:
    def dc(mtu,proto,udp):
        v=direct.get(("direct",mtu,proto,scen))
        if not v: return "–"
        agg,gw,loss=v
        return f"{agg}"+(f" ·{loss}%" if udp and loss not in('-','') else "")
    L.append(f"| {SCL[scen]} | {dc('9000','tcp',0)} | {dc('1500','tcp',0)} | {dc('9000','udp',1)} | {dc('1500','udp',1)} |")
L.append("")
L.append(vpn_tables("WireGuard (kernel wg0)",wg))
L.append(vpn_tables("tmasque (AF-XDP / QUIC-MASQUE)",tmq))

# why RSS, not RPS (the headline insight)
L.append("## Why RSS, not RPS")
L.append("")
L.append("Read down the `none → RPS → RSS` columns for the multi-client rows (`half`, `all upload`, "
         "`all download`). On this 8-core box:")
L.append("")
L.append("- **`none` (Combined=1) funnels both** — a single RX queue caps WireGuard at ~4.4 G and tmasque "
         "at ~2.8 G (TCP 9000), and more clients don't help. The gateway is far from CPU-saturated "
         "(~3–4 of 8 cores busy in aggregate) — the limiter is the single RX queue itself (RSS, below, "
         "removes it), not total CPU.")
L.append("- **RPS does not un-funnel either, here.** RPS only re-spreads the kernel RX *softirq* across "
         "cores, and that helps only when the softirq core is the bottleneck — at ~4.4 G on this fast box "
         "it isn't, so WireGuard stays ~flat (`all upload` 4.35 → 4.69). tmasque's AF-XDP datapath "
         "**bypasses the kernel softirq entirely**, so RPS can *never* move it, on any box. _(On an "
         "earlier, slower softirq-bound gateway where WG's RX core was pegged at 100%, RPS did 2× kernel-"
         "WireGuard — the classic result, just not reproduced on this faster hardware.)_")
L.append("- **RSS is the actual un-funnel — for both (~2×).** 8 hardware RX queues IRQ-pinned per core: "
         "WG's softirq spreads natively, and tmasque binds **one `xsk` per RX queue** (eBPF redirects by "
         "`ctx->rx_queue_index`), so distinct client flows hash to distinct queues → cores (~6 of 8 busy). "
         "Single-client rows don't benefit — one flow → one queue.")
L.append("")
# 2-core gateway detail (older EPYC testbed; static historical data)
L += [
"## 2-core gateway — detail (small VPS, single RX queue)",
"",
"Older testbed: 5 VMs (AMD EPYC-Rome, **2 vCPU each**, 9000 B links, single NIC RX queue). One gateway "
"(tmasqued *or* WireGuard), two clients, two `iperf3` targets reached *through* the tunnel. The 2-vCPU "
"gateway is the shared bottleneck. `(gw N)` = gateway CPU% on the 2-vCPU box.",
"",
"### TCP — jumbo tunnel (inner ~3398 B)",
"| case | direct | WireGuard | tmasque |",
"|---|--:|--:|--:|",
"| 1 client, up | 21.5 (gw 0) | **3.35** (gw 82) | 2.05 (gw 66) |",
"| 1 client, down | 21.5 (gw 0) | 1.85 (gw 65) | 1.75 (gw 61) |",
"| 2 clients, up (each) | 20.0 / 20.4 | 0.74 / 1.82 | **1.38 / 1.38** |",
"| 2 clients, down (each) | 20.4 / 20.3 | 0.57 / 1.25 | 0.98 / 0.88 |",
"",
"### TCP — standard tunnel (inner ~1500 B)",
"| case | direct | WireGuard | tmasque |",
"|---|--:|--:|--:|",
"| 1 client, up | 22.1 | 1.66 | 1.17 |",
"| 1 client, down | 22.5 | 1.81 | 0.87 |",
"| 2 clients, up (each) | 21.1 / 21.6 | 0.87 / 0.75 | 0.66 / 0.69 |",
"| 2 clients, down (each) | 20.2 / 20.1 | 0.77 / 1.16 | 0.46 / 0.47 |",
"",
"**Fairness flips with MTU:** at jumbo tmasque splits the gateway evenly across two clients "
"(1.38/1.38 = 2.76) and edges WG, which starves one (0.74/1.82 = 2.56); at 1500 it reverses "
"(WG 0.87/0.75 = 1.62 balanced, tmasque 0.66/0.69 = 1.35).",
"",
"### Client-to-client (both endpoints are VPN clients; gateway relays), inner 1500",
"| direction | WireGuard | tmasque |",
"|---|--:|--:|",
"| TCP up | **1.29** | 0.83 |",
"| TCP down | **1.22** | 0.89 |",
"",
"WG relays spoke↔spoke in-kernel; tmasque decaps from one client and **re-encapsulates into the "
"other's QUIC tunnel** (double the userspace work), so it trails. _(2-core UDP runs on this older "
"testbed predate the clean method and were unreliable — omitted.)_",
"",
]
# notes
L.append("## Notes")
L.append("")
L.append("**¹ UDP is an offered-rate stress probe, not a clean-rate sweep.** Each client offers `-b1G -P2` "
         "(up to ~12 G for a 6-client run) regardless of capacity, so every multi-client UDP cell shows heavy "
         "loss by construction. The headline UDP number is the **peak carried**, below.")
L.append("")
L.append("**Peak UDP carried:** **direct 11.7 G@2.4% · WireGuard 9.4 G@21% · tmasque 6.5 G@40%** (all RSS/9000). "
         "Clean low-loss carry ≈ 0.6 G@0.036% (solo `-b300M`).")
L.append("")
L.append("**⚠ Known issue — UDP-flood saturation (the `collapse`/0.00 cell, none+1500+UDP+multi-client).** "
         "*Proven:* under the flood the tmasque tunnel saturates — ping through it hits 66% loss to the GW and "
         "100% to the target — and **fully recovers within ~4 s** of the load stopping (not a crash). The GW is "
         "**not** spinning/deadlocked (57% idle, still forwards 5–6 GB). *Suspected (not proven):* the single-queue "
         "userspace QUIC-datagram path has no fair-queueing/AQM, so one UDP flood monopolizes the xsk and starves "
         "everything (ping, control, other flows) — and the exact reason iperf3 then logs 0.00 is unverified. "
         "*The real gap:* **WireGuard degrades gracefully here (kept ~1.1 G); tmasque does not.** Tracked as a "
         "known issue, not dismissed. (tmasque is fine at solo/2-client, gentler rates, and all TCP.)")
L.append("")
L.append("**tmasque's \"9000\" is really 3506.** `virtio_net` rejects an XDP-native attach above a 3506 B "
         "link MTU, so on the 9000 underlay tmasque's outer packet is capped at 3506 (inner 3422) while "
         "WireGuard runs the full 8920.")
L.append("")
L.append("**Why UDP is bounded (`-b1G×2`, not `-b0`).** `iperf3 -u -b0` is a flood, not a measurement: "
         "UDP has no congestion control, so it emits as fast as the (single-threaded, CPU-bound) sender "
         "allows. Through a tunnel an unmetered flood overruns the datagram queue → heavy loss, and on the "
         "QUIC tunnel it drops the connection outright. A bounded offered rate keeps the test meaningful; "
         "a true UDP ceiling test would sweep `-b<rate>` and report loss at each step.")
L.append("")
L.append("**Scope.** Cloud virtio NICs on one controlled tenant network (no public-internet leg) — "
         "absolute numbers are environment-specific; the comparisons (WG vs tmasque, regime, MTU) are the "
         "point. The 8-core run's single 4-core target caps aggregate at ~9–10 G. Treat ±10–15% as noise.")
open(OUT,"w").write("\n".join(L)+"\n")
print("wrote",OUT,"(",len(L),"lines )")

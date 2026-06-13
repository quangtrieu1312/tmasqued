//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"expvar"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	connectip "github.com/quic-go/connect-ip-go"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/sys/unix"

	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
	"github.com/yosida95/uritemplate/v3"

	"github.com/quangtrieu1312/tmasqued/config"
	"github.com/quangtrieu1312/tmasqued/constants"
	"github.com/quangtrieu1312/tmasqued/db"
	"github.com/quangtrieu1312/tmasqued/logger"
	"github.com/quangtrieu1312/tmasqued/migration"
	"github.com/quangtrieu1312/tmasqued/service"
	"github.com/quangtrieu1312/tmasqued/stats"
	"github.com/quangtrieu1312/tmasqued/utility"
	xdp "github.com/quangtrieu1312/tmasqued/xdp"
)

var tunTapDevice []*water.Interface
var mu *sync.RWMutex

// ipToTunChan maps a client's inner IP to its bonded tunnels' tunChans, indexed
// by tunnel index. A download packet is pinned to one tunnel by 5-tuple hash
// (ECMP/LACP-style) so a flow never crosses tunnels (no cross-tunnel reorder)
// while different flows spread across tunnels/cores.
var ipToTunChan map[netip.Addr][]chan *utility.Packet

// afxdpConn is the PRIMARY NIC's AF_XDP Conn (the QUIC-listen / WAN NIC). It drives
// the existing WAN forward (branch-4) and handleConn. Phase 2 makes branch-3 pick a
// per-egress-NIC Conn from egressNICs.
var afxdpConn *xdp.Conn

// nicEgress is one real NIC's datapath: its AF_XDP Conn and the IPv4 used as the
// SNAT source for traffic egressing it (the rev-NAT table is keyed by this IP, so
// flows out different NICs coexist).
type nicEgress struct {
	name     string
	conn     *xdp.Conn
	addr     netip.Addr
	combined int
}

// egressNICs holds every real NIC's datapath (one per attached NIC). Used to pick
// the right egress for a destination in the multi-NIC forward fast path.
var egressNICs []nicEgress

// xdpLoaderRef / xdpLoadersRef let GracefullyShutDown detach the XDP bpf_link(s)
// before restoring NIC MTUs (virtio_net rejects MTU > 3506 while XDP-native is
// attached). xdpLoaderRef is the primary (back-compat); xdpLoadersRef is all NICs'.
var xdpLoaderRef *xdp.Loader
var xdpLoadersRef []*xdp.Loader

// wanIfaceName is captured at startup (the signal-handler goroutine's ctx is the
// pre-config.Load one, so it can't read WAN_INTERFACE from ctx). It is the primary
// (QUIC-listen) NIC's name.
var wanIfaceName string
var serverTunIP netip.Addr
var tunChanDrops atomic.Uint64
var pktChanDrops atomic.Uint64

// virtPrefix is the VPN's virtual subnet (VIRT_CIDR) as a netip.Prefix, used by
// the forward router to recognise client-to-client traffic. localLANs are the
// directly-connected non-WAN/non-tun subnets the server bridges its clients onto.
// Both set once at startup, read-only on the forward hot path.
var virtPrefix netip.Prefix
var localLANs []netip.Prefix

// lanRoute pairs a directly-connected LAN subnet with the egress NIC (index into
// egressNICs) that owns it, for the multi-NIC LAN fast path (forwardOne branch-3).
// egIdx >= 0  → that real NIC's AF_XDP/XDP datapath: userspace SNAT to its IP +
//               its XDP NAT-reverse for the return (the WAN's treatment).
// egIdx == -1 → no XDP NIC owns the subnet (docker0, a coexisting VPN's tun) →
//               transparent kernel bridge (ip_forward + masquerade), as before.
// Built once at startup after egressNICs; read-only on the forward hot path.
type lanRoute struct {
	prefix netip.Prefix
	egIdx  int
}

var lanRoutes []lanRoute

// Forward-router counters (served at /debug/vars):
//
//	c2c_sends     = inner packets delivered straight down a peer's tunnel (no SNAT)
//	c2c_no_peer   = vIP destination with no live tunnel (dropped, not leaked to WAN)
//	lan_forwards  = packets bridged out a local LAN NIC (no SNAT)
var (
	c2cSends    = expvar.NewInt("c2c_sends")
	c2cNoPeer   = expvar.NewInt("c2c_no_peer")
	lanForwards = expvar.NewInt("lan_forwards")
)

// fwdAddDrops counts forward packets the batch refused to enqueue (the
// ForwardBatch.Add return value was previously ignored → silent drop). Served
// at /debug/vars as fwd_add_drops.
var fwdAddDrops = expvar.NewInt("fwd_add_drops")

// snatFailDrops counts forward packets dropped because SNAT failed (the
// ApplySNAT return was previously ignored → packet forwarded UN-SNAT'd → target
// replies to the unroutable client vIP → no ACK → silent inner-TCP loss). Now we
// drop+count instead. Served at /debug/vars as snat_fail_drops.
var snatFailDrops = expvar.NewInt("snat_fail_drops")

// serverInitialPacketSize is the QUIC outer-packet size, derived at startup from
// the WAN link MTU (NOT hardcoded) so the tunnel adapts to the underlay: a 1500
// link -> ~1472, the XDP-native virtio cap 3506 -> ~3478. Pinned because PMTUD is
// disabled. Hardcoding a jumbo value here would fragment/drop on a 1500/1G link.
var serverInitialPacketSize int = 1452

const (
	// xdpNativeMaxMTU mirrors scripts/bootstrap/004: virtio_net rejects MTU > 3506
	// for XDP native mode, so the tunnel can't use an outer packet bigger than this
	// regardless of what the underlay link advertises.
	xdpNativeMaxMTU = 3506
	// outerOverhead sizes the QUIC packet so the resulting IP packet (IPS+IP20+UDP8)
	// stays SAFELY under the WAN MTU. = 28 (IP+UDP) + 28B path-safety margin: a real
	// path can carry slightly less than the link MTU (OpenStack VXLAN etc.), and an
	// IP packet exactly == MTU failed the handshake here (IPS=MTU-28 dropped).
	outerOverhead = 56
	// datagramOverhead is the QUIC short header + pktnum + AEAD(16) + connect-ip
	// ctx/seq above the inner IP packet, so the inner packet fits one DATAGRAM.
	// MEASURED on the wire: outer UDP payload = inner+28. Was 52 (over-reserved 24B).
	datagramOverhead = 28
	// maxQUICPacket == quic-go protocol.MaxPacketBufferSize in our fork (buffer cap).
	maxQUICPacket = 4000
)

// tunLoopSends counts download packets pushed into tunChan via the TUN-read
// loop (XDP_PASS → kernel → TUN fallback, the ipToTunChan/pickTunnel path).
// If this is nonzero during a download test, a flow is split between this
// producer and the AF_XDP one (utility.TunAdapterSends) → reorder source.
var tunLoopSends atomic.Uint64

// writerPinSeq assigns round-robin CPUs to pinned forward writers (FORWARD_WRITER_PIN=1).
var writerPinSeq atomic.Uint64

// fwdRounds/fwdRoundPkts measure forward drain-round chunk size (avg pkts the
// writer processes per wakeup = fwd_round_pkts / fwd_rounds).
var (
	fwdRounds    = expvar.NewInt("fwd_rounds")
	fwdRoundPkts = expvar.NewInt("fwd_round_pkts")
)

func main() {
	// M3 diagnosis (perf branch): enable block + mutex profiling so /debug/pprof/
	// block and /mutex have data, to pinpoint the serial-pipeline wait (the 2nd
	// core is ~idle and we need to know exactly what stages block on what).
	runtime.SetBlockProfileRate(10000)   // ~1 sample per 10us of cumulative blocking
	runtime.SetMutexProfileFraction(100) // sample 1/100 mutex contention events
	ctx := context.WithoutCancel(context.Background())
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func(ctxt context.Context) {
		<-sigc
		RunPreDown()
		GracefullyShutDown(ctxt)
	}(ctx)

	config.Load(&ctx)
	logLevel := ctx.Value("LOG_LEVEL").(string)
	logPath := constants.LOG_PATH
	logger.UpdateLogLevelName(logLevel)
	logger.UpdateLogPath(logPath)
	ifaceName := ctx.Value("WAN_INTERFACE").(string)
	bindAddr := netip.MustParseAddr(ctx.Value("BIND_ADDR").(string))
	listenPort, err := strconv.Atoi(ctx.Value("LISTEN_PORT").(string))

	if err != nil {
		logger.Fatal(fmt.Sprintf("Failed to parse proxy port: %v", err))
	}
	bindTo := netip.AddrPortFrom(bindAddr, uint16(listenPort))

	virtCIDR := ctx.Value("VIRT_CIDR").(string)
	_, virtSubnet, err := net.ParseCIDR(virtCIDR)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to parse VIRT_CIDR: %v", err))
	}
	virtIP, err := utility.FirstUsableIP(virtCIDR)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to parse and get first usable ip from %v: %v", virtCIDR, err))
	}
	serverTunIP, err = netip.ParseAddr(virtIP)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to parse %v: %v", virtIP, err))
	}
	if vp, perr := netip.ParsePrefix(virtCIDR); perr == nil {
		virtPrefix = vp.Masked()
	} else {
		logger.Fatal(fmt.Sprintf("failed to parse VIRT_CIDR prefix %v: %v", virtCIDR, perr))
	}

	ipProtocol := 0
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to parse FILTER_IP_PROTOCOL: %v", err))
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to get %s interface: %v", ifaceName, err))
	}
	// Adaptive MTU: derive the tunnel inner MTU + the QUIC outer packet size from
	// the WAN link MTU (capped to the XDP-native virtio limit) instead of hardcoding
	// a jumbo value. So a 1500/1G underlay -> ~1420 inner / ~1472 outer (old safe
	// behavior, no fragmentation), a jumbo-capable underlay -> up to ~3426 / ~3478.
	// An explicit TUNNEL_MTU (>0) overrides the auto value.
	effWanMTU := link.Attrs().MTU
	if effWanMTU > xdpNativeMaxMTU {
		effWanMTU = xdpNativeMaxMTU
	}
	serverInitialPacketSize = effWanMTU - outerOverhead // safely under the WAN MTU
	if serverInitialPacketSize > maxQUICPacket {
		serverInitialPacketSize = maxQUICPacket
	}
	if serverInitialPacketSize < 1252 {
		serverInitialPacketSize = 1252
	}
	var mtu uint64
	if v, ok := ctx.Value("TUNNEL_MTU").(string); ok {
		if m, e := strconv.ParseUint(v, 10, 64); e == nil {
			mtu = m
		}
	}
	if mtu == 0 { // auto: inner fits one DATAGRAM in the outer QUIC packet
		inner := serverInitialPacketSize - datagramOverhead
		if inner < 576 {
			inner = 576
		}
		mtu = uint64(inner)
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("MTU: WAN(eff)=%d -> inner=%d, QUIC InitialPacketSize=%d", effWanMTU, mtu, serverInitialPacketSize))
	}
	// assuming we are only doing IPv4
	family := netlink.FAMILY_V4
	addrs, err := netlink.AddrList(link, family)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to get addresses for %s: %v", ifaceName, err))
	}
	if len(addrs) == 0 {
		logger.Fatal(fmt.Sprintf("no IP addresses found for %s", ifaceName))
	}
	var wanAddr netip.Addr
	for _, addr := range addrs {
		a, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			continue
		}
		if !a.IsLinkLocalUnicast() {
			wanAddr = a.Unmap()
			break
		}
	}
	if !wanAddr.IsValid() {
		logger.Fatal(fmt.Sprintf("no usable IP on %s", ifaceName))
	}
	localLANs = buildLocalLANs(ifaceName)
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("forward router: virt=%s wan=%s/%s localLANs=%v", virtPrefix, wanAddr, ifaceName, localLANs))
	}
	// Upload-forward egress mode. GSO needs the main tun opened IFF_VNET_HDR so the
	// coalescer can write TSO super-frames straight into it (no dedicated tmfwd0
	// device). vhost uses its own dedicated TAP "tmvhost0" — "tm"-prefixed so the standard tm+ iptables rules cover it; a true fold into the main tun is kernel-blocked (tun_xdp_one is Ethernet-only, see memory/QUIC_DCO notes).
	// DEFAULT = tun-GSO: best aggregate across every measured regime, decisively so
	// on single-queue NICs (~2-4x AF_XDP-TX under none/rps; ≈ AF_XDP-TX under rss —
	// 16-core matrix, kernels 6.8 + 6.17). Explicit FORWARD_TUN_GSO=false restores
	// the AF_XDP-TX egress; FORWARD_TUN_VHOST=true (without GSO set) selects vhost.
	forwardTunVhost := ctxBool(ctx, "FORWARD_TUN_VHOST")
	forwardTunGSO := ctxBoolDefault(ctx, "FORWARD_TUN_GSO", !forwardTunVhost)
	utility.SetForwardMode(forwardTunGSO, forwardTunVhost)

	netBitSize, _ := virtSubnet.Mask.Size()
	devs, err := createTunTapDevice(ctx, virtIP, netBitSize, int(mtu), forwardTunGSO)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to create tun/tap device: %v", err))
	}
	tunTapDevice = devs

	upChan := make(chan bool)
	go func(ctxt context.Context) {
		for {
			isRunning := <-upChan
			if isRunning {
				RunPostUp(ctxt)
			} else {
				GracefullyShutDown(ctxt)
			}
		}
	}(ctx)
	Bootstrap(ctx)
	if err := run(ctx, upChan, bindTo, uint8(ipProtocol)); err != nil {
		logger.Fatal(fmt.Sprintf("%v", err))
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Shutting down masque server.")
	}
}

// udpBufTarget is the UDP socket-buffer ceiling for the QUIC transport. quic-go
// requests ~7 MB but the kernel clamps it to net.core.{r,w}mem_max (stock ~208 KB),
// which overflows on bursts → dropped UDP packets → loss that is fatal to a single
// stream. NOTE: the server's QUIC path uses AF_XDP (its own UMEM, not a kernel UDP
// socket), so this mainly helps any kernel-UDP fallback and keeps client/server
// symmetric; the client (kernel UDP) is where it matters most. 7.5 MB.
const udpBufTarget = 7864320

// raiseSysctl raises a /proc/sys value to target only if currently lower. Raise-only
// and best-effort: missing path or read-only /proc (restricted container) just logs.
func raiseSysctl(key string, target int) {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	cur := 0
	if b, err := os.ReadFile(path); err == nil {
		cur, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	if cur >= target {
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(target)), 0644); err != nil {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("sysctl %s: could not raise to %d (%v); leaving %d", key, target, err, cur))
		}
		return
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("sysctl %s: %d -> %d (UDP buffer for QUIC transport)", key, cur, target))
	}
}

// tuneUDPBuffers raises the UDP socket buffer ceilings so quic-go's large-buffer
// request isn't clamped to the stock ~208 KB. Only the _max ceilings are touched.
func tuneUDPBuffers() {
	raiseSysctl("net.core.rmem_max", udpBufTarget)
	raiseSysctl("net.core.wmem_max", udpBufTarget)
}

// innerTCPBufTarget is the autotuning ceiling (max field of tcp_wmem/tcp_rmem) for
// INNER application TCP flows. The tunnel adds RTT, enlarging the inner TCP's BDP
// beyond a direct path's; at ~800 Mbit/s over ~35 ms the BDP is ~3.4 MB and Linux
// autotuning only grows a connection to ~half the max, so the stock 4 MB tcp_wmem
// max leaves a single stream sndbuf-limited (measured client-side: ~430 vs ~850
// Mbit/s once raised). Mirrors the client; on the server this matters for the
// local-delivery path where the server is the TCP endpoint. 32 MB.
const innerTCPBufTarget = 33554432

// raiseSysctlTriple raises only the third (max) field of a "min default max" sysctl
// (tcp_wmem/tcp_rmem), preserving min/default. Raise-only, best-effort. The max
// field is the autotuning ceiling for SO_SNDBUF/SO_RCVBUF, so this unblocks a single
// high-BDP flow without bloating every socket.
func raiseSysctlTriple(key string, targetMax int) {
	path := "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
	b, err := os.ReadFile(path)
	if err != nil {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("sysctl %s: could not read (%v); leaving as-is", key, err))
		}
		return
	}
	fields := strings.Fields(strings.TrimSpace(string(b)))
	if len(fields) != 3 {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("sysctl %s: unexpected format %q; leaving as-is", key, string(b)))
		}
		return
	}
	curMax, _ := strconv.Atoi(fields[2])
	if curMax >= targetMax {
		return
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%s %s %d", fields[0], fields[1], targetMax)), 0644); err != nil {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("sysctl %s: could not raise max to %d (%v); leaving %d", key, targetMax, err, curMax))
		}
		return
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("sysctl %s: max %d -> %d (inner-TCP BDP over tunnel)", key, curMax, targetMax))
	}
}

// tuneInnerTCPBuffers raises the inner application TCP autotuning ceilings so a
// single TCP stream over the tunnel can fill the tunnel's larger BDP. Raise-only;
// min/default preserved so idle sockets stay small.
func tuneInnerTCPBuffers() {
	raiseSysctlTriple("net.ipv4.tcp_wmem", innerTCPBufTarget)
	raiseSysctlTriple("net.ipv4.tcp_rmem", innerTCPBufTarget)
}

func Bootstrap(ctx context.Context) {
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Server in bootstrap phase")
	}
	cmd := exec.Command("/bin/bash", "-c", constants.BOOTSTRAP_SCRIPT_PATH)
	_, err := cmd.Output()
	if err != nil {
		logger.Fatal(fmt.Sprintf("Failed bootstrap scripts: %v", err))
	}
	tuneUDPBuffers()
	tuneInnerTCPBuffers()
	MigrateData(ctx)
}

func MigrateData(ctx context.Context) {
	// Migrate the schema
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Migrating data")
	}
	if err := migration.Invoke(ctx); err != nil {
		logger.Fatal(fmt.Sprintf("DB migration failed: %v", err))
	}
}

func ClearInternalDHCP(ctx context.Context) {
	virtCIDR := ctx.Value("VIRT_CIDR").(string)
	virtIP, _ := utility.FirstUsableIP(virtCIDR)
	lastIP, _ := utility.LastUsableIP(virtCIDR)
	_, virtIPNum, _ := utility.ParseIP(virtIP)
	_, lastIPNum, _ := utility.ParseIP(lastIP)
	// virtIP = reserved IP for server
	service.ResetDHCP(ctx, int64(virtIPNum+1), int64(lastIPNum))
}

func RunPostUp(ctx context.Context) {
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Server in post-up phase")
	}
	cmd := exec.Command("/bin/bash", "-c", constants.POSTUP_SCRIPT_PATH)
	_, err := cmd.Output()
	if err != nil {
		logger.Fatal(fmt.Sprintf("Cannot run postup scripts: %v", err))
	}
	go func(contxt context.Context) {
		RunManagementService(contxt)
	}(ctx)
	enableStatsStr, _ := ctx.Value("ENABLE_STATISTIC").(string)
	enableStats, _ := strconv.ParseBool(enableStatsStr)
	stats.Enable(enableStats) // drives the STATISTIC channel + the per-packet observers
	config.Watch(ctx)         // hot-reload LOG_LEVEL + ENABLE_STATISTIC on config-file change
	if enableStats {
		go http.ListenAndServe("localhost:6060", nil)
		go func() {
			t := time.NewTicker(2 * time.Second)
			for range t.C {
				quicChDrops := afxdpConn.QuicChDrops()
				fwdDrops := afxdpConn.FwdDrops()
				txDrops := afxdpConn.TxDrops()
				tunDrops := tunChanDrops.Load()
				pktDrops := pktChanDrops.Load()
				adapterDrops := utility.TunAdapterDrops.Load()
				adapterSends := utility.TunAdapterSends.Load()
				tunLoopSnd := tunLoopSends.Load()
				xf, xp, xa, sf, sp, sa := utility.BatchStats()
				diag := afxdpConn.DiagSnapshot()
				preTot := utility.PreReseqTotal.Load()
				preOOO := utility.PreReseqOOO.Load()
				prePct := 0.0
				if preTot > 0 {
					prePct = 100 * float64(preOOO) / float64(preTot)
				}
				psTot := utility.PreSendTotal.Load()
				psGen := utility.PreSendGenuine.Load()
				psRetr := utility.PreSendRetr.Load()
				psGenPct, psRetrPct := 0.0, 0.0
				if psTot > 0 {
					psGenPct = 100 * float64(psGen) / float64(psTot)
					psRetrPct = 100 * float64(psRetr) / float64(psTot)
				}
				// dg_packer_* lives in lib/quic-go; pull via expvar.Get to avoid an import cycle.
				var pkTot, pkGen, pkRetr int64
				if v := expvar.Get("dg_packer_total"); v != nil {
					pkTot = v.(*expvar.Int).Value()
				}
				if v := expvar.Get("dg_packer_genuine"); v != nil {
					pkGen = v.(*expvar.Int).Value()
				}
				if v := expvar.Get("dg_packer_retr"); v != nil {
					pkRetr = v.(*expvar.Int).Value()
				}
				pkGenPct, pkRetrPct := 0.0, 0.0
				if pkTot > 0 {
					pkGenPct = 100 * float64(pkGen) / float64(pkTot)
					pkRetrPct = 100 * float64(pkRetr) / float64(pkTot)
				}
				if stats.ShouldLog() {
					stats.Statistic(fmt.Sprintf(
						"xdp: %s | throughput: xdp(fl=%d pk=%d avg=%.1f) sock(fl=%d pk=%d avg=%.1f) | drops: quicCh=%d fwd=%d tx=%d tunChan=%d pktChan=%d adapter=%d | dl-producers: afxdp=%d tunloop=%d | pre-reseq: ooo=%d/%d (%.2f%%) | pre-send: genuine=%d/%d (%.2f%%) retr=%d (%.2f%%) | dg-packer: genuine=%d/%d (%.2f%%) retr=%d (%.2f%%)",
						diag, xf, xp, xa, sf, sp, sa,
						quicChDrops, fwdDrops, txDrops, tunDrops, pktDrops, adapterDrops,
						adapterSends, tunLoopSnd,
						preOOO, preTot, prePct,
						psGen, psTot, psGenPct, psRetr, psRetrPct,
						pkGen, pkTot, pkGenPct, pkRetr, pkRetrPct,
					))
				}
			}
		}()
	}
}

func RunPreDown() {
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Server in pre-down phase")
	}
	cmd := exec.Command("/bin/bash", "-c", constants.PREDOWN_SCRIPT_PATH)
	_, err := cmd.Output()
	if err != nil {
		logger.Fatal(fmt.Sprintf("Cannot run predown scripts: %v", err))
	}
}

func GracefullyShutDown(ctx context.Context) {
	if logger.ShouldLog(logger.INFO) {
		logger.Info("Shutting down")
	}
	db.CloseConnection()
	// Detach every NIC's XDP bpf_link first (virtio_net rejects MTU > 3506 while
	// XDP-native is attached), then restore the WAN MTU that bootstrap/004 scaled down.
	for _, l := range xdpLoadersRef {
		l.Close()
	}
	if len(xdpLoadersRef) == 0 && xdpLoaderRef != nil {
		xdpLoaderRef.Close()
	}
	restoreWanMTU(ctx)
	restoreWanOffloads()
	os.Exit(0)
}

// restoreWanOffloads reverts the offload flags that bootstrap/004 disabled for
// XDP-native (gro/lro) on EVERY NIC it pinned, from the per-NIC states saved in
// /etc/tmasqued/<iface>.offloads.orig ("<flag> <on|off>" per line). bootstrap pins
// all real NICs (the WAN + any bridged LAN NIC), so restore globs the saved files
// rather than only wanIfaceName. No-op when nothing was saved; each file is removed
// after its NIC is restored.
func restoreWanOffloads() {
	files, _ := filepath.Glob("/etc/tmasqued/*.offloads.orig")
	for _, origFile := range files {
		iface := strings.TrimSuffix(filepath.Base(origFile), ".offloads.orig")
		if iface == "" {
			continue
		}
		data, err := os.ReadFile(origFile)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || (fields[1] != "on" && fields[1] != "off") {
				continue
			}
			out, e := exec.Command("ethtool", "-K", iface, fields[0], fields[1]).CombinedOutput()
			if logger.ShouldLog(logger.INFO) {
				logger.Info(fmt.Sprintf("Restored %s offload %s=%s (err=%v %s)",
					iface, fields[0], fields[1], e, strings.TrimSpace(string(out))))
			}
		}
		os.Remove(origFile)
	}
}

// restoreWanMTU reverts every NIC that bootstrap/004 scaled down to its original MTU,
// saved per-NIC in /etc/tmasqued/<iface>.wan_mtu.orig before the scale-down to the
// XDP-native virtio limit. Globs the saved files so all pinned NICs (WAN + bridged
// LAN NICs) are restored, not just wanIfaceName. No-op when nothing was scaled.
func restoreWanMTU(ctx context.Context) {
	files, _ := filepath.Glob("/etc/tmasqued/*.wan_mtu.orig")
	for _, origFile := range files {
		iface := strings.TrimSuffix(filepath.Base(origFile), ".wan_mtu.orig")
		if iface == "" {
			continue
		}
		data, err := os.ReadFile(origFile)
		if err != nil {
			continue
		}
		mtu, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || mtu <= 0 {
			os.Remove(origFile)
			continue
		}
		if link, e := netlink.LinkByName(iface); e == nil {
			e2 := netlink.LinkSetMTU(link, mtu)
			if logger.ShouldLog(logger.INFO) {
				logger.Info(fmt.Sprintf("Restored %s MTU to %d (err=%v)", iface, mtu, e2))
			}
		}
		os.Remove(origFile)
	}
}

// ctxBool reads a boolean config option from ctx (loaded from tmasqued.conf by
// config.Load). Absent or unparseable -> false. Values already TrimSpace'd in Load.
func ctxBool(ctx context.Context, key string) bool {
	if v, ok := ctx.Value(key).(string); ok {
		b, _ := strconv.ParseBool(v)
		return b
	}
	return false
}

// ctxBoolDefault is ctxBool with an explicit default: the default applies when the
// key is absent from the config OR present but unparseable; an explicit valid
// "false"/"true" always wins. Use for options whose shipped default is not false.
func ctxBoolDefault(ctx context.Context, key string, def bool) bool {
	if v, ok := ctx.Value(key).(string); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// createTunTapDevice creates the main MASQUE tun (multiqueue). When gso is set the
// device is opened IFF_VNET_HDR + TUNSETOFFLOAD (via water), so the read side splits
// kernel GSO super-frames and the upload-forward coalescer can write TSO super-frames
// straight to GSOFd. All queues share the same flags.
// buildLocalLANs enumerates directly-connected IPv4 subnets on every interface
// EXCEPT the WAN uplink, the tun/tap VPN devices, and loopback. A forwarded inner
// packet whose destination falls in one of these is bridged transparently out that
// NIC (kernel ip_forward, no SNAT). Read once at startup; read-only thereafter.
func buildLocalLANs(wanIface string) []netip.Prefix {
	var out []netip.Prefix
	links, err := netlink.LinkList()
	if err != nil {
		return out
	}
	for _, l := range links {
		name := l.Attrs().Name
		// Exclude only what genuinely must NOT be a kernel-forward LAN target:
		//   - the WAN uplink: its subnet egresses via the AF_XDP fast path (branch-4),
		//     not the kernel forward, so it must stay out of localLANs.
		//   - loopback: never a forward destination.
		//   - OUR OWN datapath devices ("tm" prefix — tm0 main tun, tmvhost0, tmnapi0):
		//     the vIP overlay is handled by branch-2 (c2c), not a bridge.
		// Everything else with a connected IPv4 subnet is a valid branch-3 kernel-forward
		// target: docker0, custom bridges, other LAN NICs — AND crucially ANOTHER VPN the
		// operator runs (wireguard wg*, openvpn tun*/tap*): naming ours "tm" lets us bridge
		// to subnets reached via a coexisting VPN instead of wrongly excluding tun*/tap*.
		// (ip_forward + the `! -o tm+` masquerade route it out the right NIC.)
		if name == wanIface || name == "lo" || strings.HasPrefix(name, "tm") {
			continue
		}
		addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip, ok := netip.AddrFromSlice(a.IP.To4())
			if !ok {
				continue
			}
			ones, _ := a.IPNet.Mask.Size()
			p := netip.PrefixFrom(ip.Unmap(), ones).Masked()
			out = append(out, p)
		}
	}
	return out
}

func createTunTapDevice(ctx context.Context, virtIp string, virtPrefixLen int, mtu int, gso bool) ([]*water.Interface, error) {
	numQueues := runtime.NumCPU()
	// TUN_QUEUES overrides the tun-read goroutine count (default = NumCPU). Set to 1
	// to serialize tun ingest when diagnosing download reorder.
	if v := os.Getenv("TUN_QUEUES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			numQueues = n
		}
	}
	devs := make([]*water.Interface, numQueues)

	// First device — explicitly named "tm0" (NOT the kernel's auto "tun%d"). The "tm"
	// prefix (tmasque) lets the forward router (buildLocalLANs) and the iptables rules
	// tell OUR datapath devices apart from ANY OTHER VPN the operator runs alongside us:
	// wireguard/openvpn use tun*/tap*/wg*, which we now correctly treat as bridgeable
	// LANs instead of excluding them. Delete a leftover tm0 from a prior crash first.
	var err error
	if leftover, e := netlink.LinkByName("tm0"); e == nil {
		_ = netlink.LinkDel(leftover)
	}
	devs[0], err = water.New(water.Config{
		DeviceType: water.TUN,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name:       "tm0",
			MultiQueue: true,
			GSO:        gso,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN device queue 0: %w", err)
	}
	devName := devs[0].Name()
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("Created TUN device: %s (gso=%v)", devName, gso))
	}
	// Subsequent queues — MUST use same name
	for i := 1; i < numQueues; i++ {
		dev, err := water.New(water.Config{
			DeviceType: water.TUN,
			PlatformSpecificParams: water.PlatformSpecificParams{
				Name:       devName, // same device, new fd
				MultiQueue: true,
				GSO:        gso,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create TUN queue %d: %w", i, err)
		}
		devs[i] = dev
	}

	link, err := netlink.LinkByName(devs[0].Name())
	if err != nil {
		return nil, fmt.Errorf("Failed to get TUN interface: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("failed to bring up TUN interface: %w", err)
	}
	addr, err := netlink.ParseAddr(virtIp + "/" + strconv.Itoa(virtPrefixLen))
	if err != nil {
		return nil, fmt.Errorf("Failed to assign IP to %v: %v", devs[0].Name(), err)
	}
	netlink.AddrAdd(link, addr)
	netlink.LinkSetMTU(link, mtu)
	_, clientSubnet, err := net.ParseCIDR(ctx.Value("VIRT_CIDR").(string))
	if err != nil {
		return nil, fmt.Errorf("Failed to parse address: %w", err)
	}
	ip := clientSubnet.IP.String()
	bitmask, _ := clientSubnet.Mask.Size()
	prefixAddr, err := netip.ParsePrefix(ip + "/" + strconv.Itoa(bitmask))
	if err != nil {
		return nil, fmt.Errorf("Failed to parse prefix: %w", err)
	}
	route := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: utility.PrefixToIPNet(prefixAddr)}
	if err := netlink.RouteAdd(route); err != nil && errors.Is(err, syscall.EEXIST) {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("Route %v already exists, skipping", route))
		}
	} else if err != nil {
		return nil, fmt.Errorf("Failed to add route %v: %w", route, err)
	}

	return devs, nil
}

// firstIPv4 returns the first non-link-local IPv4 address configured on ifaceName.
// This is the SNAT source IP for traffic egressing that NIC.
func firstIPv4(ifaceName string) (netip.Addr, error) {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("link %s: %w", ifaceName, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("addrs %s: %w", ifaceName, err)
	}
	for _, addr := range addrs {
		a, ok := netip.AddrFromSlice(addr.IP)
		if !ok || a.IsLinkLocalUnicast() {
			continue
		}
		return a.Unmap(), nil
	}
	return netip.Addr{}, fmt.Errorf("no usable IPv4 on %s", ifaceName)
}

// maximizeChannelsFor returns the combined-channel count to use for ifaceName.
// MAXIMIZE_CHANNELS (default true) raises combined channels to the hardware max so
// the datagram TX path can fan out across every hardware ring; set false to honor an
// externally-configured count (e.g. a regime sweep pinning combined=1). Best-effort:
// always returns >= 1.
func maximizeChannelsFor(ctx context.Context, ifaceName string) int {
	var finalCombined int
	if v := ctx.Value("MAXIMIZE_CHANNELS"); v == nil || ctxBool(ctx, "MAXIMIZE_CHANNELS") {
		fc, mErr := xdp.MaximizeChannels(ifaceName)
		finalCombined = fc
		if mErr != nil && logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("NIC %s: MaximizeChannels best-effort: %v (combined=%d)", ifaceName, mErr, finalCombined))
		}
	} else {
		finalCombined = 1
		if ci, e := xdp.GetChannels(ifaceName); e == nil {
			finalCombined = ci.CurrentCombined
		}
	}
	if finalCombined < 1 {
		finalCombined = 1 // a driver can report Combined:0; guard before DatagramSendBuckets
	}
	return finalCombined
}

func run(ctxt context.Context, upChan chan<- bool, bindTo netip.AddrPort, ipProtocol uint8) error {
	ctx, cancel := context.WithCancel(ctxt)
	defer cancel()
	// ---- Multi-NIC AF_XDP/XDP datapath ----
	// WAN_INTERFACE is OPTIONAL: we auto-detect every real NIC and attach the XDP
	// program to ALL of them (so the reverse-NAT return fast path fires regardless of
	// which NIC a reply arrives on). QUIC LISTEN (xsks_quic) is registered only on
	// WAN_INTERFACE when it is set; when unset we listen on every real NIC.
	wanIface, _ := ctxt.Value("WAN_INTERFACE").(string) // "" => auto / listen on all
	nicNames, err := xdp.DetectRealNICs()
	if err != nil {
		return fmt.Errorf("detecting real NICs: %w", err)
	}
	if len(nicNames) == 0 {
		return fmt.Errorf("no real NICs detected to attach the datapath")
	}
	quicListen := map[string]bool{}
	if wanIface != "" {
		inSet := false
		for _, n := range nicNames {
			if n == wanIface {
				inSet = true
				break
			}
		}
		if !inSet {
			return fmt.Errorf("WAN_INTERFACE %q is not among the detected real NICs %v", wanIface, nicNames)
		}
		quicListen[wanIface] = true
	} else {
		for _, n := range nicNames {
			quicListen[n] = true
		}
	}

	loaders, err := xdp.LoadMultiNIC(nicNames)
	if err != nil {
		return fmt.Errorf("loading XDP on %v: %w", nicNames, err)
	}
	for _, l := range loaders {
		defer l.Close()
	}
	xdpLoadersRef = loaders // for GracefullyShutDown's per-NIC XDP-detach + MTU restore
	if len(loaders) > 0 {
		xdpLoaderRef = loaders[0]
	}
	if logger.ShouldLog(logger.INFO) {
		for _, l := range loaders {
			logger.Info(fmt.Sprintf("XDP attached on %s (mode=%s, quic-listen=%v)",
				l.Iface().Name, l.Mode(), quicListen[l.Iface().Name]))
		}
	}

	natTable, err := xdp.OpenNatTable()
	if err != nil {
		return fmt.Errorf("opening NAT table: %w", err)
	}
	defer natTable.Close()

	localAddr := &net.UDPAddr{IP: bindTo.Addr().AsSlice(), Port: int(bindTo.Port())}
	// XDP_USE_NEED_WAKEUP skips the per-batch sendto kick on the TX path. On by
	// default; kill switch: set XDP_NEED_WAKEUP=false in the config.
	needWakeup := true
	if v, _ := ctxt.Value("XDP_NEED_WAKEUP").(string); v != "" {
		needWakeup, _ = strconv.ParseBool(v)
	}
	// QUIC_KERNEL_UDP=1: run the QUIC transport on a KERNEL UDP socket instead of
	// AF_XDP. We skip registering xsks_quic on every NIC (so XDP passes UDP/443 to the
	// kernel) and hand quic-go a net.ListenUDP socket; the AF_XDP Conns are kept ONLY
	// for the forward-return dispatch (xsks_fwd).
	quicKernelUDP := os.Getenv("QUIC_KERNEL_UDP") == "1"

	// Build one AF_XDP Conn per real NIC. QUIC-listen NICs register xsks_quic; ALL
	// NICs register xsks_fwd (reverse-NAT return). Channels are maximized per NIC so
	// every egress has its TX rings (Phase 2). A NIC without a usable IPv4 is skipped.
	sessionTable := xdp.NewSessionTable()
	var listenConns []*xdp.Conn
	egressNICs = egressNICs[:0]
	for _, l := range loaders {
		ifc := l.Iface()
		addr, aerr := firstIPv4(ifc.Name)
		if aerr != nil {
			if logger.ShouldLog(logger.WARN) {
				logger.Warn(fmt.Sprintf("NIC %s: %v — skipping datapath on it", ifc.Name, aerr))
			}
			continue
		}
		combined := maximizeChannelsFor(ctx, ifc.Name)
		nq, qerr := xdp.GetQueueInfo(ifc.Name)
		if qerr != nil {
			if logger.ShouldLog(logger.WARN) {
				logger.Warn(fmt.Sprintf("NIC %s queue info failed, skipping datapath on it: %v", ifc.Name, qerr))
			}
			continue
		}
		registerQuic := quicListen[ifc.Name] && !quicKernelUDP
		conn, cerr := xdp.NewConn(ifc, l.XskQuicMap(), l.XskFwdMap(), localAddr, nq.RX, l.Mode(), needWakeup, !registerQuic)
		if cerr != nil {
			// Non-fatal: skip the datapath on this NIC (its XDP stays attached but
			// XDP_PASSes everything since its xsk maps are empty). The required
			// QUIC-listen NIC(s) are verified after the loop.
			if logger.ShouldLog(logger.WARN) {
				logger.Warn(fmt.Sprintf("AF_XDP conn on %s failed, skipping datapath on it: %v", ifc.Name, cerr))
			}
			continue
		}
		defer conn.Close()
		conn.SetForwardHandler(sessionTable.Deliver)
		egressNICs = append(egressNICs, nicEgress{name: ifc.Name, conn: conn, addr: addr, combined: combined})
		if registerQuic {
			listenConns = append(listenConns, conn)
		}
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("NIC %s: addr=%s combined=%d RX-queues=%d quic-listen=%v",
				ifc.Name, addr, combined, nq.RX, registerQuic))
		}
	}
	if len(egressNICs) == 0 {
		return fmt.Errorf("no NIC with a usable IPv4 to bind the datapath")
	}

	// Pair each directly-connected LAN subnet with the egress NIC that owns it (the
	// one whose own IPv4 sits inside the subnet). A subnet with no real XDP NIC
	// (docker0, a coexisting VPN tun) keeps egIdx == -1 → kernel bridge fallback.
	lanRoutes = lanRoutes[:0]
	for _, p := range localLANs {
		egIdx := -1
		for i := range egressNICs {
			if p.Contains(egressNICs[i].addr) {
				egIdx = i
				break
			}
		}
		lanRoutes = append(lanRoutes, lanRoute{prefix: p, egIdx: egIdx})
		if logger.ShouldLog(logger.INFO) {
			owner := "kernel-bridge (no XDP NIC)"
			if egIdx >= 0 {
				owner = fmt.Sprintf("%s XDP SNAT %s", egressNICs[egIdx].name, egressNICs[egIdx].addr)
			}
			logger.Info(fmt.Sprintf("LAN route %s -> %s", p, owner))
		}
	}

	// Primary egress = the first QUIC-listen NIC (the WAN), else the first NIC. It
	// drives the existing WAN forward (branch-4) + handleConn (afxdpConn/wanAddr) and
	// supplies the DatagramSendBuckets count.
	primary := egressNICs[0]
	for _, e := range egressNICs {
		if quicListen[e.name] {
			primary = e
			break
		}
	}
	afxdpConn = primary.conn
	wanAddr := primary.addr
	wanIfaceName = primary.name
	finalCombined := primary.combined

	// quicConn is what quic-go listens on: a kernel UDP socket (QUIC_KERNEL_UDP=1) or
	// the merged multi-NIC AF_XDP transport (default; a single Conn unwrapped when only
	// one NIC listens). The per-NIC Conns drive the forward-return path either way.
	var quicConn net.PacketConn
	if quicKernelUDP {
		uc, uerr := net.ListenUDP("udp", localAddr)
		if uerr != nil {
			return fmt.Errorf("kernel UDP listen %v: %w", localAddr, uerr)
		}
		defer uc.Close()
		quicConn = uc
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("QUIC transport: KERNEL UDP on %v (AF_XDP RX bypassed for 443)", localAddr))
		}
	} else {
		if len(listenConns) == 0 {
			return fmt.Errorf("no QUIC-listen NIC available (WAN_INTERFACE=%q, detected %v)", wanIface, nicNames)
		}
		quicConn = xdp.NewMultiConn(listenConns)
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("QUIC transport: AF_XDP on %d NIC(s)", len(listenConns)))
		}
	}
	cert, err := tls.LoadX509KeyPair(constants.SERVER_CERT_PATH, constants.SERVER_KEY_PATH)
	if err != nil {
		return fmt.Errorf("Failed to load TLS certificate: %w", err)
	}
	certPool, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("Cannot create cert pool: %w", err)
	}
	caCertPEM, err := os.ReadFile(constants.CLIENT_CA_PATH)
	if err != nil {
		return fmt.Errorf("cannot read client CA: %w", err)
	}
	ok := certPool.AppendCertsFromPEM(caCertPEM)
	if !ok {
		return fmt.Errorf("Invalid cert")
	}
	template := uritemplate.MustNew(fmt.Sprintf("https://tmasqued:%d/vpn", bindTo.Port()))
	serverConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    certPool,
	}
	ln, err := quic.ListenEarly(
		quicConn,
		http3.ConfigureTLSConfig(serverConf),
		&quic.Config{
			EnableDatagrams: true,
			// InitialPacketSize raises the QUIC packet size (and therefore the
			// SendDatagram payload budget) to quic-go's max. PMTUD is disabled
			// below, so currentMTUEstimate is FROZEN at estimateMaxPayloadSize(
			// InitialPacketSize) for the connection's life — and that is the cap
			// SendDatagram enforces on every DOWNLOAD datagram. Left unset it
			// defaults to 1280 → ~1243B budget → every full-MTU download datagram
			// is rejected with DatagramTooLargeError (connect-ip swallows it) →
			// 0 download throughput. 1452 = MaxPacketBufferSize → ~1415B budget,
			// which accommodates the 1400 tun MTU (assumes a >=1480-capable WAN).
			InitialPacketSize: uint16(serverInitialPacketSize),
			// DatagramSendBuckets is set to the active NIC combined-channel
			// count so per-flow TX dispatch can route each bucket to its own
			// hardware ring (Phase 3 of the multi-worker fan-out). When the
			// NIC reports 1 (loopback / single-channel virtual NIC) the queue
			// falls back to the original single-FIFO behavior.
			DatagramSendBuckets:            finalCombined,
			MaxIdleTimeout:                 30 * time.Second,
			KeepAlivePeriod:                10 * time.Second,
			InitialStreamReceiveWindow:     10 * 1024 * 1024, // 10 MB
			MaxStreamReceiveWindow:         10 * 1024 * 1024, // 10 MB
			InitialConnectionReceiveWindow: 15 * 1024 * 1024, // 15 MB
			MaxConnectionReceiveWindow:     15 * 1024 * 1024, // 15 MB
			DisablePathMTUDiscovery:        true,
			MaxIncomingStreams:             0,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create QUIC listener: %w", err)
	}
	defer ln.Close()

	p := connectip.Proxy{}
	mux := http.NewServeMux()
	ipToTunChan = make(map[netip.Addr][]chan *utility.Packet)
	mu = &sync.RWMutex{}
	for i, dev := range tunTapDevice {
		go func(d *water.Interface, id int) {
			for {
				pkt := utility.PacketPool.Get().(*utility.Packet)
				n, err := d.Read(pkt.Buf)
				if err != nil {
					utility.PacketPool.Put(pkt) // return on error path too
					if logger.ShouldLog(logger.ERROR) {
						logger.Error(fmt.Sprintf("queue#%d cannot read TUN/TAP device %v: %v", id, d.Name(), err))
					}
					cancel()
					break
				}
				pkt.N = n
				// assuming we are only doing IPv4
				destIP, ok := netip.AddrFromSlice(pkt.Buf[16:20])
				if !ok {
					utility.PacketPool.Put(pkt) // return on error path too
					if logger.ShouldLog(logger.TRACE) {
						logger.Trace(fmt.Sprintf("queue#%d cannot parse data to IP. Dropping packet.", id))
					}
					continue
				}
				if logger.ShouldLog(logger.TRACE) {
					logger.Trace(fmt.Sprintf("queue#%d dest IP to filter %v", id, destIP.String()))
				}
				destIP = destIP.Unmap()
				mu.RLock()
				tunChan := pickTunnel(ipToTunChan[destIP], pkt.Buf[:pkt.N])
				mu.RUnlock()
				if tunChan != nil {
					select {
					case tunChan <- pkt:
						tunLoopSends.Add(1)
					default:
						utility.PacketPool.Put(pkt) // return on error path too
						if logger.ShouldLog(logger.TRACE) {
							logger.Trace(fmt.Sprintf("queue#%d client %s channel full, dropping packet.", id, destIP.String()))
						}
						tunChanDrops.Add(1)
					}
				} else {
					utility.PacketPool.Put(pkt) // return on error path too
					if logger.ShouldLog(logger.TRACE) {
						logger.Trace(fmt.Sprintf("queue#%d cannot find connection for client IP = %s. Dropping packet.", id, destIP.String()))
					}
				}
			}
		}(dev, i)
	}
	mux.HandleFunc("/vpn", func(w http.ResponseWriter, r *http.Request) {
		if logger.ShouldLog(logger.DEBUG) {
			logger.Debug(fmt.Sprintf("/vpn handler reached, TLS peer certs: %d", len(r.TLS.PeerCertificates)))
		}
		commonName := r.TLS.PeerCertificates[0].Subject.CommonName
		clientId, err := strconv.ParseInt(commonName, 10, 64)
		if err != nil {
			if logger.ShouldLog(logger.INFO) {
				logger.Info(fmt.Sprintf("Got invalid TLS common name %v: %v", commonName, err))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if logger.ShouldLog(logger.DEBUG) {
			logger.Debug(fmt.Sprintf("Handle new HTTP client %v", clientId))
		}
		conCtx := context.WithValue(ctx, "clientId", clientId)
		// Bonded-tunnel coordinates (Model A). A legacy single-tunnel client
		// omits these → index 0, count 1.
		tunIdx, _ := strconv.Atoi(r.Header.Get("Tmasqued-Tunnel-Index"))
		tunCount, _ := strconv.Atoi(r.Header.Get("Tmasqued-Tunnel-Count"))
		if tunCount < 1 {
			tunCount = 1
		}
		if tunIdx < 0 || tunIdx >= tunCount {
			tunIdx = 0
		}
		req, err := connectip.ParseRequest(r, template)
		if err != nil {
			var perr *connectip.RequestParseError
			if errors.As(err, &perr) {
				w.WriteHeader(perr.HTTPStatus)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		conn, err := p.Proxy(w, req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// tunChan: bursty producer (tun-read goroutine) vs steady consumer
		// (tunChan-reader → WritePacket → quic). Old 256 dropped ~16k packets
		// (4% of stream) during P100 bursts. 4096 = ~5 MB at MTU; well-bounded
		// memory while absorbing the per-conn-burst that drove TCP retransmits.
		if err := handleConn(conCtx, make(chan *utility.Packet, 4096), conn, ipProtocol, natTable, wanAddr, sessionTable, tunIdx, tunCount); err != nil {
			if logger.ShouldLog(logger.ERROR) {
				logger.Error(fmt.Sprintf("failed to handle connection: %v", err))
			}
			return
		}
	})

	s := http3.Server{
		Handler:         mux,
		EnableDatagrams: true,
	}
	upChan <- true
	go func() {
		if err := s.ServeListener(ln); err != nil {
			logger.Fatal(fmt.Sprintf("ServeListener error: %v", err))
		}
	}()
	defer s.Close()
	<-ctx.Done()
	upChan <- false
	return nil
}

// pickTunnel selects the bonded tunnel for a download packet by hashing its
// 5-tuple, pinning a flow to one tunnel (no cross-tunnel reorder). If that
// tunnel is currently down (nil slot), it falls back to the next live tunnel;
// returns nil only when the client has no live tunnels. Caller holds mu.
func pickTunnel(chans []chan *utility.Packet, ip []byte) chan *utility.Packet {
	n := len(chans)
	if n == 0 {
		return nil
	}
	start := int(utility.FlowHash(ip) % uint32(n))
	for i := 0; i < n; i++ {
		if c := chans[(start+i)%n]; c != nil {
			return c
		}
	}
	return nil
}

func handleConn(ctx context.Context, tunChan chan *utility.Packet, conn *connectip.Conn, ipProtocol uint8, natTable *xdp.NatTable, wanAddr netip.Addr, sessionTable *xdp.SessionTable, tunIdx, tunCount int) error {
	setupCtx, setupCancel := context.WithTimeout(ctx, 5*time.Second)
	defer setupCancel()
	if logger.ShouldLog(logger.DEBUG) {
		logger.Debug("Start connectip flow")
	}
	// Get the next unassigned address. The client tun gets a /32 (RFC 9484
	// ADDRESS_ASSIGN requires a canonical prefix — a host address can't carry a /10).
	// Whole-subnet reachability for client-to-client is done the protocol-correct
	// way instead: VIRT_CIDR is appended to the advertised routes below, which both
	// installs the on-link route on the client AND makes the server's ingress ACL
	// accept peer-vIP destinations (no per-peer resource grant needed).
	clientId := ctx.Value("clientId").(int64)
	peerAddr, perr := service.AssignIPToClient(setupCtx, clientId)
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("Assigned IP %s to client %d", peerAddr, clientId))
	}
	if perr != nil {
		return fmt.Errorf("Failed to get available IP: %w", perr)
	}
	addr, e := netip.ParseAddr(peerAddr)
	if e != nil {
		return fmt.Errorf("Failed to parse address: %w", e)
	}
	ip4 := addr.Unmap().As4()

	sess := &xdp.TunnelSession{
		Conn:        utility.NewConnectIPAdapter(conn, tunChan),
		InnerIP:     net.IP(ip4[:]),
		TunnelIndex: tunIdx,
	}
	sessionTable.Register(sess, tunCount)
	defer sessionTable.RemoveSession(sess)
	bitmask := 32 // RFC 9484 ADDRESS_ASSIGN requires a canonical prefix; a host gets /32.
	ipPrefix := netip.PrefixFrom(addr, bitmask)
	if err := conn.AssignAddresses(setupCtx, []netip.Prefix{ipPrefix}); err != nil {
		return fmt.Errorf("failed to assign addresses: %w", err)
	}
	mu.Lock()
	{
		key := addr.Unmap()
		chans := ipToTunChan[key]
		if len(chans) != tunCount {
			grown := make([]chan *utility.Packet, tunCount)
			copy(grown, chans)
			chans = grown
		}
		if tunIdx >= 0 && tunIdx < tunCount {
			chans[tunIdx] = tunChan
		}
		ipToTunChan[key] = chans
	}
	mu.Unlock()
	clientResources, cerr := service.GetClientResources(setupCtx, clientId)
	if cerr != nil {
		return cerr
	}
	clientRoutes := []connectip.IPRoute{}
	for i := 0; i < len(*clientResources); i++ {
		r, e := netip.ParsePrefix((*clientResources)[i].Value)
		if e != nil {
			continue
		}
		connectipRoute := connectip.IPRoute{StartIP: r.Addr(), EndIP: utility.LastIPAddr(r), IPProtocol: ipProtocol}
		clientRoutes = append(clientRoutes, connectipRoute)
	}
	// Client-to-client reachability is governed ENTIRELY by the resource/role model,
	// NOT hardcoded: client A can reach client B only if A is granted a resource that
	// covers B's vIP (and B granted one covering A's, for the return path). That grant
	// is what installs the route on the client AND makes connect-ip's ingress ACL
	// accept the peer dst. Only then does forwardOne's branch-2 deliver the packet
	// down B's tunnel. We do NOT advertise VIRT_CIDR by default — that would make
	// every client reachable to every other, breaking isolation.
	if err := conn.AdvertiseRoute(setupCtx, clientRoutes); err != nil {
		return fmt.Errorf("failed to advertise route: %w", err)
	}

	errChan := make(chan error, 2)
	// pktChan: upload-direction packets from quic.ReadPacket headed to either
	// AF_XDP forward (NAT) or tun0 local-delivery. Old 1024 dropped ~4.7k at
	// P100 upload bursts. 8192 = ~10MB at MTU; bounded, easily absorbed.
	pktChan := make(chan *utility.Packet, 8192)
	logPacketStr, _ := ctx.Value("LOG_PACKET").(string)
	logPacket, _ := strconv.ParseBool(logPacketStr)
	// Download-path inner-TCP-seq resequencer (Phase 4). Off by default (net-negative
	// in testing); enable with FORWARD_RESEQ=true in the config.
	reseqEnabled := false
	if v, _ := ctx.Value("FORWARD_RESEQ").(string); v != "" {
		reseqEnabled, _ = strconv.ParseBool(v)
	}

	go func() {
		// reader goroutine — read connect-ip's decapped inner IP packet DIRECTLY into
		// the pooled buffer (no stack-buffer + copy); saves one memmove per upload pkt.
		go func() {
			for {
				p := utility.PacketPool.Get().(*utility.Packet)
				n, err := conn.ReadPacket(p.Buf)
				if err != nil {
					utility.PacketPool.Put(p)
					close(pktChan)
					errChan <- err
					return
				}
				p.N = n
				select {
				case pktChan <- p:
				default:
					// pktChan full — drop, TCP will retransmit
					utility.PacketPool.Put(p)
					pktChanDrops.Add(1)
				}
			}
		}()

		pump, srcMAC, dstMAC, _ := afxdpConn.ForwardPump()
		batch, err := utility.NewForwardBatch(pump, srcMAC, dstMAC, afxdpConn.WanIfindex(), tunTapDevice[0].GSOFd())
		if err != nil {
			errChan <- fmt.Errorf("failed to create forward batch: %w", err)
			return
		}
		defer batch.Close()

		// Per-egress-NIC forward batches for the multi-NIC LAN fast path (branch-3),
		// indexed parallel to egressNICs. ForwardBatch is single-owner, so each writer
		// goroutine (one per connection) builds its own. The primary/WAN slot reuses
		// `batch`. In tun-GSO/vhost mode every batch writes the (already-SNAT'd) packet
		// to the main tun and the kernel ip_forwards it out the NIC by destination route;
		// in AF_XDP-TX mode each batch TXes out its own NIC's pump. A NIC whose batch
		// fails to build stays nil → forwardOne falls back to the kernel bridge for it.
		lanBatch := make([]*utility.ForwardBatch, len(egressNICs))
		for i := range egressNICs {
			if egressNICs[i].conn == afxdpConn {
				lanBatch[i] = batch
				continue
			}
			p, sm, dm, _ := egressNICs[i].conn.ForwardPump()
			lb, lerr := utility.NewForwardBatch(p, sm, dm, egressNICs[i].conn.WanIfindex(), tunTapDevice[0].GSOFd())
			if lerr != nil {
				if logger.ShouldLog(logger.WARN) {
					logger.Warn(fmt.Sprintf("LAN forward batch for %s failed (kernel-bridge fallback): %v", egressNICs[i].name, lerr))
				}
				continue
			}
			lanBatch[i] = lb
			defer lb.Close()
		}
		// flushLAN / lanFull operate on the non-primary LAN batches (the primary is
		// flushed via `batch`). Cheap no-ops when there is only the WAN NIC.
		flushLAN := func() {
			for i := range lanBatch {
				lb := lanBatch[i]
				if lb == nil || lb == batch || lb.Empty() {
					continue
				}
				if err := lb.Flush(); err != nil && logger.ShouldLog(logger.ERROR) {
					logger.Error(fmt.Sprintf("LAN forward flush (%s): %v", egressNICs[i].name, err))
				}
			}
		}
		lanFull := func() bool {
			for i := range lanBatch {
				lb := lanBatch[i]
				if lb == nil || lb == batch {
					continue
				}
				if lb.Full() {
					return true
				}
			}
			return false
		}

		// Upload-path inner-TCP-seq resequencer (FORWARD_UPLOAD_RESEQ). connect-ip
		// carries inner packets over UNORDERED QUIC datagrams, so the forward input is
		// ~7% reordered (measured: dg_rcvin_ooo). Individual-frame forwarding tolerates
		// that, but GSO coalescing turns scattered 1-packet reorder into super-frame-
		// sized gaps that collapse the inner-TCP cwnd. Resequencing by inner TCP seq
		// BEFORE coalescing restores an in-order stream.
		// DEFAULT = ON whenever the forward egress coalesces (tun-GSO/vhost — the
		// bench-validated pairing); overridable via FORWARD_UPLOAD_RESEQ in the env
		// (highest precedence, no rebuild) or tmasqued.conf. window/maxAge env-tunable.
		uploadReseqOn := utility.ForwardCoalesces()
		if v := os.Getenv("FORWARD_UPLOAD_RESEQ"); v != "" {
			uploadReseqOn = v == "1" || v == "true"
		} else if v, ok := ctx.Value("FORWARD_UPLOAD_RESEQ").(string); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				uploadReseqOn = b
			}
		}
		var uploadReseq *utility.ForwardReseq
		if uploadReseqOn {
			win := 64
			if s := os.Getenv("FORWARD_UPLOAD_RESEQ_WINDOW"); s != "" {
				if n, e := strconv.Atoi(s); e == nil && n > 0 {
					win = n
				}
			}
			maxAge := 2 * time.Millisecond
			if s := os.Getenv("FORWARD_UPLOAD_RESEQ_MAXAGE_US"); s != "" {
				if n, e := strconv.Atoi(s); e == nil && n > 0 {
					maxAge = time.Duration(n) * time.Microsecond
				}
			}
			uploadReseq = utility.NewForwardReseq(win, maxAge)
			if logger.ShouldLog(logger.INFO) {
				logger.Info(fmt.Sprintf("upload-forward reseq ON: window=%d maxAge=%v", win, maxAge))
			}
		}
		var reseqOut [][]byte
		// Measures inner-TCP order on the post-reseq stream just before egress
		// (upload_postreseq_ooo at /debug/vars) — localizes residual reorder vs the
		// input (dg_rcvin_ooo) and the target's recv-OFO.
		uploadObs := utility.NewUploadOrderObserver()
		// FORWARD_LAN_XDP (default ON) gives the LAN bridge the WAN's fast path: userspace
		// SNAT to the egress NIC's IP + that NIC's XDP NAT-reverse for the return, instead
		// of the kernel masquerade/conntrack whose single return path is the LAN's
		// across-client anti-scaling. =0 restores the transparent kernel bridge (no SNAT)
		// for A/B and instant rollback without a redeploy. A subnet with no XDP NIC
		// (docker0, a coexisting VPN) always uses the kernel bridge regardless.
		lanXDP := ctxBoolDefault(ctx, "FORWARD_LAN_XDP", true)
		// The AF_XDP-TX and AF_PACKET egresses build the L2 frame in userspace and so need
		// the destination MAC; the tun-GSO/vhost egress lets the kernel ARP, so skip it.
		lanEgressNeedsMAC := !utility.ForwardCoalesces()

		// forwardOne is the forward ROUTER for ONE inner IP packet (already in send
		// order). It is a 4-way decision, not a binary one, so the gateway can bridge
		// WAN, the VPN overlay, and local LAN(s):
		//   1) destined to the server itself      → host stack (tun0 write)
		//   2) another VPN client's vIP            → straight down that peer's tunnel,
		//      NO SNAT (inner src stays the sender's vIP → the reply rides the peer's
		//      own tunnel back, never touching the underlay — the only c2c path that
		//      survives a source-spoofing-checked fabric). Offline peer → drop.
		//   3) a directly-connected LAN subnet     → forward transparently out that NIC
		//      via the kernel (ip_forward, NO SNAT) — clients see real LAN hosts.
		//   4) everything else (WAN / internet)    → SNAT + forward egress.
		forwardOne := func(ip []byte) {
			dst, _ := netip.AddrFromSlice(ip[16:20])
			dst = dst.Unmap()
			// 1) us
			if dst == wanAddr || dst == serverTunIP {
				tunTapDevice[0].Write(ip)
				return
			}
			// 2) another VPN client
			if virtPrefix.Contains(dst) {
				mu.RLock()
				tc := pickTunnel(ipToTunChan[dst], ip)
				mu.RUnlock()
				if tc == nil {
					c2cNoPeer.Add(1) // peer offline; a vIP is unroutable on WAN — drop
					return
				}
				p := utility.PacketPool.Get().(*utility.Packet)
				p.N = copy(p.Buf, ip)
				select {
				case tc <- p:
					c2cSends.Add(1)
				default:
					utility.PacketPool.Put(p)
					tunChanDrops.Add(1)
				}
				return
			}
			// 3) local LAN (bridge)
			for i := range lanRoutes {
				lr := &lanRoutes[i]
				if !lr.prefix.Contains(dst) {
					continue
				}
				// Kernel bridge: toggle off, no XDP NIC owns the subnet, or its batch
				// failed to build → transparent ip_forward + masquerade (the prior path).
				if !lanXDP || lr.egIdx < 0 || lanBatch[lr.egIdx] == nil {
					tunTapDevice[0].Write(ip)
					lanForwards.Add(1)
					return
				}
				eg := &egressNICs[lr.egIdx]
				// Userspace SNAT to the egress NIC's IP (idempotent per flow → stable
				// WAN port) so the NIC's XDP NAT-reverse owns the return, off the kernel
				// conntrack path. Then egress via that NIC's batch.
				if err := xdp.ApplySNAT(ip, eg.addr, natTable); err != nil {
					snatFailDrops.Add(1)
					lanForwards.Add(1)
					return
				}
				var mac net.HardwareAddr
				if lanEgressNeedsMAC {
					if mac = eg.conn.LanNextHopMAC(ip[16:20]); mac == nil {
						// Cold on-link target: the kernel ip_forwards the already-SNAT'd
						// packet (ARPs + delivers); an async probe primes the fast path.
						tunTapDevice[0].Write(ip)
						lanForwards.Add(1)
						return
					}
				}
				if err := lanBatch[lr.egIdx].Add(ip, mac); err != nil {
					fwdAddDrops.Add(1)
				}
				lanForwards.Add(1)
				return
			}
			// 4) WAN / internet
			uploadObs.Observe(ip) // post-reseq order, pre-SNAT
			if err := xdp.ApplySNAT(ip, wanAddr, natTable); err != nil {
				snatFailDrops.Add(1)
			} else if err := batch.Add(ip, afxdpConn.NextHopMACForIP(ip[16:20])); err != nil {
				fwdAddDrops.Add(1)
			}
		}

		// handlePkt consumes one pooled packet: through the resequencer if enabled
		// (emitting whatever is now in order), else straight to forwardOne. The pooled
		// buffer is safe to recycle on return (batch.Add and the reseq buffer both copy).
		handlePkt := func(pkt *utility.Packet, now time.Time) {
			if logPacket && logger.ShouldLog(logger.INFO) {
				logger.Info(fmt.Sprintf("TUN -> WAN: read %d bytes, payload = %x", pkt.N, pkt.Buf[:pkt.N]))
			}
			if uploadReseq != nil {
				reseqOut = uploadReseq.Push(pkt.Buf[:pkt.N], now, reseqOut[:0])
				for _, ip := range reseqOut {
					forwardOne(ip)
				}
			} else {
				forwardOne(pkt.Buf[:pkt.N])
			}
			utility.PacketPool.Put(pkt)
		}

		// FORWARD_WRITER_PIN=1 (experiment): pin this forward-writer goroutine to a
		// fixed CPU (LockOSThread + sched_setaffinity). The whole kernel forward path
		// (tun write → ip_forward → NIC TX) runs inline on this goroutine's CPU, and
		// XPS maps CPU→txq — an unpinned writer migrates, flapping the TX queue,
		// which on RSS-less virtio flaps the host's mirrored RX steering (spray).
		// Pinning stabilizes CPU → txq → host steering, and keeps caches warm.
		if os.Getenv("FORWARD_WRITER_PIN") == "1" {
			runtime.LockOSThread()
			cpu := int(writerPinSeq.Add(1)-1) % runtime.NumCPU()
			var set unix.CPUSet
			set.Set(cpu)
			if err := unix.SchedSetaffinity(0, &set); err == nil {
				if logger.ShouldLog(logger.INFO) {
					logger.Info(fmt.Sprintf("forward writer pinned to cpu %d", cpu))
				}
			}
		}
		ticker := time.NewTicker(250 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case pkt, ok := <-pktChan:
				if !ok {
					batch.Flush()
					flushLAN()
					errChan <- fmt.Errorf("pktChan closed")
					return
				}
				now := time.Now()
				roundN := 1
				handlePkt(pkt, now)
				for len(pktChan) > 0 && !batch.Full() && !lanFull() {
					handlePkt(<-pktChan, now)
					roundN++
				}
				fwdRounds.Add(1)
				fwdRoundPkts.Add(int64(roundN))
				if uploadReseq != nil {
					reseqOut = uploadReseq.FlushExpired(now, reseqOut[:0])
					for _, ip := range reseqOut {
						forwardOne(ip)
					}
				}
				if !batch.Empty() {
					if err := batch.Flush(); err != nil {
						if logger.ShouldLog(logger.ERROR) {
							logger.Error(fmt.Sprintf("sendmmsg error: %v", err))
						}
					}
				}
				flushLAN()
			case <-ticker.C:
				if uploadReseq != nil {
					reseqOut = uploadReseq.FlushExpired(time.Now(), reseqOut[:0])
					for _, ip := range reseqOut {
						forwardOne(ip)
					}
				}
				if err := batch.Flush(); err != nil {
					if logger.ShouldLog(logger.ERROR) {
						logger.Error(fmt.Sprintf("sendmmsg error: %v", err))
					}
				}
				flushLAN()
			}
		}
	}()

	timer := time.NewTimer(1 * time.Millisecond)
	defer timer.Stop()
	go func() {
		reseq := utility.NewForwardReseq(64, 5*time.Millisecond)
		// Measure inner-TCP-seq order as the server frames it (vs the in-order XDP
		// ingress) to localize where the download reorder enters. Pure measurement.
		// obs = highwater-only (conflates retransmits as OOO);
		// genObs = per-flow seen-set, splits retransmit vs genuine reorder for direct
		// comparison against client-side tun0 tcpdump.
		obs := utility.NewPreReseqObserver()
		genObs := utility.NewPreSendGenuineObserver()
		var out [][]byte

		// writeOut sends each resequenced packet via WritePacket. It returns false
		// only when the conn is closed (caller should exit the goroutine). A full
		// datagram queue drops that packet; L4 retransmits.
		writeOut := func(pkts [][]byte) bool {
			for _, b := range pkts {
				icmp, err := conn.WritePacket(b)
				if err != nil {
					if errors.Is(err, net.ErrClosed) {
						select {
						case errChan <- err:
						default:
						}
						return false
					}
					continue
				}
				if len(icmp) > 0 {
					pump, srcMAC, _, _ := afxdpConn.ForwardPump()
					// Address the ICMP error to its destination's learned next hop
					// (icmp[16:20] = dst IP), gwMAC fallback, like the bulk egress path.
					dstMAC := afxdpConn.NextHopMACForIP(icmp[16:20])
					if err := utility.ForwardSendOne(pump, srcMAC, dstMAC, icmp); err != nil {
						if logger.ShouldLog(logger.ERROR) {
							logger.Error(fmt.Sprintf("failed to send ICMP: %v", err))
						}
					}
				}
			}
			return true
		}

		for {
			select {
			case pkt, ok := <-tunChan:
				if !ok {
					select {
					case errChan <- fmt.Errorf("tunChan closed"):
					default:
					}
					return
				}
				if logPacket {
					if logger.ShouldLog(logger.INFO) {
						logger.Info(fmt.Sprintf("WAN -> TUN: read %d bytes, payload = %x", pkt.N, pkt.Buf[:pkt.N]))
					}
				}
				if stats.ShouldLog() {
					obs.Observe(pkt.Buf[:pkt.N])
					genObs.Observe(pkt.Buf[:pkt.N])
				}
				// Resequence by inner TCP seq, then write every now-ready packet.
				// out may reference pkt.Buf directly, so write before recycling pkt.
				if reseqEnabled {
					out = reseq.Push(pkt.Buf[:pkt.N], time.Now(), out[:0])
				} else {
					out = append(out[:0], pkt.Buf[:pkt.N])
				}
				alive := writeOut(out)
				utility.PacketPool.Put(pkt)
				if !alive {
					return
				}
			case <-timer.C:
				// Flush flows stalled on a genuinely missing segment.
				if reseqEnabled {
					out = reseq.FlushExpired(time.Now(), out[:0])
					if !writeOut(out) {
						return
					}
				}
				timer.Reset(1 * time.Millisecond)
			}
		}
	}()

	err := <-errChan
	if logger.ShouldLog(logger.ERROR) {
		logger.Error(fmt.Sprintf("handleConn exiting for client addr=%s err=%v", addr, err))
	}
	mu.Lock()
	{
		key := addr.Unmap()
		chans := ipToTunChan[key]
		if tunIdx >= 0 && tunIdx < len(chans) {
			chans[tunIdx] = nil
		}
		live := false
		for _, c := range chans {
			if c != nil {
				live = true
				break
			}
		}
		if !live {
			delete(ipToTunChan, key)
		}
	}
	mu.Unlock()
	close(tunChan)
	for pkt := range tunChan {
		utility.PacketPool.Put(pkt)
	}
	conn.Close()
	<-errChan // wait for the other goroutine to finish
	return err
}

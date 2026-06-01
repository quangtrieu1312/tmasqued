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
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"os/exec"
	"os/signal"
    "syscall"
    "runtime"
	"sync/atomic"
	_ "net/http/pprof"

	connectip "github.com/quic-go/connect-ip-go"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
	"github.com/yosida95/uritemplate/v3"

	xdp "github.com/quangtrieu1312/tmasqued/xdp"
    "github.com/quangtrieu1312/tmasqued/constants"
    "github.com/quangtrieu1312/tmasqued/utility"
    "github.com/quangtrieu1312/tmasqued/db"
    "github.com/quangtrieu1312/tmasqued/config"
    "github.com/quangtrieu1312/tmasqued/logger"
    "github.com/quangtrieu1312/tmasqued/stats"
    "github.com/quangtrieu1312/tmasqued/migration"
    "github.com/quangtrieu1312/tmasqued/service"
)
var tunTapDevice []*water.Interface
var mu *sync.RWMutex
// ipToTunChan maps a client's inner IP to its bonded tunnels' tunChans, indexed
// by tunnel index. A download packet is pinned to one tunnel by 5-tuple hash
// (ECMP/LACP-style) so a flow never crosses tunnels (no cross-tunnel reorder)
// while different flows spread across tunnels/cores.
var ipToTunChan map[netip.Addr][]chan *utility.Packet
var afxdpConn *xdp.Conn
var serverTunIP netip.Addr
var tunChanDrops atomic.Uint64
var pktChanDrops atomic.Uint64

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
	datagramOverhead = 52
	// maxQUICPacket == quic-go protocol.MaxPacketBufferSize in our fork (buffer cap).
	maxQUICPacket = 4000
)

// tunLoopSends counts download packets pushed into tunChan via the TUN-read
// loop (XDP_PASS → kernel → TUN fallback, the ipToTunChan/pickTunnel path).
// If this is nonzero during a download test, a flow is split between this
// producer and the AF_XDP one (utility.TunAdapterSends) → reorder source.
var tunLoopSends atomic.Uint64

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
	bindTo := netip.AddrPortFrom( bindAddr, uint16(listenPort))

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
    	if !ok { continue }
    	if !a.IsLinkLocalUnicast() {
        	wanAddr = a.Unmap()
        	break
    	}
	}
	if !wanAddr.IsValid() {
    	logger.Fatal(fmt.Sprintf("no usable IP on %s", ifaceName))
	}
	netBitSize, _ := virtSubnet.Mask.Size()
	devs, err := createTunTapDevice(ctx, virtIP, netBitSize, int(mtu))
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to create tun/tap device: %v", err))
	}
    tunTapDevice = devs

	upChan := make(chan bool)
    go func(ctxt context.Context) {
        for {
            isRunning := <- upChan
            if (isRunning) {
                RunPostUp(ctxt)
            } else {
                GracefullyShutDown(ctxt)
            }
        }
    }(ctx)
    Bootstrap(ctx)
	if err := run(ctx, upChan, bindTo, uint8(ipProtocol)); err != nil {
		logger.Fatal(fmt.Sprintf("%v",err))
	}
    if logger.ShouldLog(logger.INFO) { logger.Info("Shutting down masque server.") }
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
        if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: could not raise to %d (%v); leaving %d", key, target, err, cur)) }
        return
    }
    if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: %d -> %d (UDP buffer for QUIC transport)", key, cur, target)) }
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
        if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: could not read (%v); leaving as-is", key, err)) }
        return
    }
    fields := strings.Fields(strings.TrimSpace(string(b)))
    if len(fields) != 3 {
        if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: unexpected format %q; leaving as-is", key, string(b))) }
        return
    }
    curMax, _ := strconv.Atoi(fields[2])
    if curMax >= targetMax {
        return
    }
    if err := os.WriteFile(path, []byte(fmt.Sprintf("%s %s %d", fields[0], fields[1], targetMax)), 0644); err != nil {
        if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: could not raise max to %d (%v); leaving %d", key, targetMax, err, curMax)) }
        return
    }
    if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("sysctl %s: max %d -> %d (inner-TCP BDP over tunnel)", key, curMax, targetMax)) }
}

// tuneInnerTCPBuffers raises the inner application TCP autotuning ceilings so a
// single TCP stream over the tunnel can fill the tunnel's larger BDP. Raise-only;
// min/default preserved so idle sockets stay small.
func tuneInnerTCPBuffers() {
    raiseSysctlTriple("net.ipv4.tcp_wmem", innerTCPBufTarget)
    raiseSysctlTriple("net.ipv4.tcp_rmem", innerTCPBufTarget)
}

func Bootstrap(ctx context.Context) {
    if logger.ShouldLog(logger.INFO) { logger.Info("Server in bootstrap phase") }
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
    if logger.ShouldLog(logger.INFO) { logger.Info("Migrating data") }
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
    if logger.ShouldLog(logger.INFO) { logger.Info("Server in post-up phase") }
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
				fwdDrops    := afxdpConn.FwdDrops()
				txDrops     := afxdpConn.TxDrops()
				tunDrops    := tunChanDrops.Load()
				pktDrops    := pktChanDrops.Load()
				adapterDrops := utility.TunAdapterDrops.Load()
				adapterSends := utility.TunAdapterSends.Load()
				tunLoopSnd   := tunLoopSends.Load()
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
				if v := expvar.Get("dg_packer_total"); v != nil { pkTot = v.(*expvar.Int).Value() }
				if v := expvar.Get("dg_packer_genuine"); v != nil { pkGen = v.(*expvar.Int).Value() }
				if v := expvar.Get("dg_packer_retr"); v != nil { pkRetr = v.(*expvar.Int).Value() }
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
    if logger.ShouldLog(logger.INFO) { logger.Info("Server in pre-down phase") }
    cmd := exec.Command("/bin/bash", "-c", constants.PREDOWN_SCRIPT_PATH)
    _, err := cmd.Output()
    if err != nil {
        logger.Fatal(fmt.Sprintf("Cannot run predown scripts: %v", err))
    }
}

func GracefullyShutDown(ctx context.Context) {
    if logger.ShouldLog(logger.INFO) { logger.Info("Shutting down") }
    db.CloseConnection()
}

func createTunTapDevice(ctx context.Context, virtIp string, virtPrefixLen int, mtu int) ([]*water.Interface, error) {
    numQueues := runtime.NumCPU()
    // TUN_QUEUES overrides the tun-read goroutine count (default = NumCPU). Set to 1
    // to serialize tun ingest when diagnosing download reorder.
    if v := os.Getenv("TUN_QUEUES"); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 {
            numQueues = n
        }
    }
    devs := make([]*water.Interface, numQueues)

	// First device — let OS assign name
	var err error
	devs[0], err = water.New(water.Config{
    	DeviceType: water.TUN,
    	PlatformSpecificParams: water.PlatformSpecificParams{
        	MultiQueue: true,
    	},
	})
	if err != nil {
    	return nil, fmt.Errorf("failed to create TUN device queue 0: %w", err)
	}
	devName := devs[0].Name()
	if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("Created TUN device: %s", devName)) }
	// Subsequent queues — MUST use same name
	for i := 1; i < numQueues; i++ {
    	dev, err := water.New(water.Config{
        	DeviceType: water.TUN,
        	PlatformSpecificParams: water.PlatformSpecificParams{
            	Name:       devName, // same device, new fd
            	MultiQueue: true,
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
        return  nil, fmt.Errorf("Failed to parse address: %w", err)
    }
    ip := clientSubnet.IP.String()
    bitmask, _ := clientSubnet.Mask.Size()
    prefixAddr, err := netip.ParsePrefix(ip + "/" + strconv.Itoa(bitmask))
    if err != nil {
        return  nil, fmt.Errorf("Failed to parse prefix: %w", err)
    }
    route := &netlink.Route{ LinkIndex: link.Attrs().Index, Dst: utility.PrefixToIPNet(prefixAddr) }
	if err := netlink.RouteAdd(route); err != nil && errors.Is(err, syscall.EEXIST){
		if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("Route %v already exists, skipping", route)) }
	} else if err != nil {
		return nil, fmt.Errorf("Failed to add route %v: %w", route, err)
	}

	return devs, nil
}


func run(ctxt context.Context, upChan chan<- bool, bindTo netip.AddrPort, ipProtocol uint8) error {
    ctx, cancel := context.WithCancel(ctxt)
    defer cancel()
	ifaceName := ctxt.Value("WAN_INTERFACE").(string)
	xdpLoader, err := xdp.Load(ifaceName)
	if err != nil {
		return fmt.Errorf("loading XDP program: %w", err)
	}
	defer xdpLoader.Close()
	if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("XDP mode: %s", xdpLoader.Mode())) }
	natTable, err := xdp.OpenNatTable()
	if err != nil {
    	return fmt.Errorf("opening NAT table: %w", err)
	}
	defer natTable.Close()

	    link, err := netlink.LinkByName(ifaceName)
    if err != nil {
        return fmt.Errorf("failed to get %s interface: %w", ifaceName, err)
    }
    addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
    if err != nil {
        return fmt.Errorf("failed to get addresses for %s: %w", ifaceName, err)
    }
    var wanAddr netip.Addr
    for _, addr := range addrs {
        a, ok := netip.AddrFromSlice(addr.IP)
        if !ok { continue }
        if !a.IsLinkLocalUnicast() {
            wanAddr = a.Unmap()
            break
        }
    }
    if !wanAddr.IsValid() {
        return fmt.Errorf("no usable IP on %s", ifaceName)
    }
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("interface %s: %w", ifaceName, err)
	}
	localAddr := &net.UDPAddr{IP: bindTo.Addr().AsSlice(), Port: int(bindTo.Port())}
	// Auto-detect + maximize NIC combined channels BEFORE binding AF_XDP. This
	// frees all hardware TX rings the driver can offer, so the datagram TX
	// path can fan out across them. On a fresh boot the OCI NICs typically
	// report combined=1 even though the hardware supports more — auto-setting
	// here makes the perf path symmetric with WireGuard's multi-queue TX.
	// Failure is non-fatal: we fall back to whatever the soft limit was.
	finalCombined, err := xdp.MaximizeChannels(ifaceName)
	if err != nil {
		if logger.ShouldLog(logger.INFO) {
			logger.Info(fmt.Sprintf("NIC %s: MaximizeChannels best-effort: %v (continuing with %d queue)",
				ifaceName, err, finalCombined))
		}
	}
	nicQueues, err := xdp.GetQueueInfo(ifaceName)
	if err != nil {
		return fmt.Errorf("reading NIC queue info for %s: %w", ifaceName, err)
	}
	if logger.ShouldLog(logger.INFO) {
		logger.Info(fmt.Sprintf("NIC %s: combined channels=%d active RX queues=%d — datagram TX will use %d buckets",
			ifaceName, finalCombined, nicQueues.RX, finalCombined))
	}

	// XDP_USE_NEED_WAKEUP skips the per-batch sendto kick on the TX path. On by
	// default; kill switch: set XDP_NEED_WAKEUP=false in the config.
	needWakeup := true
	if v, _ := ctxt.Value("XDP_NEED_WAKEUP").(string); v != "" {
		needWakeup, _ = strconv.ParseBool(v)
	}
	afxdpConn, err = xdp.NewConn(iface, xdpLoader.XskQuicMap(), xdpLoader.XskFwdMap(), localAddr, nicQueues.RX, xdpLoader.Mode(), needWakeup)
	if err != nil {
		return fmt.Errorf("creating AF_XDP conn: %w", err)
	}
	defer afxdpConn.Close()
	sessionTable := xdp.NewSessionTable()
	afxdpConn.SetForwardHandler(sessionTable.Deliver)
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
        return fmt.Errorf("Cannot read client CA:", err)
	}
	ok := certPool.AppendCertsFromPEM(caCertPEM)
    if !ok {
		return fmt.Errorf("Invalid cert")
	}
	template := uritemplate.MustNew(fmt.Sprintf("https://tmasqued:%d/vpn", bindTo.Port()))
	serverConf := &tls.Config{
	    Certificates:          []tls.Certificate{cert},
	    ClientAuth:            tls.RequireAndVerifyClientCert,
	    ClientCAs:             certPool,
	}
    ln, err := quic.ListenEarly(
		afxdpConn,
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
            DatagramSendBuckets: finalCombined,
            MaxIdleTimeout: 30 * time.Second,
            KeepAlivePeriod: 10 * time.Second,
			InitialStreamReceiveWindow:     10 * 1024 * 1024,  // 10 MB
    		MaxStreamReceiveWindow:         10 * 1024 * 1024,  // 10 MB
    		InitialConnectionReceiveWindow: 15 * 1024 * 1024,  // 15 MB
    		MaxConnectionReceiveWindow:     15 * 1024 * 1024,  // 15 MB
			DisablePathMTUDiscovery: true,
			MaxIncomingStreams: 0,
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
                	if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("queue#%d cannot read TUN/TAP device %v: %v", id, d.Name(), err)) }
            		cancel()
            		break
        		}
        		pkt.N = n
                // assuming we are only doing IPv4
                destIP, ok := netip.AddrFromSlice(pkt.Buf[16:20])
                if ! ok {
            		utility.PacketPool.Put(pkt) // return on error path too
					if logger.ShouldLog(logger.TRACE) {
				    	logger.Trace(fmt.Sprintf("queue#%d cannot parse data to IP. Dropping packet.", id))
					}
                    continue
                }
				if logger.ShouldLog(logger.TRACE) {
					logger.Trace(fmt.Sprintf("queue#%d dest IP to filter %v",id, destIP.String()))
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
		fmt.Printf("DEBUG /vpn handler reached, TLS peer certs: %d\n", len(r.TLS.PeerCertificates))
        commonName := r.TLS.PeerCertificates[0].Subject.CommonName
    	clientId, err := strconv.ParseInt(commonName, 10, 64)
		if err != nil {
			if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("Got invalid TLS common name %v: %v", commonName, err)) }
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
			if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("failed to handle connection: %v", err)) }
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

func handleConn(ctx context.Context, tunChan chan *utility.Packet,  conn *connectip.Conn, ipProtocol uint8, natTable *xdp.NatTable, wanAddr netip.Addr, sessionTable *xdp.SessionTable, tunIdx, tunCount int) error {
	setupCtx, setupCancel := context.WithTimeout(ctx, 5*time.Second)
	defer setupCancel()
	if logger.ShouldLog(logger.DEBUG) {
    	logger.Debug("Start connectip flow")
	}
    // Get the next unassigned address
    // And assign prefix = IP/32 to the client
    // Note:
    // We can assign any subnet size here but I'm using /32 for simplicity
    // I may want to go back to this hardcoded number when I see issues for site-to-side VPN
    clientId := ctx.Value("clientId").(int64)
    peerAddr, perr := service.AssignIPToClient(setupCtx, clientId)
	if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("Assigned IP %s to client %d", peerAddr, clientId)) }
    if perr != nil {
        return fmt.Errorf("Failed to get available IP: %w", perr)
    }
    addr, e := netip.ParseAddr(peerAddr)
    if e != nil {
        return fmt.Errorf("Failed to parse address: %w", e)
    }
	ip4 := addr.Unmap().As4()

	sess := &xdp.TunnelSession{
    	Conn: utility.NewConnectIPAdapter(conn, tunChan),
    	InnerIP: net.IP(ip4[:]),
    	TunnelIndex: tunIdx,
	}
    sessionTable.Register(sess, tunCount)
    defer sessionTable.RemoveSession(sess)
    bitmask := 32
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
	
		sock, srcMAC, dstMAC, genericMode, txMu := afxdpConn.ForwardSocket()
		batch, err := utility.NewForwardBatch(sock, srcMAC, dstMAC, genericMode, txMu)
		if err != nil {
    		errChan <- fmt.Errorf("failed to create forward batch: %w", err)
    		return
		}
		defer batch.Close()
    	ticker := time.NewTicker(5 * time.Millisecond)
    	defer ticker.Stop()
    	for {
        	select {
        	case pkt, ok := <-pktChan:
            	if !ok {
                	batch.Flush()
					errChan <- fmt.Errorf("pktChan closed")
                	return
            	}
				if logPacket {
					if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("TUN -> WAN: read %d bytes, payload = %x", pkt.N, pkt.Buf[:pkt.N])) }
				}
				dst, _ := netip.AddrFromSlice(pkt.Buf[16:20])
				dst = dst.Unmap()

				if dst == wanAddr || dst == serverTunIP {
    				tunTapDevice[0].Write(pkt.Buf[:pkt.N])
    				utility.PacketPool.Put(pkt)
				} else {
					xdp.ApplySNAT(pkt.Buf[:pkt.N], wanAddr, natTable)
					batch.Add(pkt.Buf[:pkt.N], afxdpConn.NextHopMACForIP(pkt.Buf[16:20]))
					utility.PacketPool.Put(pkt)
				}
				for len(pktChan) > 0 && !batch.Full() {
                	pkt = <-pktChan
					dst, _ := netip.AddrFromSlice(pkt.Buf[16:20])
					dst = dst.Unmap()

					if dst == wanAddr || dst == serverTunIP {
    					tunTapDevice[0].Write(pkt.Buf[:pkt.N])
    					utility.PacketPool.Put(pkt)
					} else {
						xdp.ApplySNAT(pkt.Buf[:pkt.N], wanAddr, natTable)
						// dst IP (pkt.Buf[16:20]) is the target — unchanged by SNAT,
						// which only rewrites the source. Resolve its on-link MAC so the
						// frame goes direct instead of hairpinning through the gateway.
						batch.Add(pkt.Buf[:pkt.N], afxdpConn.NextHopMACForIP(pkt.Buf[16:20]))
						utility.PacketPool.Put(pkt)
					}
            	}
            	// Flush once the input channel is drained and the batch has anything,
            	// instead of waiting for the ticker. Under load the inner drain loop
            	// keeps the batch near-full so this still amortizes sendmmsg; for a
            	// download's sparse inner-TCP ACKs it removes the up-to-5ms idle wait
            	// that throttled the ACK-clocked remote sender (P1-dn). Full() ⊂ !Empty().
            	if !batch.Empty() {
                	if err := batch.Flush(); err != nil {
                    	if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("sendmmsg error: %v", err)) }
                	}
            	}
        	case <-ticker.C:
            	if err := batch.Flush(); err != nil {
                	if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("sendmmsg error: %v", err)) }
            	}
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
					sock, srcMAC, _, genericMode, txMu := afxdpConn.ForwardSocket()
					// Address the ICMP error to its destination's learned next hop
					// (icmp[16:20] = dst IP), gwMAC fallback, like the bulk egress path.
					dstMAC := afxdpConn.NextHopMACForIP(icmp[16:20])
					if err := utility.ForwardSendOne(sock, srcMAC, dstMAC, genericMode, txMu, icmp); err != nil {
						if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("failed to send ICMP: %v", err)) }
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
					if logger.ShouldLog(logger.INFO) { logger.Info(fmt.Sprintf("WAN -> TUN: read %d bytes, payload = %x", pkt.N, pkt.Buf[:pkt.N])) }
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
	if logger.ShouldLog(logger.ERROR) { logger.Error(fmt.Sprintf("handleConn exiting for client addr=%s err=%v", addr, err)) }
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

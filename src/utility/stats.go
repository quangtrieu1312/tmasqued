package utility

// BatchStats returns per-path flush/packet counters and mean batch sizes.
// Keeping them separate makes it easy to verify the XDP vs socket split at a glance.
func BatchStats() (xdpFlushes, xdpPackets int64, xdpAvg float64, sockFlushes, sockPackets int64, sockAvg float64) {
    xdpFlushes, xdpPackets, xdpAvg = XDPBatchStats()
    sockFlushes, sockPackets, sockAvg = SocketBatchStats()
    return
}

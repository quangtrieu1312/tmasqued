//go:build linux

package utility

import (
	"context"
	"time"

	"github.com/quangtrieu1312/tmasqued/config"
)

// LoadConfig applies the forward-path tuning knobs from the tmasqued.conf context to
// this package's settings. Call once at startup (after config.Load, before any
// NewForwardBatch/NewForwardReseq). Like SetForwardMode, the low-level forward
// packages have no ctx of their own, so main threads the config in here. Each knob
// defaults to the package var's current value when the key is absent from the config.
func LoadConfig(ctx context.Context) {
	forwardAckViaSocket = config.Bool(ctx, "FORWARD_ACK_VIA_SOCKET", forwardAckViaSocket)
	forwardViaPacket = config.Bool(ctx, "FORWARD_KERNEL_TX", forwardViaPacket)
	forwardViaUring = config.Bool(ctx, "FORWARD_TUN_URING", forwardViaUring)
	forwardViaNapiTun = config.Bool(ctx, "FORWARD_TUN_NAPI", forwardViaNapiTun)
	tunNoCoalesce = config.Bool(ctx, "FORWARD_TUN_NOCOALESCE", tunNoCoalesce)
	if n := config.Int(ctx, "FORWARD_TUN_GSO_MAXSEGS", tunMaxSegs); n >= 1 && n <= gsoMaxSegs {
		tunMaxSegs = n
	}
	if us := config.Int(ctx, "FORWARD_TUN_GSO_FLUSH_IDLE_US", int(flushIdle/time.Microsecond)); us > 0 {
		flushIdle = time.Duration(us) * time.Microsecond
	}
}

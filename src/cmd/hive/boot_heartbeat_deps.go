package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/hub"
)

// bootHeartbeatDeps are the long-lived effects bootHeartbeat performs (#7571,
// step 2): publishing the collect-independent identity and starting the two
// hub push loops. Everything bootHeartbeat hands those loops — the payload
// collector and the dozen hub→spoke callbacks — is captured by a test through
// the fake and driven directly, so the callbacks' reconciliation logic is
// tested without a hub or a ticker.
type bootHeartbeatDeps struct {
	publishIdentity     func(hiveID, org, primaryRepo string, repos []string, reporter, startedAt, gitHash string)
	startHeartbeat      func(ctx context.Context, hubURL string, collect hub.StatusCollector, interval time.Duration, logger *slog.Logger, callbacks ...any)
	startTaskStatusPush func(ctx context.Context, hubURL string, collect hub.TaskStatusCollector, logger *slog.Logger)
}

// heartbeatSendInterval is INDEPENDENT of the governor eval interval. It was
// previously tied to cfg.Governor.EvalIntervalS, so a low-ACMM hive (which
// evaluates infrequently by design — e.g. ~10 min at L2) beat the hub only
// every ~10 min. The hub marks a hive stale after heartbeatHealthStaleness
// (5 min), so such hives showed a gray/stale dot for half of every cycle
// despite being perfectly healthy. Beat on a fixed interval comfortably under
// that 5-min threshold so every hive, regardless of ACMM level, stays fresh
// on the hub.
const heartbeatSendInterval = 2 * time.Minute

func defaultBootHeartbeatDeps() bootHeartbeatDeps {
	return bootHeartbeatDeps{
		publishIdentity: hub.PublishHeartbeatIdentity,
		startHeartbeat: func(ctx context.Context, hubURL string, collect hub.StatusCollector, interval time.Duration, logger *slog.Logger, callbacks ...any) {
			go hub.StartHeartbeat(ctx, hubURL, collect, interval, logger, callbacks...)
		},
		startTaskStatusPush: func(ctx context.Context, hubURL string, collect hub.TaskStatusCollector, logger *slog.Logger) {
			go hub.StartTaskStatusPush(ctx, hubURL, collect, logger)
		},
	}
}

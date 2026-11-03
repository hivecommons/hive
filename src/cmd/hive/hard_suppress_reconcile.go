package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// hardSuppressReconcileGate throttles the hard-suppress reconcile
// (hivecommons/hive#12002) to issues.hard_suppress_reconcile_interval: each
// pass reads label events and timelines for every open hold/blocked issue,
// which is too much to repeat every eval cycle.
type hardSuppressReconcileGate struct {
	mu   sync.Mutex
	last time.Time
	now  func() time.Time
}

var hardSuppressReconcileSched = &hardSuppressReconcileGate{now: time.Now}

// due reports whether a pass should run now and, if so, records it.
func (g *hardSuppressReconcileGate) due(every time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if !g.last.IsZero() && now.Sub(g.last) < every {
		return false
	}
	g.last = now
	return true
}

func runHardSuppressReconcile(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger) {
	runHardSuppressReconcileWith(ctx, cfg, ghClient, logger, hardSuppressReconcileSched)
}

func runHardSuppressReconcileWith(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger, gate *hardSuppressReconcileGate) {
	if cfg == nil || ghClient == nil || !cfg.Issues.LiftHiveHardSuppressEnabled() {
		return
	}
	if !gate.due(cfg.Issues.EffectiveHardSuppressReconcileInterval()) {
		return
	}
	result, err := ghClient.ReconcileHiveHardSuppress(ctx, github.HardSuppressReconcileConfig{
		// ActiveRepositories, not Repositories: a paused repo gets no
		// hard-suppress lift written to it (#6203).
		Repos:    ghClient.ActiveRepositories(),
		AIAuthor: cfg.EffectiveAIAuthor(),
	})
	if err != nil {
		logger.Warn("hard-suppress reconcile incomplete", "error", err)
	}
	if result != nil {
		logger.Info("hard-suppress reconcile complete",
			"lifted", result.Lifted,
			"skipped", result.Skipped,
			"items", result.Items,
		)
	}
}

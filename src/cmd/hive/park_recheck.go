package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// parkRecheckSched throttles the park re-check (github.RecheckParkedIssues)
// to the hard-suppress reconcile cadence: each pass reads the timeline and
// comments of every open issue parked at stage/045-needs-decision.
var parkRecheckSched = &hardSuppressReconcileGate{now: time.Now}

func runParkRecheck(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger) {
	runParkRecheckWith(ctx, cfg, ghClient, logger, parkRecheckSched)
}

func runParkRecheckWith(ctx context.Context, cfg *config.Config, ghClient *github.Client, logger *slog.Logger, gate *hardSuppressReconcileGate) {
	if cfg == nil || ghClient == nil {
		return
	}
	if !gate.due(cfg.Issues.EffectiveHardSuppressReconcileInterval()) {
		return
	}
	result, err := ghClient.RecheckParkedIssues(ctx, github.ParkRecheckConfig{Repos: ghClient.Repositories()})
	if err != nil {
		logger.Warn("park re-check incomplete", "error", err)
	}
	if result != nil {
		logger.Info("park re-check complete",
			"posted", result.Posted,
			"updated", result.Updated,
			"skipped", result.Skipped,
			"items", result.Items,
		)
	}
}

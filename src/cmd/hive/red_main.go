package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// redMainSweepInterval is the minimum spacing between red-main sweeps
// (hivecommons/hive#13630). The breaker trips after hours (6h by default), so
// a few minutes of detection latency is immaterial, and each pass costs a
// handful of reads per repo.
const redMainSweepInterval = 10 * time.Minute

// runRedMainSweepIfDue observes every active repo's default branch for
// required checks that are red, at most once per redMainSweepInterval. The
// tracker it feeds is what StampRedMain, the PR-request relay and the kick
// read; a failed pass leaves the previous observation in place.
func runRedMainSweepIfDue(ctx context.Context, ghClient *github.Client, lastRun *time.Time, logger *slog.Logger) {
	if ghClient == nil {
		return
	}
	now := time.Now()
	if lastRun != nil && !lastRun.IsZero() && now.Sub(*lastRun) < redMainSweepInterval {
		return
	}
	if lastRun != nil {
		*lastRun = now
	}
	res, err := ghClient.SweepRedMain(ctx, now)
	if err != nil {
		if logger != nil {
			logger.Warn("red-main sweep failed", "error", err)
		}
		return
	}
	if res.Red > 0 && logger != nil {
		logger.Info("red-main sweep: required checks red on a default branch", "repos", res.Repos, "red", res.Red, "skipped", res.Skipped)
	}
}

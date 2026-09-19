package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/policies"
)

// bootPoliciesDeps are bootPolicies' two effects (#7571, step 2): applying
// the ACMM pack through the dashboard (rewrites the agent roster and saves
// hive.yaml) and starting the policy repo watcher (clones and polls). The
// ACMM boot plan itself is pure and runs for real.
type bootPoliciesDeps struct {
	applyPack          func(srv *dashboard.Server, level int) (*dashboard.ApplyPackResult, error)
	startPolicyWatcher func(ctx context.Context, repo, branch, subPath, localDir string, poll time.Duration, logger *slog.Logger) error
}

func defaultBootPoliciesDeps() bootPoliciesDeps {
	return bootPoliciesDeps{
		applyPack: (*dashboard.Server).ApplyPack,
		startPolicyWatcher: func(ctx context.Context, repo, branch, subPath, localDir string, poll time.Duration, logger *slog.Logger) error {
			return policies.NewWatcher(repo, branch, subPath, localDir, poll, logger).Start(ctx)
		},
	}
}

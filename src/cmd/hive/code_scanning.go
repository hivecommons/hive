package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// runCodeScanningSweep is the code_scanning sweep job: one pass of the
// opt-in code scanning sweep over every repo it is enabled on
// (src/docs/code-scanning.md). The worker already sheds it outside normal
// API budget mode; the client re-checks between repos.
func runCodeScanningSweep(ctx context.Context, ghClient *github.Client, cfg *config.Config, logger *slog.Logger) {
	if ghClient == nil || cfg == nil {
		return
	}
	res := ghClient.RunCodeScanningSweep(ctx, cfg, time.Now())
	if logger != nil && (res.Errors > 0 || res.BudgetSkipped) {
		logger.Info("code scanning sweep", "repos", res.Repos, "filed", res.Filed, "updated", res.Updated,
			"closed", res.Closed, "errors", res.Errors, "budget_skipped", res.BudgetSkipped)
	}
}

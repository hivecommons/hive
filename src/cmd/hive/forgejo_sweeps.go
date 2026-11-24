package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/forgesweep"
)

// forgejoSweepInterval is how often the Forgejo automerge and close-on-merge
// sweeps run.
const forgejoSweepInterval = 2 * time.Minute

// forgejoHoldLabel is the forge-neutral hold gate (forge.SetHold).
const forgejoHoldLabel = "hold"

// newForgejoSweepAPI builds the Forgejo client a sweep uses. It is a variable
// so tests can substitute a fake.
var newForgejoSweepAPI = forge.NewForgejoSweepAPI

// forgejoSweepAPIForRepo returns the Forgejo client for repo, refusing when
// the forgejo token would go to another host (CheckForgeTokenHost) or no
// token is configured: both sweeps write.
func forgejoSweepAPIForRepo(cfg *config.Config, repo string) (forge.SweepAPI, error) {
	if cfg.ForgeKindForRepo(repo) != config.ForgeForgejo {
		return nil, fmt.Errorf("repo %q is not on forgejo", repo)
	}
	if err := cfg.CheckForgeTokenHost(repo); err != nil {
		return nil, err
	}
	token := os.Getenv(cfg.Forgejo.TokenEnvName())
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%s is empty", cfg.Forgejo.TokenEnvName())
	}
	return newForgejoSweepAPI(token, forge.Options{BaseURL: cfg.ForgeURLForRepo(repo), Org: cfg.Project.Org})
}

// forgejoSweeperFor builds one pass's sweeper from cfg. Only repos whose kind
// has the sweeps (config.ForgeSweepsFor) are included; GitLab and Gitea
// repos never reach it, and GitHub repos keep the GitHub sweeps.
func forgejoSweeperFor(cfg *config.Config, boundary effects.Boundary, logger *slog.Logger) *forgesweep.Sweeper {
	if cfg == nil {
		return nil
	}
	repos := cfg.ForgejoSweepRepos()
	if len(repos) == 0 {
		return nil
	}
	blocking := []string{forgejoHoldLabel, cfg.Sentinel.LabelOrDefault(), cfg.Governor.Labels.DecisionNeededLabelOrDefault()}
	blocking = append(blocking, cfg.Governor.Labels.Exempt...)
	return &forgesweep.Sweeper{
		Repos:            repos,
		API:              func(repo string) (forge.SweepAPI, error) { return forgejoSweepAPIForRepo(cfg, repo) },
		AutoMergeEnabled: cfg.RepoAutoMergeEnabled,
		AutoMergeLabel:   cfg.Governor.Labels.AutoMerge,
		BlockingLabels:   blocking,
		MaxMerges:        cfg.AutoMerge.MaxMerges,
		Mutation:         boundary,
		Logger:           logger,
	}
}

// runForgejoSweepPass runs one automerge pass and, when issues.close_on_merge
// is on, one close-on-merge pass over the Forgejo repos.
func runForgejoSweepPass(ctx context.Context, cfg *config.Config, boundary effects.Boundary, logger *slog.Logger) (merges []forgesweep.MergeOutcome, closes []forgesweep.CloseOutcome) {
	s := forgejoSweeperFor(cfg, boundary, logger)
	if s == nil {
		return nil, nil
	}
	if config.ForgeSweepsFor(config.ForgeForgejo).AutoMerge {
		merges = s.AutoMerge(ctx)
	}
	if config.ForgeSweepsFor(config.ForgeForgejo).CloseOnMerge && cfg.Issues.CloseOnMergeEnabled() {
		closes = s.CloseOnMerge(ctx)
	}
	return merges, closes
}

// startForgejoSweeps runs the Forgejo sweeps every interval until ctx ends.
// cfgFn is read on every pass so a reload takes effect without a restart. A
// hive with no Forgejo repo does no work on a pass. The returned channel
// closes once the loop has exited.
func startForgejoSweeps(ctx context.Context, cfgFn func() *config.Config, boundary effects.Boundary, logger *slog.Logger, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		interval = forgejoSweepInterval
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, _, err := safeForgejoSweepPass(ctx, cfgFn(), boundary, logger); err != nil && logger != nil {
					logger.Error("forgejo sweeps: pass panicked", "error", err)
				}
			}
		}
	}()
	return done
}

// safeForgejoSweepPass is runForgejoSweepPass with a panic turned into an
// error, so one bad pass never ends the loop.
func safeForgejoSweepPass(ctx context.Context, cfg *config.Config, boundary effects.Boundary, logger *slog.Logger) (merges []forgesweep.MergeOutcome, closes []forgesweep.CloseOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New(fmt.Sprint(r))
		}
	}()
	merges, closes = runForgejoSweepPass(ctx, cfg, boundary, logger)
	return merges, closes, nil
}

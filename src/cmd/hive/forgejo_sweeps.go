package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/forgesweep"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// forgejoSweepInterval is how often the Forgejo automerge and close-on-merge
// sweeps run.
const forgejoSweepInterval = 2 * time.Minute

// forgejoHoldLabel is the forge-neutral hold gate (forge.SetHold).
const forgejoHoldLabel = "hold"

// newForgejoSweepAPI builds the Forgejo client a sweep uses. It is a variable
// so tests can substitute a fake.
var newForgejoSweepAPI = forge.NewForgejoSweepAPI

// forgejoSweepDeps are the process-wide pieces every pass shares.
type forgejoSweepDeps struct {
	// Mutation is the boundary merges and comments go through.
	Mutation effects.Boundary
	// Gate is the unsolicited write-back gate; nil allows.
	Gate   *writeback.Gate
	Logger *slog.Logger
}

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
	token, source, err := forgejoRESTToken(cfg)
	if err != nil {
		return nil, fmt.Errorf("forgejo token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("%s is empty", source)
	}
	return newForgejoSweepAPI(token, forge.Options{
		BaseURL: cfg.ForgeURLForRepo(repo),
		Org:     cfg.Project.Org,
		Login:   cfg.Forgejo.Login,
	})
}

// forgejoSweeperFor builds one pass's sweeper from cfg. Only repos whose kind
// has the sweeps (config.ForgeSweepsFor) are included; GitLab and Gitea
// repos never reach it, and GitHub repos keep the GitHub sweeps.
func forgejoSweeperFor(cfg *config.Config, deps forgejoSweepDeps) *forgesweep.Sweeper {
	if cfg == nil {
		return nil
	}
	repos := cfg.ForgejoSweepRepos()
	if len(repos) == 0 {
		return nil
	}
	blocking := []string{forgejoHoldLabel, cfg.Sentinel.LabelOrDefault(), cfg.Governor.Labels.DecisionNeededLabelOrDefault()}
	// The hive gate labels block as on GitHub (github.IsHiveGateLabel): the
	// PR relay holds every PR it opens with ready-for-review, and a person
	// releases it by removing the label (#19912). The defaults stay in the
	// list after a rename, so a label applied under the old name still holds.
	// The configured names are read from cfg too: on a forge-only hive no
	// GitHub client installs them into pkg/github.
	blocking = append(blocking, github.HiveGateLabels()...)
	blocking = append(blocking, cfg.Governor.Labels.ReadyForReviewLabelOrDefault())
	blocking = append(blocking, github.DefaultReadyForReviewLabel, github.DefaultDecisionNeededLabel)
	blocking = append(blocking, cfg.Governor.Labels.Exempt...)
	s := &forgesweep.Sweeper{
		Repos:            repos,
		API:              func(repo string) (forge.SweepAPI, error) { return forgejoSweepAPIForRepo(cfg, repo) },
		AutoMergeEnabled: cfg.RepoAutoMergeEnabled,
		AutoMergeLabel:   cfg.Governor.Labels.AutoMerge,
		BlockingLabels:   blocking,
		MaxMerges:        cfg.AutoMerge.MaxMerges,
		Mutation:         deps.Mutation,
		Logger:           deps.Logger,
	}
	if gate := deps.Gate; gate != nil {
		s.Gate = func(repo string, item writeback.Item, kind writeback.Kind, preview string) writeback.Decision {
			full, _ := config.NormalizeRepoForOrg(cfg.Project.Org, repo)
			return gate.AllowedPreview(full, item, kind, preview)
		}
	}
	return s
}

// runForgejoSweepPass runs one automerge pass and, when issues.close_on_merge
// is on, one close-on-merge pass over the Forgejo repos.
func runForgejoSweepPass(ctx context.Context, cfg *config.Config, deps forgejoSweepDeps) (merges []forgesweep.MergeOutcome, closes []forgesweep.CloseOutcome) {
	s := forgejoSweeperFor(cfg, deps)
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
func startForgejoSweeps(ctx context.Context, cfgFn func() *config.Config, deps forgejoSweepDeps, interval time.Duration) <-chan struct{} {
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
				if _, _, err := safeForgejoSweepPass(ctx, cfgFn(), deps); err != nil && deps.Logger != nil {
					deps.Logger.Error("forgejo sweeps: pass panicked", "error", err)
				}
			}
		}
	}()
	return done
}

// safeForgejoSweepPass is runForgejoSweepPass with a panic turned into an
// error, so one bad pass never ends the loop.
func safeForgejoSweepPass(ctx context.Context, cfg *config.Config, deps forgejoSweepDeps) (merges []forgesweep.MergeOutcome, closes []forgesweep.CloseOutcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New(fmt.Sprint(r))
		}
	}()
	merges, closes = runForgejoSweepPass(ctx, cfg, deps)
	return merges, closes, nil
}

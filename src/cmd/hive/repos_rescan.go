package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// lastActionablePath is where each enumeration's result is cached on the /data
// PVC so a restart can repaint the dashboard before the first eval cycle
// completes. Both writers (the governor eval cycle and the operator-initiated
// rescan below) and the boot-time reader key off this one constant so they
// cannot drift onto different files.
const lastActionablePath = "/data/last-actionable.json"

// errNoForgeCredentials is returned when the hive is running without usable
// GitHub credentials. There is nothing to enumerate, and the dashboard already
// shows a banner naming the cause, so the rescan says so plainly rather than
// reporting a scan that found zero of everything.
var errNoForgeCredentials = errors.New("hive is running without GitHub credentials")

// rescanRepos re-enumerates every watched repository's open issues and pull
// requests and republishes the dashboard snapshot, so the REPOSITORIES cards
// show the current tickets on demand instead of at the governor's eval
// cadence (minutes, and longer the quieter the hive is).
//
// It is deliberately the READ-ONLY half of runEvalCycle, in the same order:
// enumerate, overlay a non-GitHub work source if one is configured, enrich PR
// CI status, apply the duplicate-PR claim guard. Everything the eval cycle
// does that ACTS — governor mode evaluation, agent kicks, advisory issue
// posting, the escalation sweep, the auto-merge sweep — is left out, which is
// what makes the Rescan button safe to press at any moment.
//
// recordRedStaleness is also deliberately skipped: it advances the escalation
// clock, and an operator refreshing a view must not age PRs toward escalation.
// The claim guard's red-stale release predicate therefore reads a clock that
// has not seen this pass, which can only make it MORE conservative (a claiming
// PR keeps suppressing its issue) — never less.
func rescanRepos(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	lastActionable *atomic.Pointer[github.ActionableResult],
	refresh func(),
	logger *slog.Logger,
) (*github.ActionableResult, error) {
	if ghClient == nil {
		return nil, errNoForgeCredentials
	}

	actionable, err := ghClient.EnumerateActionable(ctx)
	if err != nil {
		// Same tolerance as the eval cycle: on a hive whose issues come from a
		// non-GitHub work source, a GitHub failure must not sink the whole
		// scan — the work source can still populate the issue side.
		var ok bool
		actionable, ok = actionableAfterGitHubEnumerate(cfg, actionable, err, logger)
		if !ok {
			return nil, err
		}
	}

	if wsType := cfg.Governor.WorkSource.Type; wsType != "" && wsType != "github" {
		ghToken := cfg.GitHub.Token
		if ghToken == "" {
			ghToken = os.Getenv("HIVE_GITHUB_TOKEN")
		}
		ws, wsErr := worksource.FromConfig(cfg.Governor.WorkSource, ghClient, ghToken, cfg.Project.Org, logger)
		actionable.Issues = workSourceIssuesForCycle(ctx, ws, wsErr, cfg.Governor.Labels.Exempt, cfg.Project.IssueFilter, logger)
	}

	ghClient.EnrichCIStatus(ctx, actionable.PRs.Items)
	applyDuplicatePRGuard(ctx, cfg, ghClient, actionable, logger)

	lastActionable.Store(actionable)
	if data, marshalErr := json.Marshal(actionable); marshalErr == nil {
		atomicWrite(lastActionablePath, data)
	}
	if refresh != nil {
		refresh()
	}

	logger.Info("manual repository rescan complete",
		"issues", actionable.Issues.Count,
		"prs", actionable.PRs.Count,
		"hold", actionable.Hold.Total,
	)
	return actionable, nil
}

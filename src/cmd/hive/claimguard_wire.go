package main

import (
	"log/slog"
	"sync/atomic"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/github"
)

// contributorClaimGuard is the process-wide guard the governor's live-claim
// filter runs through (hivecommons/hive#13579). Nil until the dashboard is
// wired, which is the pre-#13579 behaviour: every live claim withholds.
var contributorClaimGuard atomic.Pointer[github.ContributorClaimGuard]

// installContributorClaimGuard builds the guard from governor.claims and the
// contribute hub's flap cool-off, and publishes it for the eval cycle.
func installContributorClaimGuard(cfg *config.Config, dashSrv *dashboard.Server, logger *slog.Logger) *github.ContributorClaimGuard {
	if cfg == nil || !cfg.Governor.Claims.IsEnabled() {
		return nil
	}
	var coolOff func(repo string, number int) (string, bool)
	if dashSrv != nil {
		coolOff = dashSrv.ContributorCoolOffReason
	}
	g := github.NewContributorClaimGuard(cfg.Governor.Claims.EffectiveContributorStarvation(), coolOff)
	contributorClaimGuard.Store(g)
	if logger != nil {
		threshold, window, cooloff := cfg.Governor.Claims.EffectiveContributorFlap()
		logger.Info("issue-claims: contributor claim guard installed (#13579)",
			"starvation", g.Starvation.String(), "flap_threshold", threshold,
			"flap_window", window.String(), "flap_cooloff", cooloff.String())
	}
	return g
}

// contributorClaimOverrideLookup wraps a scheduler in-flight lookup so an
// issue the live-claim filter let through despite a contributor claim is not
// re-withheld by the ledger lookup on the same cycle.
func contributorClaimOverrideLookup(guard func() *github.ContributorClaimGuard, inner func(github.Issue) (string, bool)) func(github.Issue) (string, bool) {
	if inner == nil {
		return nil
	}
	return func(issue github.Issue) (string, bool) {
		if guard != nil {
			if g := guard(); g != nil {
				if _, ok := g.Overridden(issue.Repo, issue.Number); ok {
					return "", false
				}
			}
		}
		return inner(issue)
	}
}

package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// prClaimsHookFor builds the github.PRClaimsHook the governor installs
// (hivecommons/hive#14814): a `pull_request` webhook's claims go into the
// duplicate-PR claim ledger at once, the ledger is persisted, and the cached
// actionable snapshot is re-filtered through the same FilterClaimedIssues
// the reconcile pass applies — so an issue a just-opened PR fixes leaves
// the offerable set the moment the delivery lands instead of on the next
// enumeration, for the contribute lane (which reads the ledger at offer
// time) and the hub's own agents (which plan from the snapshot) alike. The
// ledger is resolved per call because it loads lazily; a nil patcher skips
// the snapshot step.
func prClaimsHookFor(ledger func() *github.ClaimLedger, patch *snapshotPatcher, logger *slog.Logger) github.PRClaimsHook {
	return func(d github.PRClaimDelivery) {
		if ledger == nil {
			return
		}
		l := ledger()
		if l == nil {
			return
		}
		recorded, released := l.ApplyPRDelivery(d)
		if recorded == 0 && released == 0 {
			return
		}
		if err := l.Save(); err != nil && logger != nil {
			logger.Warn("webhook PR claims: failed to persist claim ledger",
				"repo", d.Repo, "pr", d.Number, "error", err)
		}
		suppressed := patch.suppressClaimed(l, logger)
		if logger != nil {
			logger.Info("webhook PR claims applied to claim ledger",
				"repo", d.Repo, "pr", d.Number, "action", d.Action, "pr_state", d.State,
				"issues", d.ClaimedIssues(), "recorded", recorded, "released", released,
				"suppressed", suppressed, "source", "webhook")
		}
	}
}

// suppressClaimed re-filters the stored actionable snapshot against the
// claim ledger, copy-on-write, and reports how many issues it dropped. The
// red+stale release valve is not consulted: a claim the webhook just
// recorded is seconds old, and the reconcile pass re-applies the valve with
// live CI state. No stored snapshot is a no-op.
func (p *snapshotPatcher) suppressClaimed(ledger *github.ClaimLedger, logger *slog.Logger) int {
	if p == nil || p.target == nil || ledger == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.target.Load()
	if cur == nil {
		return 0
	}
	next := cur.Clone()
	suppressed := github.FilterClaimedIssues(next, ledger, nil, logger)
	if suppressed == 0 {
		return 0
	}
	p.target.Store(next)
	if p.logger != nil {
		p.logger.Info("webhook PR claims filtered the cached actionable snapshot",
			"suppressed", suppressed, "issues", next.Issues.Count,
			"snapshot_age", time.Since(next.GeneratedAt).Round(time.Second).String())
	}
	if p.onPatch != nil {
		p.onPatch(suppressed)
	}
	return suppressed
}

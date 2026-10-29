package main

import (
	"context"
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
// the snapshot step. onMerged, when non-nil, runs after a MERGED delivery
// whose claims the ledger recorded, so the PR's issues close on the event
// (closeOnMergeForDelivery) rather than on the next reconcile pass.
// onCovered, when non-nil, runs for every OPEN delivery that covers an
// issue (PRClaimDelivery.CoveringClaims), before the ledger's rank rule
// decides whether the claim is recorded: a contributor task in flight on
// that issue is preempted whether or not this PR became the ledger's
// holder (preemptCoveredIssueTasks).
func prClaimsHookFor(ledger func() *github.ClaimLedger, patch *snapshotPatcher, logger *slog.Logger, onMerged, onCovered func(github.PRClaimDelivery)) github.PRClaimsHook {
	return func(d github.PRClaimDelivery) {
		if ledger == nil {
			return
		}
		l := ledger()
		if l == nil {
			return
		}
		if onCovered != nil && len(d.CoveringClaims()) > 0 {
			onCovered(d)
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
		if onMerged != nil && d.State == github.PRStateMerged && recorded > 0 {
			onMerged(d)
		}
	}
}

// closeOnMergeForDelivery runs the close-on-merge ledger sweep for the one
// PR a MERGED `pull_request` delivery names, detached from the webhook
// handler and bounded by closeOnMergeDeliveryTimeout. Before this, an issue
// whose PR merged right after the reconcile pass's sweep waited for the next
// pass — hours, when webhook-driven cached cycles skip the optional sweeps —
// and in the meantime kept its stale stage record (hivecommons/hive#14646
// sat at 010 under a PR merged at 12:54Z). The sweep re-reads the PR and the
// issue live and applies the same identity/trusted-author policy as the
// scheduled one; it does not need the sweep scheduler's cadence.
func (b *boot) closeOnMergeForDelivery(d github.PRClaimDelivery) {
	cfg, client := b.cfg, b.ghClient
	if cfg == nil || client == nil || !cfg.Issues.CloseOnMergeEnabled() || d.Number <= 0 {
		return
	}
	ledger := getClaimLedger(b.logger)
	parent := b.ctx
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(parent, closeOnMergeDeliveryTimeout)
		defer cancel()
		opts := closeOnMergeOptionsFor(cfg, b.logger, time.Now)
		opts.OnlyPRRepo, opts.OnlyPRNumber = d.Repo, d.Number
		sweep := client.CloseOnMergeLedgerSweep(ctx, ledger, opts)
		logCloseOnMergeResults(b.logger, "close-on-merge webhook sweep", sweep.Results)
		// The merged PR closed its issues; a hive-authored PR still open
		// for one of them is superseded now, not after the next sweep
		// (hivecommons/hive#14965 → #14957).
		if cfg.Governor.AutoCloseSupersededEnabled() {
			closed, err := client.CloseSupersededHivePRsForMergedPR(ctx, d.Repo, d.Number, d.ClaimedIssues())
			if err != nil && b.logger != nil {
				b.logger.Warn("close-on-merge: superseded hive PR check failed", "repo", d.Repo, "pr", d.Number, "error", err)
			}
			if len(closed) > 0 && b.logger != nil {
				b.logger.Info("close-on-merge: superseded hive PRs closed", "repo", d.Repo, "merged", d.Number, "closed", closed)
			}
		}
	}()
}

// closeOnMergeDeliveryTimeout bounds one webhook-triggered close-on-merge
// sweep: a handful of reads and at most a few closes.
const closeOnMergeDeliveryTimeout = 2 * time.Minute

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

// prClaimCoverLookup is the github.Client's read of the duplicate-PR claim
// ledger for the stage-fact sink: the ledger's live claim on (repo, issue),
// unless the claiming PR is red and stale on this spoke's last snapshot, in
// which case FilterClaimedIssues would have released the issue and the
// fact sink must not re-cover it. The ledger loads lazily, so it is
// resolved per call.
func (b *boot) prClaimCoverLookup() func(repo string, number int) (github.IssueClaim, bool) {
	return func(repo string, number int) (github.IssueClaim, bool) {
		ledger := getClaimLedger(b.logger)
		claim, ok := ledger.Lookup(repo, number)
		if !ok {
			return github.IssueClaim{}, false
		}
		if b.cfg != nil && !claim.MergedPR {
			if redStale := claimingPRRedStale(b.cfg, b.lastActionable.Load()); redStale != nil && redStale(claim.PRRepo, claim.PRNumber) {
				return github.IssueClaim{}, false
			}
		}
		return claim, true
	}
}

// preemptCoveredIssueTasks is the onCovered hook: an OPEN `pull_request`
// delivery that declares `Fixes #N` releases every relay contributor still
// dispatched on #N (other than the PR's own author) so the hive does not
// finish a second PR for an issue a person — or another agent — already
// covered (hivecommons/hive#15469: the owner's #15470 landed 45 s after
// the hive dispatched its fixer, which then opened #15473). The dashboard
// server owns the hub; before it boots there is nothing to preempt.
func (b *boot) preemptCoveredIssueTasks(d github.PRClaimDelivery) {
	if b == nil || b.dashSrv == nil {
		return
	}
	hub := b.dashSrv.ContributeHub()
	if hub == nil {
		return
	}
	for _, c := range d.CoveringClaims() {
		released := hub.PreemptIssueCoveredByPR(c.Repo, c.Issue, d.Number, c.PRAuthor)
		if released > 0 && b.logger != nil {
			b.logger.Info("webhook PR claims: contributor task preempted, an open PR now covers its issue",
				"repo", d.Repo, "issue", c.Issue, "pr", d.Number, "pr_author", c.PRAuthor, "released", released, "source", "webhook")
		}
	}
}

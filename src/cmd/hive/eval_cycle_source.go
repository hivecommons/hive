package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// evalCycleSource says where an eval cycle gets its actionable snapshot.
//
// The model (docs/operator-reference.md, "Polling cadence, webhook patch,
// reconcile"): the cadence tick re-lists every tracked repository and is
// authoritative; webhook-driven wakes plan from the cached snapshot, which
// the delivery's targeted re-read has already patched for the item it
// named, so inbound webhooks replace outbound API calls instead of
// multiplying them.
type evalCycleSource int

const (
	// evalSourceEnumerate re-lists every repository (the reconcile pass).
	evalSourceEnumerate evalCycleSource = iota
	// evalSourceCached plans from the last stored snapshot, patched by any
	// webhook re-reads since, without listing. Falls back to enumerate when
	// no snapshot exists yet.
	evalSourceCached
)

func (s evalCycleSource) String() string {
	if s == evalSourceCached {
		return "cached"
	}
	return "enumerate"
}

// cachedSnapshotFor returns the snapshot a cycle of the given source plans
// from (a copy, so webhook patches landing meanwhile are not lost), or nil
// when the cycle must enumerate: an enumerate source, or no snapshot yet.
func cachedSnapshotFor(source evalCycleSource, last *atomic.Pointer[github.ActionableResult]) *github.ActionableResult {
	if source != evalSourceCached || last == nil {
		return nil
	}
	return last.Load().Clone()
}

// keepLastGoodAfterFailedEnumerate is the reconcile pass's failure rule:
// the snapshot the governor already holds (including any webhook patches)
// stays published, marked stale for every repo, until a later pass
// succeeds. Returns the kept snapshot, nil when there was none.
func keepLastGoodAfterFailedEnumerate(last *atomic.Pointer[github.ActionableResult], logger *slog.Logger) *github.ActionableResult {
	if last == nil {
		return nil
	}
	prev := last.Load()
	if prev == nil {
		return nil
	}
	if prev.Stale() && len(prev.StaleRepos) == len(prev.TotalByRepo) {
		return prev
	}
	next := prev.Clone()
	next.MarkAllStale()
	last.Store(next)
	if logger != nil {
		logger.Warn("GitHub enumeration failed; keeping last-good snapshot (marked stale) until the next reconcile pass",
			"snapshot_age", time.Since(next.GeneratedAt).Round(time.Second).String(),
			"issues", next.Issues.Count, "prs", next.PRs.Count, "repos", len(next.StaleRepos))
	}
	return next
}

// cachedSnapshotSkippedStep is reported alongside the API-budget step names
// when a cached cycle sheds the enumeration-class work.
const cachedSnapshotSkippedStep = "cached-snapshot"

// snapshotPatcher applies webhook re-reads to the governor's stored
// actionable snapshot copy-on-write, serialising concurrent flushes.
type snapshotPatcher struct {
	mu      sync.Mutex
	target  *atomic.Pointer[github.ActionableResult]
	logger  *slog.Logger
	onPatch func(applied int)
}

// apply upserts each snapshot into the stored result and returns how many
// changed it. No stored result (before the first enumeration) is a no-op:
// the first cycle lists everything anyway.
func (p *snapshotPatcher) apply(snaps []github.WebhookItemSnapshot) int {
	if p == nil || p.target == nil || len(snaps) == 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var next *github.ActionableResult
	applied := 0
	// Compare-and-swap, not Store: a reconcile pass publishing between the
	// load and the store (publishEnumeratedSnapshot) must not be overwritten
	// by a patch of the snapshot it replaced.
	for {
		cur := p.target.Load()
		if cur == nil {
			return 0
		}
		next = cur.Clone()
		applied = 0
		for _, snap := range snaps {
			if next.ApplyWebhookItem(snap) {
				applied++
			}
		}
		if applied == 0 {
			return 0
		}
		if p.target.CompareAndSwap(cur, next) {
			break
		}
	}
	if p.logger != nil {
		p.logger.Info("webhook re-read patched the cached actionable snapshot",
			"items", applied, "issues", next.Issues.Count, "prs", next.PRs.Count, "hold", next.Hold.Total,
			"snapshot_age", time.Since(next.GeneratedAt).Round(time.Second).String())
	}
	if p.onPatch != nil {
		p.onPatch(applied)
	}
	return applied
}

// applyWebhookItemsFunc is the dashboard.Dependencies.ApplyWebhookItems hook.
func (b *boot) applyWebhookItemsFunc() func([]github.WebhookItemSnapshot) {
	if b.snapshotPatch == nil {
		b.snapshotPatch = &snapshotPatcher{target: &b.lastActionable, logger: b.logger}
		if b.evalLoop != nil {
			b.snapshotPatch.onPatch = b.evalLoop.notePatch
		}
		// A `pull_request` delivery's issue claims land in the ledger and
		// drop out of this snapshot on the event (hivecommons/hive#14814).
		github.SetPRClaimsHook(prClaimsHookFor(func() *github.ClaimLedger { return getClaimLedger(b.logger) }, b.snapshotPatch, b.logger, b.closeOnMergeForDelivery, b.preemptCoveredIssueTasks))
	}
	return func(snaps []github.WebhookItemSnapshot) { b.snapshotPatch.apply(snaps) }
}

// publishEnumeratedSnapshot makes a reconcile pass's result the governor's
// stored snapshot. Webhook re-reads that patched the previous snapshot after
// this enumeration started are replayed onto the result first
// (ActionableResult.ReplayWebhookPatches): the enumeration's copy of such an
// item predates the read, so storing it unpatched would roll the item back
// — the hive's own `blocked` removal, re-read within seconds, showed as
// still blocked until the next pass (hivecommons/hive#14952). Published by
// compare-and-swap so a patch landing meanwhile is retried on top.
func publishEnumeratedSnapshot(last *atomic.Pointer[github.ActionableResult], next *github.ActionableResult, logger *slog.Logger) {
	if last == nil || next == nil {
		return
	}
	for {
		prev := last.Load()
		replayed := next.ReplayWebhookPatches(prev)
		if !last.CompareAndSwap(prev, next) {
			continue
		}
		if replayed > 0 && logger != nil {
			logger.Info("reconcile pass replayed webhook re-reads newer than its listing onto the published snapshot",
				"items", replayed, "issues", next.Issues.Count, "prs", next.PRs.Count,
				"snapshot_age", time.Since(next.GeneratedAt).Round(time.Second).String())
		}
		return
	}
}

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
	cur := p.target.Load()
	if cur == nil {
		return 0
	}
	next := cur.Clone()
	applied := 0
	for _, snap := range snaps {
		if next.ApplyWebhookItem(snap) {
			applied++
		}
	}
	if applied == 0 {
		return 0
	}
	p.target.Store(next)
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
	}
	return func(snaps []github.WebhookItemSnapshot) { b.snapshotPatch.apply(snaps) }
}

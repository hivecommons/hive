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
// the first cycle lists everything anyway. Each applied read is remembered
// on the published snapshot (WebhookReads) so a reconcile pass already in
// flight can replay it (publishReconciledSnapshot). The swap is a
// compare-and-swap: a reconcile result published meanwhile is patched
// rather than overwritten.
func (p *snapshotPatcher) apply(snaps []github.WebhookItemSnapshot) int {
	if p == nil || p.target == nil || len(snaps) == 0 {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		cur := p.target.Load()
		if cur == nil {
			return 0
		}
		next := cur.Clone()
		now := time.Now()
		applied := 0
		for _, snap := range snaps {
			if next.ApplyWebhookItem(snap) {
				applied++
				if snap.FetchedAt.IsZero() {
					snap.FetchedAt = now
				}
				next.WebhookReads = append(next.WebhookReads, snap)
			}
		}
		if applied == 0 {
			return 0
		}
		next.WebhookReads = pruneWebhookReads(next.WebhookReads, now.Add(-webhookReadReplayWindow))
		if !p.target.CompareAndSwap(cur, next) {
			continue
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
}

// webhookReadReplayWindow bounds how long an applied webhook re-read is
// remembered for replay: far longer than any reconcile pass runs.
const webhookReadReplayWindow = 2 * time.Hour

// pruneWebhookReads drops reads fetched before cutoff, into a fresh slice.
func pruneWebhookReads(reads []github.WebhookItemSnapshot, cutoff time.Time) []github.WebhookItemSnapshot {
	var out []github.WebhookItemSnapshot
	for _, r := range reads {
		if !r.FetchedAt.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out
}

// publishReconciledSnapshot stores a reconcile pass's result. Webhook
// re-reads fetched after the pass started (passStart) describe a newer
// GitHub state than the pass may have listed — the hive's own `blocked`
// removal at 12:41 under a pass that listed at 12:38 — so they are replayed
// onto the result first (ApplyWebhookItem drops any the pass already saw a
// newer copy of) and kept on it for the next pass. Compare-and-swap, so a
// patch landing meanwhile is replayed too rather than lost
// (hivecommons/hive#14952).
func publishReconciledSnapshot(last *atomic.Pointer[github.ActionableResult], actionable *github.ActionableResult, passStart time.Time) {
	if last == nil || actionable == nil {
		return
	}
	for {
		prev := last.Load()
		var reads []github.WebhookItemSnapshot
		if prev != nil {
			for _, r := range prev.WebhookReads {
				if r.FetchedAt.After(passStart) {
					actionable.ApplyWebhookItem(r)
					reads = append(reads, r)
				}
			}
		}
		actionable.WebhookReads = reads
		if last.CompareAndSwap(prev, actionable) {
			return
		}
	}
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
		github.SetPRClaimsHook(prClaimsHookFor(func() *github.ClaimLedger { return getClaimLedger(b.logger) }, b.snapshotPatch, b.logger))
	}
	return func(snaps []github.WebhookItemSnapshot) { b.snapshotPatch.apply(snaps) }
}

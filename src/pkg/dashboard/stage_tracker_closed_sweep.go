package dashboard

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The closed-item sweep is the stage tracker's reconciliation backstop
// (hivecommons/hive#14159, #14064). The webhook path drops a row the moment
// a delivery says its item closed (evictClosedWebhookItem) and again when
// the confirming read says so; a delivery that never arrived, or a read the
// budget guards refused, leaves the row to the next full enumeration, which
// a rate-limited or failing cycle can postpone indefinitely. So on each
// status publish, at most once per interval, the sweep asks each repository
// for the items closed since its last look — one list call per repo, newest
// first — and evicts any the status snapshot still lists as open, through
// the same patch path a webhook read takes (so the governor's snapshot and
// the replay memory see it too). It is deliberately not tied to GET
// /api/stage-tracker, which never reaches GitHub (see
// TestStageTrackerCacheServesWithoutGitHub).

// closedSweepFirstLookback bounds the first sweep's `since` after start:
// anything closed earlier than this and still listed is the enumeration's
// to fix.
const closedSweepFirstLookback = 24 * time.Hour

// closedSweepMaxPages caps one sweep's list calls per repo.
const closedSweepMaxPages = 3

// mergedLedgerBackfillLookback is the first sweep's `since` when the
// merged ledger was seeded on this boot (no file under /data yet): the
// audit log only remembers the hive's own merges, so the ledger backfills
// from GitHub's closed list — every PR merged in the last 30 days, by
// anyone — within closedSweepMaxPages pages per repo
// (hivecommons/hive#15147).
const mergedLedgerBackfillLookback = 30 * 24 * time.Hour

// closedSweepOverlap is re-read on each sweep so an item whose updated_at
// fell just before the previous `since` is not missed.
const closedSweepOverlap = 2 * time.Minute

// closedSweepState is the sweep's bookkeeping; guarded by its mutex.
type closedSweepState struct {
	mu      sync.Mutex
	lastAt  time.Time
	since   time.Time
	running bool
	// done is closed when the running sweep finishes; tests wait on it.
	done chan struct{}
	// list is the GitHub read; tests inject one.
	list func(ctx context.Context, repo string, since time.Time) ([]github.WebhookItemSnapshot, error)
}

// stageTrackerClosedSweepInterval is dashboard.stage_tracker.closed_sweep_interval_s.
func (s *Server) stageTrackerClosedSweepInterval() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerClosedSweepInterval()
	}
	return (*config.Config)(nil).StageTrackerClosedSweepInterval()
}

// maybeSweepClosedRows starts a sweep in the background when one is due
// and none is running; it reports whether it started one. UpdateStatusIfFresh
// calls it on every published snapshot.
func (s *Server) maybeSweepClosedRows(now time.Time) bool {
	if s == nil || s.deps == nil || (s.deps.GHClient == nil && s.closedSweep.list == nil) {
		return false
	}
	interval := s.stageTrackerClosedSweepInterval()
	if interval <= 0 {
		return false
	}
	st := &s.closedSweep
	st.mu.Lock()
	if st.running || (!st.lastAt.IsZero() && now.Sub(st.lastAt) < interval) {
		st.mu.Unlock()
		return false
	}
	st.running = true
	done := make(chan struct{})
	st.done = done
	st.mu.Unlock()
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	go func() {
		defer func() {
			st.mu.Lock()
			st.running = false
			st.mu.Unlock()
			close(done)
		}()
		if _, err := s.sweepClosedRows(ctx, now); err != nil {
			s.webhookLogger().Debug("stage tracker closed sweep skipped", "error", err)
		}
	}()
	return true
}

// sweepClosedRows runs one sweep synchronously: for every repository in the
// status snapshot it lists the items closed since the previous sweep (the
// first looks back closedSweepFirstLookback) and evicts those still listed.
// It returns how many list entries it dropped and the first list error; a
// repo whose read failed is retried from the same `since` next time.
func (s *Server) sweepClosedRows(ctx context.Context, now time.Time) (int, error) {
	st := &s.closedSweep
	st.mu.Lock()
	since := st.since
	list := st.list
	st.mu.Unlock()
	backfill := since.IsZero() && s.mergedLedger() != nil && s.mergedLedger().NeedsBackfill()
	if backfill {
		since = now.Add(-mergedLedgerBackfillLookback)
	} else if since.IsZero() {
		since = now.Add(-closedSweepFirstLookback)
	} else {
		since = since.Add(-closedSweepOverlap)
	}
	if list == nil {
		client := s.deps.GHClient
		list = func(ctx context.Context, repo string, since time.Time) ([]github.WebhookItemSnapshot, error) {
			return client.ListClosedSince(ctx, repo, since, closedSweepMaxPages)
		}
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return 0, nil
	}
	var firstErr error
	allRead := true
	var evict, merged []github.WebhookItemSnapshot
	for _, repo := range status.Repos {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		name := overviewRepoName(repo)
		closed, err := list(ctx, name, since)
		if err != nil {
			allRead = false
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, snap := range closed {
			if snap.Open() {
				continue
			}
			if !strings.EqualFold(snap.Repo, name) && !strings.EqualFold(snap.Repo, repo.Name) {
				snap.Repo = name
			}
			if snap.IsPR && snap.Merged {
				merged = append(merged, snap)
			}
			if frontendRepoLists(repo, snap.Number, snap.IsPR) {
				evict = append(evict, snap)
			}
		}
	}
	// Every merged PR feeds the ledger, listed or not: a PR that left the
	// tracker before the sweep saw it close still counts as merged.
	s.recordMergedSnapshots(merged)
	if backfill && allRead {
		s.mergedLedger().Backfilled()
		s.webhookLogger().Info("merged ledger backfilled from GitHub", "merged", len(merged), "since", since.UTC().Format(time.RFC3339))
	}
	evicted := 0
	if len(evict) > 0 {
		evicted = s.applyWebhookItemSnapshots(evict)
		if evicted > 0 {
			s.webhookLogger().Info("stage tracker closed sweep evicted rows GitHub reports closed", "rows", evicted, "since", since.UTC().Format(time.RFC3339))
		}
	}
	st.mu.Lock()
	st.lastAt = now
	if allRead {
		st.since = now
	}
	st.mu.Unlock()
	return evicted, firstErr
}

// frontendRepoLists reports whether repo's item lists hold the item.
func frontendRepoLists(repo FrontendRepo, number int, isPR bool) bool {
	kind := "issue"
	if isPR {
		kind = "pr"
	}
	for _, it := range repoOpenItems(repo) {
		if it.Kind == kind && it.Number == number {
			return true
		}
	}
	// GitHub's issues feed lists pull requests too: a closed PR's copy in
	// the issue lists leaves with it (patchFrontendRepoItem), so a PR that
	// only the issue lists still hold counts as listed.
	if isPR {
		for _, it := range repoOpenItems(repo) {
			if it.Kind == "issue" && it.Number == number {
				return true
			}
		}
	}
	return false
}

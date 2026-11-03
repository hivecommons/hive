package forgestate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Reconcile lists repo's open issues and PRs and makes them the cache's
// authoritative content. On any failure — budget refusal, 403, network — the
// previous snapshot is kept untouched and marked Stale; the cache never goes
// empty because a list call failed. Patches a webhook or write-through
// applied while the list was in flight are newer than the list and survive
// it. The number of items the list found wrong in the cache is recorded as
// the forgestate_drift metric.
func (s *Store) Reconcile(ctx context.Context, repo string) error {
	rs := s.lookup(repo)
	if rs == nil {
		return fmt.Errorf("%w: %s", ErrUnknownRepo, repo)
	}
	start := s.opts.Now()
	if err := s.opts.Gate.Admit(ctx, BucketCore); err != nil {
		s.markStale(rs, start, err)
		s.opts.Metrics.Reconcile(rs.name, resultDenied)
		return fmt.Errorf("forgestate: reconcile %s: %w", rs.name, err)
	}
	s.mu.RLock()
	etag := rs.etag
	s.mu.RUnlock()
	res, err := s.opts.Fetcher.List(ctx, rs.name, etag)
	now := s.opts.Now()
	if err == nil && res.NotModified && etag == "" {
		err = errors.New("not modified without a cached etag")
	}
	if err != nil {
		s.markStale(rs, now, err)
		s.opts.Metrics.Reconcile(rs.name, resultError)
		s.opts.Logger.Warn("forgestate: reconcile failed; keeping last good snapshot",
			slog.String("repo", rs.name), slog.Any("err", err))
		return fmt.Errorf("forgestate: reconcile %s: %w", rs.name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if res.NotModified {
		// GitHub confirmed the list is unchanged since etag; anything the
		// webhook and write-through writers applied since is newer still.
		rs.drift, rs.driftHigh = 0, false
		s.markGoodLocked(rs, now)
		s.opts.Metrics.Drift(rs.name, 0)
		s.opts.Metrics.Reconcile(rs.name, resultNotModified)
		return nil
	}
	issues := s.mergeListLocked(rs, KindIssue, res.Issues, start, now)
	prs := s.mergeListLocked(rs, KindPR, res.PRs, start, now)
	drift := 0
	if rs.ready {
		issueDrift, prDrift := countDrift(rs.issues, issues), countDrift(rs.prs, prs)
		drift = issueDrift + prDrift
		recordDrift(ctx, rs.name, KindIssue, issueDrift)
		recordDrift(ctx, rs.name, KindPR, prDrift)
	}
	rs.issues, rs.prs = issues, prs
	for k, t := range rs.tombs {
		if t.at.Before(start) {
			delete(rs.tombs, k)
		}
	}
	rs.etag = res.ETag
	total := len(issues) + len(prs)
	if total < 1 {
		total = 1
	}
	rs.drift = drift
	rs.driftHigh = float64(drift)/float64(total) > s.opts.DriftThreshold
	if rs.driftHigh {
		s.opts.Logger.Warn("forgestate: reconcile drift above threshold; shortening interval",
			slog.String("repo", rs.name), slog.Int("drift", drift), slog.Int("items", total),
			slog.Float64("threshold", s.opts.DriftThreshold))
	}
	s.markGoodLocked(rs, now)
	s.opts.Metrics.Drift(rs.name, drift)
	s.opts.Metrics.Reconcile(rs.name, resultOK)
	return nil
}

// mergeListLocked builds the next item map for kind from a listing, keeping
// the patches and removals that landed after start (they are newer than
// the listing).
func (s *Store) mergeListLocked(rs *repoState, kind Kind, listed []Item, start, now time.Time) map[int]Item {
	next := make(map[int]Item, len(listed))
	for _, it := range listed {
		it = it.clone()
		it.Kind = kind
		if it.Number <= 0 || !it.Open() {
			continue
		}
		it.Source, it.FetchedAt = SourceReconcile, now
		next[it.Number] = it
	}
	for n, cur := range rs.items(kind) {
		if cur.Source == SourceReconcile || cur.FetchedAt.Before(start) {
			continue
		}
		if l, ok := next[n]; !ok || !supersedes(l, cur) {
			next[n] = cur
		}
	}
	for k, t := range rs.tombs {
		if k.kind != kind || t.at.Before(start) {
			continue
		}
		if l, ok := next[k.number]; ok && !l.UpdatedAt.After(t.updatedAt) {
			delete(next, k.number)
		}
	}
	return next
}

// countDrift is the number of items whose cached state differs from the
// authoritative one: missing, extra, or changed.
func countDrift(prev, next map[int]Item) int {
	d := 0
	for n, p := range prev {
		if q, ok := next[n]; !ok || !sameContent(p, q) {
			d++
		}
	}
	for n := range next {
		if _, ok := prev[n]; !ok {
			d++
		}
	}
	return d
}

func (s *Store) markGoodLocked(rs *repoState, now time.Time) {
	rs.stale, rs.staleSince, rs.staleReason = false, time.Time{}, ""
	rs.lastGoodAt = now
	s.opts.Metrics.Stale(rs.name, false)
	s.markReadyLocked(rs)
}

func (s *Store) markStale(rs *repoState, now time.Time, err error) {
	s.mu.Lock()
	if !rs.stale {
		rs.staleSince = now
	}
	rs.stale = true
	rs.staleReason = err.Error()
	s.mu.Unlock()
	s.opts.Metrics.Stale(rs.name, true)
}

// interval is how long until repo's next reconcile: the short interval
// while it is stale or drifting, the configured one otherwise. Either is
// stretched by AdaptiveInterval as the core rate-limit budget drains, so a
// stale repo on a nearly spent quota is retried less often rather than
// hammered; a healthy budget leaves the interval alone.
func (s *Store) interval(rs *repoState) time.Duration {
	s.mu.RLock()
	base := s.opts.ReconcileInterval
	if rs.stale || rs.driftHigh {
		base = s.opts.MinReconcileInterval
	}
	s.mu.RUnlock()
	if budget := s.budget(); budget != nil {
		if remaining, limit, ok := budget(string(BucketCore)); ok {
			return AdaptiveInterval(base, remaining, limit)
		}
	}
	return base
}

// Run reconciles every repo immediately and then each on its own interval
// until ctx ends. Errors are recorded on the snapshot (Stale) and retried
// on the short interval; Run itself only returns ctx.Err().
func (s *Store) Run(ctx context.Context) error {
	due := make(map[string]time.Time, len(s.order))
	for {
		for _, key := range s.order {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if s.opts.Now().Before(due[key]) {
				continue
			}
			rs := s.repos[key]
			_ = s.Reconcile(ctx, rs.name)
			due[key] = s.opts.Now().Add(s.interval(rs))
		}
		wait := s.opts.ReconcileInterval
		now := s.opts.Now()
		for _, at := range due {
			if d := at.Sub(now); d < wait {
				wait = d
			}
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.opts.After(wait):
		}
	}
}

package dashboard

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// webhookTrackerRefresh is the activity-monitor half of a GitHub webhook
// delivery. The receiver (handleGitHubWebhook) already invalidates the PR
// caches and wakes the governor; until the wake cycle re-enumerates the
// repository the tracker would keep showing the row as it was. Instead,
// the item the delivery named is re-read on its own — one GET per item,
// debounced so a check fan-out coalesces — and its row in the status
// snapshot is patched in place (labels, title, draft, last activity, open
// or gone), then the /api/stage-tracker cache is dropped and SSE nudges
// the page. No full scan runs; new items still arrive with the next cycle.

// trackerRefreshKey identifies one PR or issue to re-read.
type trackerRefreshKey struct {
	repo   string
	number int
	pr     bool
}

// trackerRefreshTimer is the one-shot timer the refresher arms; tests inject
// a hand-fired implementation so no test sleeps.
type trackerRefreshTimer interface {
	Stop() bool
}

type webhookTrackerRefresher struct {
	mu sync.Mutex
	// pending maps each queued item to the X-GitHub-Delivery id of the
	// newest delivery that asked for it (audit only; never state).
	pending  map[trackerRefreshKey]string
	timer    trackerRefreshTimer
	debounce func() time.Duration
	// newTimer arms a one-shot timer that calls f after d; time.AfterFunc in
	// production.
	newTimer func(d time.Duration, f func()) trackerRefreshTimer
	fetch    func(ctx context.Context, repo string, number int, isPR bool) (github.WebhookItemSnapshot, error)
	apply    func(snaps []github.WebhookItemSnapshot)
	onFetch  func(n int)
	ctx      func() context.Context
	// flushes counts debounce windows that fired (tests).
	flushes int
}

func newWebhookTrackerRefresher() *webhookTrackerRefresher {
	return &webhookTrackerRefresher{
		pending: map[trackerRefreshKey]string{},
		newTimer: func(d time.Duration, f func()) trackerRefreshTimer {
			return time.AfterFunc(d, f)
		},
		onFetch: github.RecordWebhookTargetedRefreshes,
	}
}

// enqueue schedules a re-read of each key on behalf of deliveryID. Keys
// already waiting are coalesced (the newest delivery id wins); the first key
// of an idle refresher arms the debounce timer and later keys ride the same
// window.
func (r *webhookTrackerRefresher) enqueue(keys []trackerRefreshKey, deliveryID string) (armed bool) {
	if r == nil || len(keys) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range keys {
		if k.number <= 0 || k.repo == "" {
			continue
		}
		r.pending[k] = deliveryID
	}
	if len(r.pending) == 0 || r.timer != nil {
		return false
	}
	d := defaultWebhookRefreshDebounce
	if r.debounce != nil {
		d = r.debounce()
	}
	r.timer = r.newTimer(d, r.flush)
	return true
}

const defaultWebhookRefreshDebounce = 2 * time.Second

// flushNow runs a pending window immediately instead of waiting out the
// debounce, so a governor wake can plan from the patched snapshot. A
// refresher with nothing pending is a no-op.
func (r *webhookTrackerRefresher) flushNow() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	r.flush()
}

// flush takes every pending key, fetches each once and applies the results.
// Keys enqueued while the fetches run start a new window.
func (r *webhookTrackerRefresher) flush() {
	r.mu.Lock()
	keys := make([]trackerRefreshKey, 0, len(r.pending))
	deliveries := make(map[trackerRefreshKey]string, len(r.pending))
	for k, id := range r.pending {
		keys = append(keys, k)
		deliveries[k] = id
	}
	r.pending = map[trackerRefreshKey]string{}
	r.timer = nil
	r.flushes++
	fetch, apply, onFetch, ctxFn := r.fetch, r.apply, r.onFetch, r.ctx
	r.mu.Unlock()
	if len(keys) == 0 || fetch == nil {
		return
	}
	ctx := context.Background()
	if ctxFn != nil {
		if c := ctxFn(); c != nil {
			ctx = c
		}
	}
	snaps := make([]github.WebhookItemSnapshot, 0, len(keys))
	fetched := 0
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		fetched++
		// The delivery id rides the context so the fetch can record it on
		// the governor's state cache as a webhook patch rather than a
		// read-through (pkg/forgestate).
		snap, err := fetch(github.WithWebhookDelivery(ctx, deliveries[k]), k.repo, k.number, k.pr)
		if err != nil {
			continue
		}
		snap.DeliveryID = deliveries[k]
		snaps = append(snaps, snap)
	}
	if onFetch != nil {
		onFetch(fetched)
	}
	if apply != nil {
		apply(snaps)
	}
}

// pendingCount is for tests.
func (r *webhookTrackerRefresher) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// webhookTrackerKeys turns a verified delivery's result into the items the
// tracker should re-read: every invalidated PR plus every touched issue.
func webhookTrackerKeys(res github.WebhookEventResult) []trackerRefreshKey {
	if !res.KnownRepo {
		return nil
	}
	repo := res.ConfiguredRepo
	if repo == "" {
		repo = res.Repo
	}
	var keys []trackerRefreshKey
	for _, n := range res.Invalidated {
		keys = append(keys, trackerRefreshKey{repo: repo, number: n, pr: true})
	}
	seen := map[int]bool{}
	for _, n := range append(append([]int{}, res.TouchedIssues...), res.ReopenedIssue) {
		if n > 0 && !seen[n] {
			seen[n] = true
			keys = append(keys, trackerRefreshKey{repo: repo, number: n, pr: false})
		}
	}
	return keys
}

// webhookTrackerRefreshEnabled is dashboard.stage_tracker.webhook_refresh.
func (s *Server) webhookTrackerRefreshEnabled() bool {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return true
	}
	return s.deps.Config.StageTrackerWebhookRefresh()
}

// trackerRefresher returns the server's refresher, building it on first
// use wired to the GitHub client and the status snapshot. The debounce is
// dashboard.stage_tracker.webhook_refresh_debounce_ms, read per window so
// a config save applies without a restart.
func (s *Server) trackerRefresher() *webhookTrackerRefresher {
	s.webhookRefreshOnce.Do(func() {
		if s.webhookRefresh == nil {
			s.webhookRefresh = newWebhookTrackerRefresher()
		}
		r := s.webhookRefresh
		r.debounce = func() time.Duration {
			if s.deps != nil && s.deps.Config != nil {
				return s.deps.Config.StageTrackerWebhookRefreshDebounce()
			}
			return defaultWebhookRefreshDebounce
		}
		if r.fetch == nil {
			r.fetch = func(ctx context.Context, repo string, number int, isPR bool) (github.WebhookItemSnapshot, error) {
				if s.deps == nil || s.deps.GHClient == nil {
					return github.WebhookItemSnapshot{}, github.ErrNoGitHubClient
				}
				snap, err := s.deps.GHClient.FetchWebhookItemSnapshot(ctx, repo, number, isPR)
				if err != nil {
					s.webhookLogger().Debug("webhook tracker refresh skipped", "repo", repo, "number", number, "error", err)
				}
				return snap, err
			}
		}
		if r.apply == nil {
			r.apply = s.applyWebhookItemSnapshots
		}
		if r.ctx == nil {
			r.ctx = func() context.Context {
				if s.deps != nil && s.deps.Ctx != nil {
					return s.deps.Ctx
				}
				return context.Background()
			}
		}
	})
	return s.webhookRefresh
}

// scheduleWebhookTrackerRefresh is the receiver's hook: it drops the
// memoised tracker payload so the next GET sees any stage-store change the
// delivery reflects, and queues the named items for a targeted re-read
// tagged with the delivery id. The payload's own fields are never applied:
// the delivery is a hint to re-read, and the GET is the state. It reports
// how many items were queued.
func (s *Server) scheduleWebhookTrackerRefresh(res github.WebhookEventResult, deliveryID string) int {
	if !s.webhookTrackerRefreshEnabled() {
		return 0
	}
	keys := webhookTrackerKeys(res)
	if len(keys) == 0 {
		return 0
	}
	s.invalidateStageTrackerForWebhook()
	s.trackerRefresher().enqueue(keys, deliveryID)
	return len(keys)
}

// invalidateStageTrackerForWebhook drops the tracker cache on a webhook's
// account, counts it, and nudges SSE clients that have the tracker open.
func (s *Server) invalidateStageTrackerForWebhook() {
	served := s.invalidateStageTrackerCache()
	github.RecordWebhookTrackerInvalidations(1)
	if served {
		s.statusMu.RLock()
		seq := s.statusSeq
		s.statusMu.RUnlock()
		s.broadcastFrame(stageTrackerSSEFrame(seq))
	}
}

// FlushWebhookTrackerRefresh applies any targeted re-reads still waiting
// out their debounce, synchronously. The governor loop calls it before a
// webhook-wake cycle so the cycle plans from the patched snapshot.
func (s *Server) FlushWebhookTrackerRefresh() {
	if s == nil || s.webhookRefresh == nil {
		return
	}
	s.trackerRefresher().flushNow()
}

// applyWebhookItemSnapshots hands the re-read items to the governor's
// snapshot (Dependencies.ApplyWebhookItems), then patches the current status
// snapshot with them and republishes it (copy-on-write: readers holding the
// old pointer are unaffected). An item that closed or merged leaves its
// list; an item the status snapshot never listed is left for the next
// cycle. The tracker cache is dropped when anything changed.
func (s *Server) applyWebhookItemSnapshots(snaps []github.WebhookItemSnapshot) {
	if len(snaps) == 0 {
		return
	}
	if s.deps != nil && s.deps.ApplyWebhookItems != nil {
		s.deps.ApplyWebhookItems(snaps)
	}
	s.statusMu.Lock()
	cur := s.status
	if cur == nil {
		s.statusMu.Unlock()
		return
	}
	next := *cur
	next.Repos = append([]FrontendRepo(nil), cur.Repos...)
	changed := 0
	for i := range next.Repos {
		repo := &next.Repos[i]
		name := overviewRepoName(*repo)
		for _, snap := range snaps {
			if !strings.EqualFold(snap.Repo, name) && !strings.EqualFold(snap.Repo, repo.Name) {
				continue
			}
			changed += patchFrontendRepoItem(repo, snap)
		}
	}
	if changed > 0 {
		s.status = &next
	}
	s.statusMu.Unlock()
	if changed > 0 {
		s.invalidateStageTrackerForWebhook()
	}
}

// patchFrontendRepoItem rewrites every list entry of repo that names the
// snapshot's item and reports how many entries changed. Lists are replaced,
// never mutated, so a snapshot handed out earlier stays consistent.
func patchFrontendRepoItem(repo *FrontendRepo, snap github.WebhookItemSnapshot) int {
	changed := 0
	patch := func(list []any, fn func(raw any) (any, bool)) []any {
		if len(list) == 0 {
			return list
		}
		var out []any
		touched := false
		for i, raw := range list {
			replacement, matched := fn(raw)
			if !matched {
				if touched {
					out = append(out, raw)
				}
				continue
			}
			if !touched {
				out = append(make([]any, 0, len(list)), list[:i]...)
				touched = true
			}
			changed++
			if replacement != nil {
				out = append(out, replacement)
			}
		}
		if !touched {
			return list
		}
		return out
	}
	if snap.IsPR {
		repo.OpenPrs = patch(repo.OpenPrs, func(raw any) (any, bool) { return patchPRAny(raw, snap) })
		repo.HeldPrs = patch(repo.HeldPrs, func(raw any) (any, bool) { return patchPRAny(raw, snap) })
		repo.NonActionablePrs = patch(repo.NonActionablePrs, func(raw any) (any, bool) { return patchWorkPRAny(raw, snap) })
		return changed
	}
	repo.ActionableIssues = patch(repo.ActionableIssues, func(raw any) (any, bool) { return patchIssueAny(raw, snap) })
	repo.HeldIssues = patch(repo.HeldIssues, func(raw any) (any, bool) { return patchIssueAny(raw, snap) })
	repo.NonActionableIssues = patch(repo.NonActionableIssues, func(raw any) (any, bool) { return patchWorkIssueAny(raw, snap) })
	return changed
}

func patchPullRequest(pr github.PullRequest, snap github.WebhookItemSnapshot) github.PullRequest {
	pr.Title = snap.Title
	pr.Labels = append([]string(nil), snap.Labels...)
	pr.Draft = snap.Draft
	if !snap.UpdatedAt.IsZero() {
		pr.UpdatedAt = snap.UpdatedAt
	}
	if snap.State != "" {
		pr.State = snap.State
	}
	return pr
}

// patchPRAny matches FrontendPR / github.PullRequest entries (by value or
// pointer). A closed or merged PR returns a nil replacement: it leaves.
func patchPRAny(raw any, snap github.WebhookItemSnapshot) (any, bool) {
	switch v := raw.(type) {
	case FrontendPR:
		if v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		v.PullRequest = patchPullRequest(v.PullRequest, snap)
		return v, true
	case *FrontendPR:
		if v == nil || v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		cp := *v
		cp.PullRequest = patchPullRequest(cp.PullRequest, snap)
		return &cp, true
	case github.PullRequest:
		if v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		return patchPullRequest(v, snap), true
	case *github.PullRequest:
		if v == nil || v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		cp := patchPullRequest(*v, snap)
		return &cp, true
	}
	return nil, false
}

func patchWorkPRAny(raw any, snap github.WebhookItemSnapshot) (any, bool) {
	w, ok := repoWorkPR(raw)
	if !ok || w.Number != snap.Number {
		return nil, false
	}
	if !snap.Open() {
		return nil, true
	}
	w.Title = snap.Title
	w.Labels = append([]string(nil), snap.Labels...)
	w.Draft = snap.Draft
	if !snap.UpdatedAt.IsZero() {
		w.UpdatedAt = snap.UpdatedAt
	}
	if _, isPtr := raw.(*github.RepoWorkPR); isPtr {
		return &w, true
	}
	return w, true
}

func patchIssue(issue github.Issue, snap github.WebhookItemSnapshot) github.Issue {
	issue.Title = snap.Title
	issue.Labels = append([]string(nil), snap.Labels...)
	if !snap.UpdatedAt.IsZero() {
		issue.UpdatedAt = snap.UpdatedAt
	}
	if snap.State != "" {
		issue.State = snap.State
	}
	return issue
}

// patchIssueAny matches github.Issue / github.HoldItem entries (by value or
// pointer); a closed issue leaves.
func patchIssueAny(raw any, snap github.WebhookItemSnapshot) (any, bool) {
	switch v := raw.(type) {
	case github.Issue:
		if v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		return patchIssue(v, snap), true
	case *github.Issue:
		if v == nil || v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		cp := patchIssue(*v, snap)
		return &cp, true
	case github.HoldItem:
		if v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		v.Title = snap.Title
		v.Labels = append([]string(nil), snap.Labels...)
		return v, true
	case *github.HoldItem:
		if v == nil || v.Number != snap.Number {
			return nil, false
		}
		if !snap.Open() {
			return nil, true
		}
		cp := *v
		cp.Title = snap.Title
		cp.Labels = append([]string(nil), snap.Labels...)
		return &cp, true
	}
	return nil, false
}

func patchWorkIssueAny(raw any, snap github.WebhookItemSnapshot) (any, bool) {
	w, ok := repoWorkIssue(raw)
	if !ok || w.Number != snap.Number {
		return nil, false
	}
	if !snap.Open() {
		return nil, true
	}
	w.Title = snap.Title
	w.Labels = append([]string(nil), snap.Labels...)
	if !snap.UpdatedAt.IsZero() {
		w.UpdatedAt = snap.UpdatedAt
	}
	if _, isPtr := raw.(*github.RepoWorkIssue); isPtr {
		return &w, true
	}
	return w, true
}

// MarkStatusStale republishes the current status snapshot flagged as
// last-good data as of since: the governor calls it when a whole reconcile
// pass fails so the overview shows "stale · as of" instead of either zeros
// or figures that look fresh. Repos already flagged keep their marker.
func (s *Server) MarkStatusStale(since time.Time) {
	if s == nil {
		return
	}
	marker := ""
	if !since.IsZero() {
		marker = since.UTC().Format(time.RFC3339)
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	cur := s.status
	if cur == nil || (cur.StaleSince == marker && cur.OverviewKPIIncomplete) {
		return
	}
	next := *cur
	next.StaleSince = marker
	next.OverviewKPIIncomplete = true
	next.Repos = append([]FrontendRepo(nil), cur.Repos...)
	for i := range next.Repos {
		if !next.Repos[i].CountsStale {
			next.Repos[i].CountsStale = true
			next.Repos[i].CountsStaleSince = marker
		}
	}
	s.status = &next
}

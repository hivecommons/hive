package dashboard

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
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
			r.apply = func(snaps []github.WebhookItemSnapshot) { s.applyWebhookItemSnapshots(snaps) }
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
	s.evictClosedWebhookItem(res, deliveryID, time.Now())
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

// selfEditDeliveryID tags the re-reads the hive's own GitHub edits ask for,
// in place of a webhook delivery id, in ItemPatches and the logs.
const selfEditDeliveryID = "self:edit"

// RefreshSelfEditedItem is the github.ItemRefreshHook: the hive itself just
// changed an issue or PR on GitHub (removed `blocked`, for one), so the
// tracker cache is dropped and the item queued for the same targeted re-read
// an inbound webhook gets. The re-read patches the cached actionable
// snapshot and the tracker live, so the row re-derives now rather than after
// the next full enumeration. Without a GitHub client, or with the webhook
// refresh disabled, only the cache invalidation happens; the enumeration
// pass is then the fallback, as for an undelivered webhook.
func (s *Server) RefreshSelfEditedItem(repo string, number int, isPR bool) {
	if s == nil || repo == "" || number <= 0 {
		return
	}
	s.invalidateStageTrackerForWebhook()
	if !s.webhookTrackerRefreshEnabled() || s.deps == nil || s.deps.GHClient == nil {
		return
	}
	s.trackerRefresher().enqueue([]trackerRefreshKey{{repo: repo, number: number, pr: isPR}}, selfEditDeliveryID)
}

// evictClosedWebhookItem applies the one payload fact the tracker trusts
// without a confirming read: the item the delivery embeds is closed or
// merged. Every `issues`, `issue_comment` and `pull_request*` payload carries
// the full object with its state, so a `labeled`, `unlabeled` or `created`
// delivery for an already-closed item says closed too, and the row leaves
// on whichever delivery arrives — not only on `closed`. Dropping a row is
// the conservative direction (it can only stop the hive acting on the
// item); the open fields (title, labels, bucket) are still only ever taken
// from the GET the refresher makes next. Without this, a confirming GET that
// the budget or rate-limit guards refused, or that failed, left the row
// listed open until the next full enumeration: hivecommons/hive#14159 and
// #14064 sat on the tracker twenty minutes after they closed, through the
// close, a bot comment and the stage-label sweep's unlabel, each of which
// had said "closed". It reports whether a row was dropped.
func (s *Server) evictClosedWebhookItem(res github.WebhookEventResult, deliveryID string, now time.Time) bool {
	if s == nil || !res.KnownRepo || !res.Item.Closed() {
		return false
	}
	repo := res.ConfiguredRepo
	if repo == "" {
		repo = res.Repo
	}
	h := res.Item
	snap := github.WebhookItemSnapshot{Repo: repo, Number: h.Number, IsPR: h.IsPR, State: "closed", Merged: h.Merged,
		UpdatedAt: h.UpdatedAt, FetchedAt: now, DeliveryID: deliveryID}
	return s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{snap}) > 0
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
		s.nudgeStageTracker(seq)
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
// cycle. The tracker cache is dropped when anything changed. It reports how
// many list entries changed.
//
// Ordering: an open read whose updated_at is older than the listed copy's
// (two deliveries' reads landing out of order, or a stale read after a
// close) is dropped, so the row never moves backwards; a closed read is
// applied whatever its timestamp — a row may only stay open while the
// newest word on it says open.
func (s *Server) applyWebhookItemSnapshots(snaps []github.WebhookItemSnapshot) int {
	if len(snaps) == 0 {
		return 0
	}
	s.recordMergedSnapshots(snaps)
	s.notePushedSnapshots(snaps)
	if s.deps != nil && s.deps.ApplyWebhookItems != nil {
		s.deps.ApplyWebhookItems(snaps)
	}
	s.statusMu.Lock()
	cur := s.status
	if cur == nil {
		s.statusMu.Unlock()
		return 0
	}
	// An open read no list carries is inserted — unless a remembered
	// close for the same item is newer: that is the out-of-order
	// `unlabeled` read after a `closed` one, not a reopen.
	noInsert := s.webhookOpenReadsSupersededLocked(snaps)
	// Remembered before the lists are patched: a snapshot the governor is
	// building right now (from the enumeration its cycle began with) still
	// lists the item as it was, and will have these re-applied on publish.
	s.rememberWebhookPatchesLocked(snaps, time.Now())
	next := *cur
	next.Repos = append([]FrontendRepo(nil), cur.Repos...)
	changed := 0
	for i := range next.Repos {
		repo := &next.Repos[i]
		name := overviewRepoName(*repo)
		repoChanged := 0
		for _, snap := range snaps {
			if !strings.EqualFold(snap.Repo, name) && !strings.EqualFold(snap.Repo, repo.Name) {
				continue
			}
			if snap.Open() && snapshotOlderThanListed(*repo, snap) {
				continue
			}
			n := patchFrontendRepoItem(repo, snap)
			if n == 0 && snap.Open() && !noInsert[github.ItemPatchKey(snap.Repo, snap.Number, snap.IsPR)] {
				n = insertFrontendRepoItem(repo, snap)
			}
			repoChanged += n
		}
		if repoChanged > 0 {
			recountRepoOpenItems(repo)
		}
		changed += repoChanged
	}
	if changed > 0 {
		s.status = &next
	}
	s.statusMu.Unlock()
	if changed > 0 {
		s.invalidateStageTrackerForWebhook()
	}
	return changed
}

// notePushedSnapshots books the head each open PR re-read reports on its
// stage record (stagelabels.Store.NotePush) so a `pull_request` synchronize
// delivery shows as "pushed abc1234" on the activity monitor at once, ahead
// of the enumerate that re-stages the PR (hivecommons/hive#15784). Reads
// whose head the store already knows change nothing; the store decides.
func (s *Server) notePushedSnapshots(snaps []github.WebhookItemSnapshot) int {
	store := s.stageStoreOrShared()
	if store == nil {
		return 0
	}
	booked := 0
	for _, snap := range snaps {
		if !snap.IsPR || !snap.Open() || snap.HeadSHA == "" {
			continue
		}
		if store.NotePush(stagelabels.Key(snap.Repo, snap.Number), snap.HeadSHA, snap.FetchedAt) {
			booked++
		}
	}
	return booked
}

// snapshotOlderThanListed reports whether the listed copy of the snapshot's
// item carries a later updated_at than the read: the read is stale.
func snapshotOlderThanListed(repo FrontendRepo, snap github.WebhookItemSnapshot) bool {
	if snap.UpdatedAt.IsZero() {
		return false
	}
	held := frontendRepoItemUpdatedAt(repo, snap.Number, snap.IsPR)
	return !held.IsZero() && held.After(snap.UpdatedAt)
}

// webhookPatchEntry is one remembered webhook re-read: the snapshot the GET
// returned and when it was applied, for expiry.
type webhookPatchEntry struct {
	snap      github.WebhookItemSnapshot
	appliedAt time.Time
}

// maxWebhookPatchEntries bounds the remembered re-reads whatever the replay
// window: the oldest are forgotten first.
const maxWebhookPatchEntries = 4096

// webhookPatchReplayWindow is dashboard.stage_tracker.webhook_refresh_replay_window_s.
func (s *Server) webhookPatchReplayWindow() time.Duration {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.StageTrackerWebhookRefreshReplayWindow()
	}
	return (*config.Config)(nil).StageTrackerWebhookRefreshReplayWindow()
}

// rememberWebhookPatchesLocked records snaps for replay (statusMu held).
// The newest read of an item by GitHub's updated_at replaces the previous
// one, so a reopen after a close is replayed as open, while an open read
// that is older than the remembered one (a `labeled` delivery's read landing
// after the `closed` delivery's) cannot un-remember the close; a closed read
// always replaces. Entries past the window or over the cap go.
// webhookOpenReadsSupersededLocked lists (statusMu held) the open reads in
// snaps that a remembered close for the same item outranks: a read with no
// timestamp, or one GitHub stamped no later than the close. Such a read
// may not re-list the item.
func (s *Server) webhookOpenReadsSupersededLocked(snaps []github.WebhookItemSnapshot) map[string]bool {
	var out map[string]bool
	for _, snap := range snaps {
		if !snap.Open() || snap.Number <= 0 || snap.Repo == "" {
			continue
		}
		key := github.ItemPatchKey(snap.Repo, snap.Number, snap.IsPR)
		prev, ok := s.webhookPatches[key]
		if !ok || prev.snap.Open() {
			continue
		}
		if snap.UpdatedAt.IsZero() || !snap.UpdatedAt.After(prev.snap.UpdatedAt) {
			if out == nil {
				out = map[string]bool{}
			}
			out[key] = true
		}
	}
	return out
}

func (s *Server) rememberWebhookPatchesLocked(snaps []github.WebhookItemSnapshot, now time.Time) {
	if s.webhookPatches == nil {
		s.webhookPatches = map[string]webhookPatchEntry{}
	}
	for _, snap := range snaps {
		if snap.Number <= 0 || snap.Repo == "" {
			continue
		}
		key := github.ItemPatchKey(snap.Repo, snap.Number, snap.IsPR)
		if prev, ok := s.webhookPatches[key]; ok && snap.Open() && !snap.UpdatedAt.IsZero() && prev.snap.UpdatedAt.After(snap.UpdatedAt) {
			continue
		}
		s.webhookPatches[key] = webhookPatchEntry{snap: snap, appliedAt: now}
	}
	s.pruneWebhookPatchesLocked(now)
}

func (s *Server) pruneWebhookPatchesLocked(now time.Time) {
	window := s.webhookPatchReplayWindow()
	for key, e := range s.webhookPatches {
		if now.Sub(e.appliedAt) > window {
			delete(s.webhookPatches, key)
		}
	}
	for len(s.webhookPatches) > maxWebhookPatchEntries {
		oldestKey, oldest := "", time.Time{}
		for key, e := range s.webhookPatches {
			if oldestKey == "" || e.appliedAt.Before(oldest) {
				oldestKey, oldest = key, e.appliedAt
			}
		}
		delete(s.webhookPatches, oldestKey)
	}
}

// replayWebhookPatchesLocked re-applies the remembered re-reads to status
// (statusMu held) and reports how many list entries changed. A snapshot the
// governor publishes was built from the enumeration its cycle started with,
// so an item that closed mid-cycle is still listed open in it; the replay
// takes it out again, as the live patch did minutes earlier. Monotonic: a
// listed copy GitHub updated after the re-read (a reopen the next
// enumeration saw) is left alone.
func (s *Server) replayWebhookPatchesLocked(status *StatusPayload, now time.Time) int {
	if status == nil || len(s.webhookPatches) == 0 {
		return 0
	}
	s.pruneWebhookPatchesLocked(now)
	changed := 0
	for i := range status.Repos {
		repo := &status.Repos[i]
		name := overviewRepoName(*repo)
		repoChanged := 0
		for _, e := range s.webhookPatches {
			snap := e.snap
			if !strings.EqualFold(snap.Repo, name) && !strings.EqualFold(snap.Repo, repo.Name) {
				continue
			}
			if held := frontendRepoItemUpdatedAt(*repo, snap.Number, snap.IsPR); !held.IsZero() && !snap.UpdatedAt.IsZero() && held.After(snap.UpdatedAt) {
				continue
			}
			n := patchFrontendRepoItem(repo, snap)
			if n == 0 && snap.Open() {
				n = insertFrontendRepoItem(repo, snap)
			}
			repoChanged += n
		}
		if repoChanged > 0 {
			recountRepoOpenItems(repo)
		}
		changed += repoChanged
	}
	return changed
}

// frontendRepoItemUpdatedAt is GitHub's updated_at on the listed copy of an
// item, zero when the repo does not list it or the list has no timestamp.
func frontendRepoItemUpdatedAt(repo FrontendRepo, number int, isPR bool) time.Time {
	kind := "issue"
	if isPR {
		kind = "pr"
	}
	for _, it := range repoOpenItems(repo) {
		if it.Kind != kind || it.Number != number {
			continue
		}
		switch {
		case it.Sidelined && isPR:
			return it.WorkPR.UpdatedAt
		case it.Sidelined:
			return it.WorkIssue.UpdatedAt
		case isPR:
			return it.PR.pr.UpdatedAt
		default:
			return it.Issue.UpdatedAt
		}
	}
	return time.Time{}
}

// recountRepoOpenItems makes the repo row's issue / PR totals follow its
// patched item lists. Unlike reconcileRepoOpenItems it leaves the unlisted
// remainder alone: an item a webhook took out was listed, not unlisted.
func recountRepoOpenItems(r *FrontendRepo) {
	items := repoOpenItems(*r)
	counts := countOpenItems(items)
	r.Issues, r.PRs = counts.Issues, counts.PRs
	refitRepoWorkBreakdown(r, items, counts)
}

// webhookUnclassifiedReason is the work-detail reason on an open item a
// webhook re-read brought in before any enumeration or classifier placed it.
const webhookUnclassifiedReason = "Opened on GitHub; not yet classified by the scanner"

// insertFrontendRepoItem adds an open item none of repo's lists carry, so a
// brand-new issue or PR is in the status payload — and the activity
// monitor — from its `opened` delivery rather than from the governor's next
// publish (hivecommons/hive#15094). The governor-side classification rides
// on the snapshot: an actionable row joins the actionable list, a held row
// the held list, and anything sidelined (or unplaced) the sidelined list
// with the classifier's bucket or an "other" placeholder the next
// enumeration settles. Returns the number of entries added (0 or 1).
func insertFrontendRepoItem(repo *FrontendRepo, snap github.WebhookItemSnapshot) int {
	if repo == nil || !snap.Open() || snap.Number <= 0 {
		return 0
	}
	full := overviewRepoName(*repo)
	p := snap.Patch
	if snap.IsPR {
		switch {
		case p.Bucket == github.WebhookBucketActionable && p.PR != nil:
			repo.OpenPrs = append(append([]any(nil), repo.OpenPrs...), FrontendPR{PullRequest: *p.PR})
		case p.Bucket == github.WebhookBucketHeld && p.PR != nil:
			repo.HeldPrs = append(append([]any(nil), repo.HeldPrs...), FrontendPR{PullRequest: *p.PR})
		case p.WorkPR != nil:
			repo.NonActionablePrs = append(append([]any(nil), repo.NonActionablePrs...), *p.WorkPR)
		default:
			bucket := "other"
			if snap.Draft {
				bucket = "draft"
			}
			repo.NonActionablePrs = append(append([]any(nil), repo.NonActionablePrs...), github.RepoWorkPR{
				Repo: full, Number: snap.Number, Title: snap.Title, Author: snap.Author,
				Labels: append([]string(nil), snap.Labels...), Draft: snap.Draft,
				AppAuthored: snap.AppAuthored, HiveAttributed: snap.HiveAttributed,
				UpdatedAt: snap.UpdatedAt, URL: snap.URL, Bucket: bucket, Reason: webhookUnclassifiedReason,
			})
		}
		return 1
	}
	switch {
	case p.Bucket == github.WebhookBucketActionable && p.Issue != nil:
		repo.ActionableIssues = append(append([]any(nil), repo.ActionableIssues...), *p.Issue)
	case p.Bucket == github.WebhookBucketHeld && p.Hold != nil:
		repo.HeldIssues = append(append([]any(nil), repo.HeldIssues...), *p.Hold)
	case p.WorkIssue != nil:
		repo.NonActionableIssues = append(append([]any(nil), repo.NonActionableIssues...), *p.WorkIssue)
	default:
		repo.NonActionableIssues = append(append([]any(nil), repo.NonActionableIssues...), github.RepoWorkIssue{
			Repo: full, Number: snap.Number, Title: snap.Title, Author: snap.Author,
			Labels: append([]string(nil), snap.Labels...), UpdatedAt: snap.UpdatedAt, URL: snap.URL,
			Bucket: "other", Reason: webhookUnclassifiedReason,
		})
	}
	return 1
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
		if snap.Open() {
			return changed
		}
		// GitHub's issues feed lists pull requests too; numbers are one
		// namespace, so a same-numbered issue entry is that PR's copy and
		// leaves with it rather than surfacing as an issue row.
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
	// The re-read head keeps per-head lookups (the sentinel's evidence
	// bundle) on the revision GitHub now reports.
	if snap.HeadSHA != "" {
		pr.HeadSHA = snap.HeadSHA
	}
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

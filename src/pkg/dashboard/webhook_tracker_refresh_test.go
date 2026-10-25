package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeRefreshTimer is a hand-fired one-shot timer: the test calls fire()
// where production would wait out the debounce.
type fakeRefreshTimer struct {
	mu      sync.Mutex
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeRefreshTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	return true
}

func (t *fakeRefreshTimer) fire() { t.f() }

type fakeTimers struct {
	mu     sync.Mutex
	timers []*fakeRefreshTimer
}

func (ft *fakeTimers) newTimer(d time.Duration, f func()) trackerRefreshTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeRefreshTimer{d: d, f: f}
	ft.timers = append(ft.timers, t)
	return t
}

func (ft *fakeTimers) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.timers)
}

func (ft *fakeTimers) last() *fakeRefreshTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.timers[len(ft.timers)-1]
}

type recordedFetch struct {
	repo   string
	number int
	pr     bool
}

// TestWebhookTrackerRefresherCoalescesBurstIntoOneFetchPerItem: twelve
// check_run deliveries for one PR inside the window cost one GET; a second
// item in the same window rides the same timer; the window length is the
// configured debounce; events after the flush arm a fresh window.
func TestWebhookTrackerRefresherCoalescesBurstIntoOneFetchPerItem(t *testing.T) {
	timers := &fakeTimers{}
	var mu sync.Mutex
	var fetched []recordedFetch
	var applied [][]github.WebhookItemSnapshot
	var counted []int
	r := newWebhookTrackerRefresher()
	r.newTimer = timers.newTimer
	r.debounce = func() time.Duration { return 1500 * time.Millisecond }
	r.fetch = func(_ context.Context, repo string, number int, pr bool) (github.WebhookItemSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		fetched = append(fetched, recordedFetch{repo, number, pr})
		if number == 99 {
			return github.WebhookItemSnapshot{}, errors.New("boom")
		}
		return github.WebhookItemSnapshot{Repo: repo, Number: number, IsPR: pr, State: "open", Title: "t"}, nil
	}
	r.apply = func(s []github.WebhookItemSnapshot) { mu.Lock(); applied = append(applied, s); mu.Unlock() }
	r.onFetch = func(n int) { mu.Lock(); counted = append(counted, n); mu.Unlock() }

	key := trackerRefreshKey{repo: "acme/console", number: 101, pr: true}
	if armed := r.enqueue([]trackerRefreshKey{key}, ""); !armed {
		t.Fatal("first key must arm the debounce timer")
	}
	for i := 0; i < 11; i++ {
		if armed := r.enqueue([]trackerRefreshKey{key}, ""); armed {
			t.Fatal("a burst inside the window must not re-arm")
		}
	}
	r.enqueue([]trackerRefreshKey{{repo: "acme/console", number: 7, pr: false}, {repo: "acme/console", number: 99, pr: true}}, "")
	r.enqueue([]trackerRefreshKey{{repo: "", number: 5}, {repo: "acme/console", number: 0}}, "")
	if timers.count() != 1 || timers.last().d != 1500*time.Millisecond {
		t.Fatalf("timers = %d (last %v), want one armed for the configured debounce", timers.count(), timers.last().d)
	}
	if r.pendingCount() != 3 {
		t.Fatalf("pending = %d, want 3 coalesced items", r.pendingCount())
	}
	mu.Lock()
	if len(fetched) != 0 {
		t.Fatalf("nothing may be fetched before the window closes: %v", fetched)
	}
	mu.Unlock()

	timers.last().fire()

	mu.Lock()
	if len(fetched) != 3 {
		t.Fatalf("fetched = %v, want one fetch per distinct item", fetched)
	}
	seen := map[recordedFetch]bool{}
	for _, f := range fetched {
		seen[f] = true
	}
	if !seen[recordedFetch{"acme/console", 101, true}] || !seen[recordedFetch{"acme/console", 7, false}] || !seen[recordedFetch{"acme/console", 99, true}] {
		t.Fatalf("fetched = %v", fetched)
	}
	if len(applied) != 1 || len(applied[0]) != 2 {
		t.Fatalf("applied = %v, want one apply of the two successful snapshots", applied)
	}
	if !reflect.DeepEqual(counted, []int{3}) {
		t.Fatalf("targeted refresh count = %v, want [3] (the failed fetch still spent a request)", counted)
	}
	mu.Unlock()
	if r.pendingCount() != 0 {
		t.Fatal("flush must clear the pending set")
	}

	if armed := r.enqueue([]trackerRefreshKey{key}, ""); !armed || timers.count() != 2 {
		t.Fatalf("after a flush the next event arms a new window: armed=%v timers=%d", armed, timers.count())
	}
	timers.last().fire()
	mu.Lock()
	defer mu.Unlock()
	if len(fetched) != 4 {
		t.Fatalf("fetched = %d, want 4", len(fetched))
	}
}

func TestWebhookTrackerRefresherNilAndEmptySafe(t *testing.T) {
	var r *webhookTrackerRefresher
	if r.enqueue([]trackerRefreshKey{{repo: "a", number: 1}}, "") {
		t.Fatal("nil refresher must be a no-op")
	}
	r = newWebhookTrackerRefresher()
	r.newTimer = (&fakeTimers{}).newTimer
	if r.enqueue(nil, "") {
		t.Fatal("no keys must not arm")
	}
	r.flush()
	if r.flushes != 1 {
		t.Fatal("flush with nothing pending is harmless")
	}
}

// TestWebhookTrackerKeysFromEventResult: PRs come from Invalidated, issues
// from TouchedIssues and ReopenedIssue (deduplicated), unknown repos yield
// nothing, and the configured repo name wins over the payload's casing.
func TestWebhookTrackerKeysFromEventResult(t *testing.T) {
	if keys := webhookTrackerKeys(github.WebhookEventResult{Repo: "x/y", Invalidated: []int{1}}); keys != nil {
		t.Fatalf("unknown repo keys = %v, want none", keys)
	}
	keys := webhookTrackerKeys(github.WebhookEventResult{KnownRepo: true, Repo: "Acme/Console", ConfiguredRepo: "acme/console",
		Invalidated: []int{101, 102}, TouchedIssues: []int{7}, ReopenedIssue: 7})
	want := []trackerRefreshKey{{"acme/console", 101, true}, {"acme/console", 102, true}, {"acme/console", 7, false}}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	if keys := webhookTrackerKeys(github.WebhookEventResult{KnownRepo: true, Repo: "acme/console"}); len(keys) != 0 {
		t.Fatalf("a delivery naming nothing yields no keys: %v", keys)
	}
}

// TestApplyWebhookItemSnapshotsPatchesRowsAndInvalidatesTracker: the
// re-read item's labels, title, draft flag and activity time replace the
// snapshot's copy in every list that carries it (actionable, held,
// sidelined; value or pointer), a closed item leaves, an unknown item is
// left for the next cycle, readers of the old snapshot are unaffected, and
// the tracker cache is dropped so the next GET shows the change.
func TestApplyWebhookItemSnapshotsPatchesRowsAndInvalidatesTracker(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	s.statusMu.Lock()
	repo := &s.status.Repos[0]
	repo.NonActionablePrs = []any{
		github.RepoWorkPR{Repo: "acme/console", Number: 106, Title: "WIP draft", Draft: true, Bucket: "draft", Reason: "Draft"},
		&github.RepoWorkPR{Repo: "acme/console", Number: 107, Title: "Exempt", Labels: []string{"do-not-merge"}, Bucket: "filtered", Reason: "Exempt label"},
	}
	repo.HeldIssues = []any{github.HoldItem{Repo: "acme/console", Number: 20, Title: "Held", Type: "issue", Labels: []string{"needs-human"}}}
	repo.NonActionableIssues = []any{&github.RepoWorkIssue{Repo: "acme/console", Number: 21, Title: "Parked", Bucket: "filtered"}}
	old := s.status
	oldOpen := append([]any(nil), old.Repos[0].OpenPrs...)
	s.statusMu.Unlock()

	before := s.buildStageTracker(s.status, s.stageStore, now)
	pr101 := trackerItem(t, before, "pr", 101)
	if !reflect.DeepEqual(pr101.Labels, []string{"stage/072-returned-to-contributor", "author/contributor", "needs-rebase"}) {
		t.Fatalf("fixture pr101 labels = %v", pr101.Labels)
	}
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("first payload builds")
	}
	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	updated := now.Add(-5 * time.Second)
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 101, IsPR: true, State: "open", Title: "Fix redirect (v2)", Labels: []string{"stage/050-review-pending", "author/contributor"}, UpdatedAt: updated},
		{Repo: "acme/console", Number: 106, IsPR: true, State: "open", Title: "Ready now", Draft: false, Labels: []string{"author/clanker"}, UpdatedAt: updated},
		{Repo: "acme/console", Number: 107, IsPR: true, State: "closed", Merged: true, Title: "Exempt"},
		{Repo: "acme/console", Number: 20, IsPR: false, State: "open", Title: "Held (renamed)", Labels: []string{"needs-human", "kind/bug"}},
		{Repo: "acme/console", Number: 21, IsPR: false, State: "closed", Title: "Parked"},
		{Repo: "acme/console", Number: 8, IsPR: false, State: "open", Title: "Flaky test", Labels: []string{"stage/020-claimed", "agent/scanner"}, UpdatedAt: updated},
		{Repo: "acme/console", Number: 999, IsPR: true, State: "open", Title: "Brand new"},
		{Repo: "other/repo", Number: 101, IsPR: true, State: "open", Title: "Not ours"},
	})

	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	if cur == old {
		t.Fatal("apply must publish a new snapshot pointer (copy-on-write)")
	}
	if !reflect.DeepEqual(old.Repos[0].OpenPrs, oldOpen) {
		t.Fatal("the previous snapshot's lists must not be mutated under readers")
	}
	after := s.buildStageTracker(cur, s.stageStore, now)
	got101 := trackerItem(t, after, "pr", 101)
	if got101.Title != "Fix redirect (v2)" || !reflect.DeepEqual(got101.Labels, []string{"stage/050-review-pending", "author/contributor"}) || got101.UpdatedAt == nil || !got101.UpdatedAt.Equal(updated) {
		t.Fatalf("pr101 after webhook = %+v", got101)
	}
	if hasMod(got101, modNeedsRebase) {
		t.Fatal("the dropped needs-rebase label must leave the modifiers")
	}
	got106 := trackerItem(t, after, "pr", 106)
	if got106.Title != "Ready now" || hasMod(got106, modDraft) {
		t.Fatalf("sidelined draft 106 should now read as ready: %+v", got106)
	}
	for _, it := range after.Items {
		if (it.Kind == "pr" && it.Number == 107) || (it.Kind == "issue" && it.Number == 21) {
			t.Fatalf("closed item still listed: %+v", it)
		}
		if it.Number == 999 {
			t.Fatal("an item the snapshot never listed must wait for the next cycle")
		}
	}
	if got20 := trackerItem(t, after, "issue", 20); got20.Title != "Held (renamed)" || len(got20.Labels) != 2 {
		t.Fatalf("held issue 20 = %+v", got20)
	}
	if got8 := trackerItem(t, after, "issue", 8); got8.Stage != trackerNumberClaimed {
		t.Fatalf("issue 8 relabelled to claimed: stage = %d", got8.Stage)
	}
	if _, _, _, fresh := s.stageTrackerPayload(cur, now.Add(time.Second)); !fresh {
		t.Fatal("apply must drop the memoised tracker payload")
	}
	select {
	case f := <-ch:
		if !strings.HasPrefix(string(f), "event: stage-tracker\n") {
			t.Fatalf("frame = %q", f)
		}
	default:
		t.Fatal("a served tracker must get an SSE nudge after the patch")
	}
	if snap := github.WebhookHealthSnapshot(); snap.TrackerInvalidations1h != 1 {
		t.Fatalf("tracker_invalidations_1h = %d, want 1", snap.TrackerInvalidations1h)
	}

	// Nothing matched: no publish, no invalidation.
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 4242, IsPR: true, State: "open"}})
	s.statusMu.RLock()
	same := s.status == cur
	s.statusMu.RUnlock()
	if !same {
		t.Fatal("a snapshot batch matching nothing must not republish")
	}
	if snap := github.WebhookHealthSnapshot(); snap.TrackerInvalidations1h != 1 {
		t.Fatalf("tracker_invalidations_1h = %d, want still 1", snap.TrackerInvalidations1h)
	}
	s.applyWebhookItemSnapshots(nil)
	s.statusMu.Lock()
	s.status = nil
	s.statusMu.Unlock()
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "acme/console", Number: 101, IsPR: true, State: "open"}})
}

func TestPatchPRAnyHandlesEveryListShape(t *testing.T) {
	snap := github.WebhookItemSnapshot{Number: 5, IsPR: true, State: "open", Title: "new", Labels: []string{"a"}, Draft: true}
	verdict := &github.MergeVerdict{Tier: "trusted"}
	for _, raw := range []any{
		FrontendPR{PullRequest: github.PullRequest{Number: 5, Title: "old"}, MergeVerdict: verdict},
		&FrontendPR{PullRequest: github.PullRequest{Number: 5, Title: "old"}, MergeVerdict: verdict},
		github.PullRequest{Number: 5, Title: "old"},
		&github.PullRequest{Number: 5, Title: "old"},
	} {
		out, matched := patchPRAny(raw, snap)
		if !matched {
			t.Fatalf("%T must match", raw)
		}
		fp, ok := frontendPullRequest(out)
		if !ok || fp.pr.Title != "new" || !fp.pr.Draft || !reflect.DeepEqual(fp.pr.Labels, []string{"a"}) {
			t.Fatalf("%T patched = %+v", raw, out)
		}
		if _, hadVerdict := raw.(FrontendPR); hadVerdict && fp.verdict != verdict {
			t.Fatal("the merge verdict must survive the patch")
		}
		if gone, matched := patchPRAny(raw, github.WebhookItemSnapshot{Number: 5, IsPR: true, State: "closed"}); !matched || gone != nil {
			t.Fatalf("%T closed: out=%v matched=%v", raw, gone, matched)
		}
		if _, matched := patchPRAny(raw, github.WebhookItemSnapshot{Number: 6, IsPR: true, State: "open"}); matched {
			t.Fatalf("%T must not match another number", raw)
		}
	}
	if _, matched := patchPRAny("junk", snap); matched {
		t.Fatal("foreign entries are left alone")
	}
	var nilFP *FrontendPR
	if _, matched := patchPRAny(nilFP, snap); matched {
		t.Fatal("nil pointer entries are left alone")
	}
	issueSnap := github.WebhookItemSnapshot{Number: 9, State: "open", Title: "renamed", Labels: []string{"x"}}
	for _, raw := range []any{github.Issue{Number: 9}, &github.Issue{Number: 9}, github.HoldItem{Number: 9}, &github.HoldItem{Number: 9}} {
		out, matched := patchIssueAny(raw, issueSnap)
		if !matched {
			t.Fatalf("%T must match", raw)
		}
		if issue, ok := frontendIssue(out); !ok || issue.Title != "renamed" || !reflect.DeepEqual(issue.Labels, []string{"x"}) {
			t.Fatalf("%T patched = %+v", raw, out)
		}
		if gone, matched := patchIssueAny(raw, github.WebhookItemSnapshot{Number: 9, State: "closed"}); !matched || gone != nil {
			t.Fatalf("%T closed: out=%v", raw, gone)
		}
	}
	if out, matched := patchWorkIssueAny(&github.RepoWorkIssue{Number: 9}, issueSnap); !matched || out.(*github.RepoWorkIssue).Title != "renamed" {
		t.Fatalf("pointer work issue = %v", out)
	}
	if out, matched := patchWorkPRAny(github.RepoWorkPR{Number: 5}, snap); !matched || out.(github.RepoWorkPR).Title != "new" {
		t.Fatalf("value work PR = %v", out)
	}
}

// TestGitHubWebhookReceiverSchedulesTargetedTrackerRefresh drives the
// receiver end to end with a hand-fired timer: a labeled pull_request
// delivery drops the tracker cache at once, queues the PR, and when the
// window closes exactly one GET re-reads it and the row changes. Unknown
// repos and events naming nothing schedule nothing, and the
// dashboard.stage_tracker.webhook_refresh switch turns it off.
func TestGitHubWebhookReceiverSchedulesTargetedTrackerRefresh(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	var ghCalls []string
	var ghMu sync.Mutex
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghMu.Lock()
		ghCalls = append(ghCalls, r.URL.Path)
		ghMu.Unlock()
		if r.URL.Path != "/repos/acme/console/pulls/101" {
			t.Errorf("unexpected GitHub call %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 101, "title": "Fix redirect", "state": "open",
			"user":   map[string]any{"login": "karibhou"},
			"labels": []map[string]any{{"name": "stage/072-returned-to-contributor"}, {"name": "author/contributor"}, {"name": "priority/high"}}})
	}))
	t.Cleanup(ghSrv.Close)
	cfg := &config.Config{}
	cfg.Dashboard.StageTracker.WebhookRefreshDebounceMS = 250
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(ghSrv.URL, "acme", []string{"console"}, s.logger),
		Ctx:      context.Background(),
		Logger:   s.logger,
		Config:   cfg,
	}
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("first build is fresh")
	}

	unknown := []byte(`{"action":"labeled","pull_request":{"number":101},"repository":{"full_name":"someone/else"}}`)
	if rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, unknown), unknown); !strings.Contains(rec.Body.String(), `"ignored"`) {
		t.Fatalf("unknown repo: %s", rec.Body.String())
	}
	nothing := []byte(`{"action":"created","repository":{"full_name":"acme/console"}}`)
	if rec := postGitHubWebhook(s, "ping", signGitHubWebhookForTest(secret, nothing), nothing); strings.Contains(rec.Body.String(), "tracker_refresh") {
		t.Fatalf("event naming nothing scheduled a refresh: %s", rec.Body.String())
	}
	if timers.count() != 0 {
		t.Fatal("nothing should be armed yet")
	}
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now.Add(time.Second)); fresh {
		t.Fatal("ignored deliveries must not drop the tracker cache")
	}

	labeled := []byte(`{"action":"labeled","pull_request":{"number":101},"repository":{"full_name":"acme/console"}}`)
	for i := 0; i < 3; i++ {
		rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, labeled), labeled)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tracker_refresh":1`) {
			t.Fatalf("labeled: %d %s", rec.Code, rec.Body.String())
		}
	}
	if timers.count() != 1 || timers.last().d != 250*time.Millisecond {
		t.Fatalf("timers = %d (d=%v), want one window of the configured debounce", timers.count(), timers.last().d)
	}
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now.Add(2*time.Second)); !fresh {
		t.Fatal("a delivery naming a tracked PR must drop the tracker cache immediately")
	}
	ghMu.Lock()
	if len(ghCalls) != 0 {
		t.Fatalf("no GitHub call before the window closes: %v", ghCalls)
	}
	ghMu.Unlock()

	timers.last().fire()

	ghMu.Lock()
	if !reflect.DeepEqual(ghCalls, []string{"/repos/acme/console/pulls/101"}) {
		t.Fatalf("GitHub calls = %v, want exactly one PR read", ghCalls)
	}
	ghMu.Unlock()
	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	resp := s.buildStageTracker(cur, s.stageStore, now)
	if got := trackerItem(t, resp, "pr", 101); !reflect.DeepEqual(got.Labels, []string{"stage/072-returned-to-contributor", "author/contributor", "priority/high"}) {
		t.Fatalf("pr101 labels after webhook = %v", got.Labels)
	}

	statusRec := httptest.NewRecorder()
	s.handleStatus(statusRec, httptest.NewRequest(http.MethodGet, "/api/status?fields=webhooks", nil))
	var status struct {
		Webhooks map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Webhooks["targeted_refreshes_1h"] != float64(1) || status.Webhooks["tracker_invalidations_1h"] != float64(4) {
		t.Fatalf("/api/status webhooks = %v, want targeted_refreshes_1h 1 (three deliveries, one read) and tracker_invalidations_1h 4 (one per delivery + the apply)", status.Webhooks)
	}

	off := false
	cfg.Dashboard.StageTracker.WebhookRefresh = &off
	rec := postGitHubWebhook(s, "pull_request", signGitHubWebhookForTest(secret, labeled), labeled)
	if strings.Contains(rec.Body.String(), "tracker_refresh") || timers.count() != 1 {
		t.Fatalf("webhook_refresh=false must schedule nothing: %s timers=%d", rec.Body.String(), timers.count())
	}
}

func TestStageTrackerWebhookRefreshConfigDefaults(t *testing.T) {
	var nilCfg *config.Config
	if !nilCfg.StageTrackerWebhookRefresh() || nilCfg.StageTrackerWebhookRefreshDebounce() != 2*time.Second {
		t.Fatal("nil config: refresh on, 2s debounce")
	}
	cfg := &config.Config{}
	if !cfg.StageTrackerWebhookRefresh() || cfg.StageTrackerWebhookRefreshDebounce() != 2*time.Second {
		t.Fatal("zero config: refresh on, 2s debounce")
	}
	off := false
	cfg.Dashboard.StageTracker.WebhookRefresh = &off
	cfg.Dashboard.StageTracker.WebhookRefreshDebounceMS = 500
	if cfg.StageTrackerWebhookRefresh() || cfg.StageTrackerWebhookRefreshDebounce() != 500*time.Millisecond {
		t.Fatal("explicit config must win")
	}
	s := &Server{}
	if !s.webhookTrackerRefreshEnabled() {
		t.Fatal("server without deps defaults to on")
	}
	s.deps = &Dependencies{Config: cfg}
	if s.webhookTrackerRefreshEnabled() {
		t.Fatal("server reads the switch")
	}
}

// A webhook re-read reaches the governor's snapshot through
// Dependencies.ApplyWebhookItems before the status rows are patched, and
// FlushWebhookTrackerRefresh runs a pending debounce window at once so a
// wake cycle plans from the patched snapshot. No list call is made: the
// only GitHub request is the single-item GET.
func TestWebhookRefreshHandsItemsToGovernorAndFlushesOnDemand(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	var ghCalls []string
	var ghMu sync.Mutex
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ghMu.Lock()
		ghCalls = append(ghCalls, r.URL.Path)
		ghMu.Unlock()
		if r.URL.Path != "/repos/acme/console/issues/8" {
			t.Errorf("unexpected GitHub call %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "title": "Flaky test (closed)", "state": "closed",
			"user": map[string]any{"login": "karibhou"}})
	}))
	t.Cleanup(ghSrv.Close)
	var handed []github.WebhookItemSnapshot
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(ghSrv.URL, "acme", []string{"console"}, s.logger),
		Ctx:      context.Background(),
		Logger:   s.logger,
		Config:   &config.Config{},
		ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) {
			handed = append(handed, snaps...)
		},
	}
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	s.FlushWebhookTrackerRefresh() // nothing pending: no-op, no fetch
	if n := s.scheduleWebhookTrackerRefresh(github.WebhookEventResult{KnownRepo: true, Repo: "acme/console", TouchedIssues: []int{8}}, ""); n != 1 {
		t.Fatalf("queued %d, want 1", n)
	}
	if timers.count() != 1 {
		t.Fatalf("debounce timer not armed")
	}
	s.FlushWebhookTrackerRefresh()
	if !timers.last().stopped {
		t.Fatal("flush must stop the pending debounce timer")
	}
	if s.webhookRefresh.pendingCount() != 0 {
		t.Fatal("flush left keys pending")
	}
	ghMu.Lock()
	calls := append([]string(nil), ghCalls...)
	ghMu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("GitHub calls = %v, want the single issue GET", calls)
	}
	if len(handed) != 1 || handed[0].Number != 8 || handed[0].Open() {
		t.Fatalf("governor hook received %+v", handed)
	}
	// The status row left too (the item closed).
	for _, it := range s.buildStageTracker(s.status, s.stageStore, now).Items {
		if it.Kind == "issue" && it.Number == 8 {
			t.Fatalf("closed issue still in tracker: %+v", it)
		}
	}
	// Firing the stopped timer later must not fetch again.
	timers.last().fire()
	ghMu.Lock()
	defer ghMu.Unlock()
	if len(ghCalls) != 1 {
		t.Fatalf("stopped timer refetched: %v", ghCalls)
	}
}

// The delivery payload is a hint, never state: an `issues` "closed" delivery
// whose confirming GET says the issue is open leaves the row open, and the
// delivery id rides on the re-read for audit.
// The payload's open-side fields are never applied: title, labels and
// bucket come from the confirming GET only. Its closed state, however, is
// trusted at once (the conservative direction, hivecommons/hive#14159): the
// row leaves on the delivery, and the GET that then says "open" cannot
// bring it back with the payload's title — the next enumeration relists it.
func TestWebhookRefreshIgnoresPayloadStateAndTagsDelivery(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	var gets atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		if r.URL.Path != "/repos/acme/console/issues/8" {
			t.Errorf("unexpected GitHub call %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "title": "Flaky test (still open)", "state": "open",
			"user": map[string]any{"login": "karibhou"}, "labels": []map[string]any{{"name": "kind/bug"}}})
	}))
	t.Cleanup(ghSrv.Close)
	var handed []github.WebhookItemSnapshot
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(ghSrv.URL, "acme", []string{"console"}, s.logger),
		Ctx:      context.Background(),
		Logger:   s.logger,
		Config:   &config.Config{},
		ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) {
			handed = append(handed, snaps...)
		},
	}
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer

	// The payload claims the issue closed with a bogus title.
	body := []byte(`{"action":"closed","issue":{"number":8,"state":"closed","title":"PAYLOAD LIES"},"repository":{"full_name":"acme/console"}}`)
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForTest(secret, body))
	req.Header.Set("X-GitHub-Delivery", "delivery-123")
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	if !strings.Contains(rec.Body.String(), `"tracker_refresh":1`) {
		t.Fatalf("delivery not queued: %s", rec.Body.String())
	}
	// The payload's "closed" is applied at once, before (and whether or not)
	// the confirming GET runs; nothing else of the payload is.
	if gets.Load() != 0 {
		t.Fatal("GET made before the debounce window closed")
	}
	if len(handed) != 1 || handed[0].Open() || handed[0].Title != "" || handed[0].DeliveryID != "delivery-123" {
		t.Fatalf("governor received %+v, want the payload's closed state only, tagged with the delivery id", handed)
	}
	for _, it := range s.buildStageTracker(s.status, s.stageStore, now).Items {
		if it.Kind == "issue" && it.Number == 8 {
			t.Fatalf("a delivery saying closed must drop the row at once: %+v", it)
		}
	}
	timers.last().fire()
	if gets.Load() != 1 {
		t.Fatalf("GETs = %d, want the one confirming read", gets.Load())
	}
	if len(handed) != 2 || !handed[1].Open() || handed[1].Title != "Flaky test (still open)" || handed[1].DeliveryID != "delivery-123" {
		t.Fatalf("governor received %+v, want the GET's state tagged with the delivery id", handed)
	}
	for _, it := range s.buildStageTracker(s.status, s.stageStore, now).Items {
		if it.Kind == "issue" && it.Number == 8 && strings.Contains(it.Title, "PAYLOAD") {
			t.Fatalf("payload title leaked into the row: %+v", it)
		}
	}
}

// A governor cycle publishes the snapshot it built from the enumeration it
// began with. When a webhook re-read closed an item mid-cycle, that publish
// still lists the item open; the remembered re-read is replayed so the row
// does not come back (pluk#313: closed by a human, re-listed 4s later by the
// in-flight cycle, gone again only when the next cycle re-enumerated).
func TestUpdateStatusReplaysWebhookPatchesOverStaleSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 22, 2, 46, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	// A second fixture is the cycle-local snapshot the governor is still
	// holding: same enumeration, PR 101 and issue 8 open.
	staleOwner, _ := stageTrackerFixture(t, now)
	stale := staleOwner.status
	// The fixture never ran reconcileRepoOpenItems; counts follow the lists.
	before := countOpenItems(repoOpenItems(stale.Repos[0]))
	prsBefore, issuesBefore := before.PRs, before.Issues

	closedAt := now.Add(-4 * time.Second)
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 101, IsPR: true, State: "closed", Title: "Fix redirect", UpdatedAt: closedAt},
		{Repo: "acme/console", Number: 8, IsPR: false, State: "closed", Title: "Flaky test", UpdatedAt: closedAt},
	})
	s.statusMu.RLock()
	live := s.status
	s.statusMu.RUnlock()
	for _, it := range s.buildStageTracker(live, s.stageStore, now).Items {
		if (it.Kind == "pr" && it.Number == 101) || (it.Kind == "issue" && it.Number == 8) {
			t.Fatalf("webhook close must drop %s #%d from the live tracker", it.Kind, it.Number)
		}
	}
	if live.Repos[0].PRs != prsBefore-1 || live.Repos[0].Issues != issuesBefore-1 {
		t.Fatalf("live counts prs=%d issues=%d, want %d/%d", live.Repos[0].PRs, live.Repos[0].Issues, prsBefore-1, issuesBefore-1)
	}

	// The in-flight cycle now publishes its stale snapshot.
	s.UpdateStatus(stale)
	s.statusMu.RLock()
	published := s.status
	s.statusMu.RUnlock()
	if published != stale {
		t.Fatal("the stale snapshot must still publish (the epoch guard is not what drops the row)")
	}
	for _, it := range s.buildStageTracker(published, s.stageStore, now).Items {
		if (it.Kind == "pr" && it.Number == 101) || (it.Kind == "issue" && it.Number == 8) {
			t.Fatalf("stale publish resurrected %s #%d", it.Kind, it.Number)
		}
	}
	if published.Repos[0].PRs != prsBefore-1 || published.Repos[0].Issues != issuesBefore-1 {
		t.Fatalf("replayed counts prs=%d issues=%d, want %d/%d", published.Repos[0].PRs, published.Repos[0].Issues, prsBefore-1, issuesBefore-1)
	}
	if published.Repos[0].UnlistedPRs != stale.Repos[0].UnlistedPRs {
		t.Fatal("a replayed removal is not an unlisted item")
	}
	// Other rows are untouched.
	trackerItem(t, s.buildStageTracker(published, s.stageStore, now), "pr", 102)
}

// The replay is monotonic: a listed copy GitHub updated after the re-read
// (the next enumeration saw the item reopened) wins over the remembered close.
func TestReplayWebhookPatchesKeepsNewerListedCopy(t *testing.T) {
	now := time.Date(2026, 10, 22, 2, 46, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	closedAt := now.Add(-10 * time.Minute)
	s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 101, IsPR: true, State: "closed", UpdatedAt: closedAt},
	})

	fresh, _ := stageTrackerFixture(t, now)
	next := fresh.status
	reopened := next.Repos[0].OpenPrs[0].(FrontendPR)
	if reopened.Number != 101 {
		t.Fatalf("fixture order changed: first open PR is #%d", reopened.Number)
	}
	reopened.UpdatedAt = closedAt.Add(2 * time.Minute)
	next.Repos[0].OpenPrs = append([]any{reopened}, next.Repos[0].OpenPrs[1:]...)
	s.UpdateStatus(next)
	s.statusMu.RLock()
	published := s.status
	s.statusMu.RUnlock()
	trackerItem(t, s.buildStageTracker(published, s.stageStore, now), "pr", 101)
}

// Remembered re-reads expire after the replay window and are capped.
func TestWebhookPatchMemoryExpiresAndCaps(t *testing.T) {
	now := time.Date(2026, 10, 22, 2, 46, 0, 0, time.UTC)
	cfg := &config.Config{}
	cfg.Dashboard.StageTracker.WebhookRefreshReplayWindowS = 60
	s := &Server{deps: &Dependencies{Config: cfg}}
	if s.webhookPatchReplayWindow() != time.Minute {
		t.Fatalf("window = %s", s.webhookPatchReplayWindow())
	}
	s.rememberWebhookPatchesLocked([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 1, IsPR: true, State: "closed"},
		{Repo: "acme/console", Number: 0, IsPR: true, State: "closed"},
		{Repo: "", Number: 2, IsPR: true, State: "closed"},
	}, now)
	if len(s.webhookPatches) != 1 {
		t.Fatalf("remembered %d entries, want 1 (invalid keys skipped)", len(s.webhookPatches))
	}
	s.rememberWebhookPatchesLocked([]github.WebhookItemSnapshot{
		{Repo: "acme/console", Number: 1, IsPR: true, State: "open"},
	}, now.Add(30*time.Second))
	if e := s.webhookPatches[github.ItemPatchKey("acme/console", 1, true)]; e.snap.State != "open" || len(s.webhookPatches) != 1 {
		t.Fatalf("newest read must replace: %+v (n=%d)", e, len(s.webhookPatches))
	}
	s.pruneWebhookPatchesLocked(now.Add(91 * time.Second))
	if len(s.webhookPatches) != 0 {
		t.Fatalf("entry past the window must expire, have %d", len(s.webhookPatches))
	}

	snaps := make([]github.WebhookItemSnapshot, 0, maxWebhookPatchEntries+5)
	for i := 1; i <= maxWebhookPatchEntries+5; i++ {
		snaps = append(snaps, github.WebhookItemSnapshot{Repo: "acme/console", Number: i, IsPR: true, State: "closed"})
	}
	s.rememberWebhookPatchesLocked(snaps[:5], now)
	s.rememberWebhookPatchesLocked(snaps[5:], now.Add(time.Second))
	if len(s.webhookPatches) != maxWebhookPatchEntries {
		t.Fatalf("cap: %d entries, want %d", len(s.webhookPatches), maxWebhookPatchEntries)
	}
	for i := 1; i <= 5; i++ {
		if _, ok := s.webhookPatches[github.ItemPatchKey("acme/console", i, true)]; ok {
			t.Fatalf("oldest entry #%d must be evicted first", i)
		}
	}
	status := &StatusPayload{Repos: []FrontendRepo{{Name: "console", Full: "acme/console"}}}
	if n := s.replayWebhookPatchesLocked(status, now.Add(time.Second)); n != 0 {
		t.Fatalf("replay onto a repo that lists none of them changes %d, want 0", n)
	}
}

func TestStageTrackerWebhookRefreshReplayWindowConfig(t *testing.T) {
	var nilCfg *config.Config
	if nilCfg.StageTrackerWebhookRefreshReplayWindow() != 30*time.Minute {
		t.Fatalf("nil config default = %s", nilCfg.StageTrackerWebhookRefreshReplayWindow())
	}
	cfg := &config.Config{}
	if cfg.StageTrackerWebhookRefreshReplayWindow() != 30*time.Minute {
		t.Fatalf("zero config default = %s", cfg.StageTrackerWebhookRefreshReplayWindow())
	}
	cfg.Dashboard.StageTracker.WebhookRefreshReplayWindowS = 90
	if cfg.StageTrackerWebhookRefreshReplayWindow() != 90*time.Second {
		t.Fatal("explicit window must win")
	}
	cfg.Dashboard.StageTracker.WebhookRefreshReplayWindowS = -1
	if cfg.StageTrackerWebhookRefreshReplayWindow() != 30*time.Minute {
		t.Fatal("negative window falls back to the default")
	}
}

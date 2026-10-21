package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
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
	if armed := r.enqueue([]trackerRefreshKey{key}); !armed {
		t.Fatal("first key must arm the debounce timer")
	}
	for i := 0; i < 11; i++ {
		if armed := r.enqueue([]trackerRefreshKey{key}); armed {
			t.Fatal("a burst inside the window must not re-arm")
		}
	}
	r.enqueue([]trackerRefreshKey{{repo: "acme/console", number: 7, pr: false}, {repo: "acme/console", number: 99, pr: true}})
	r.enqueue([]trackerRefreshKey{{repo: "", number: 5}, {repo: "acme/console", number: 0}})
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

	if armed := r.enqueue([]trackerRefreshKey{key}); !armed || timers.count() != 2 {
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
	if r.enqueue([]trackerRefreshKey{{repo: "a", number: 1}}) {
		t.Fatal("nil refresher must be a no-op")
	}
	r = newWebhookTrackerRefresher()
	r.newTimer = (&fakeTimers{}).newTimer
	if r.enqueue(nil) {
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

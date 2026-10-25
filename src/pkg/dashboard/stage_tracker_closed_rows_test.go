package dashboard

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The two issues of hivecommons/hive#14159 / #14064 as GitHub reported them
// on 2026-10-25: both closed at 11:56 by a sweep (one not_planned, one
// completed), both touched again at 12:00 by the stage-label sweep's
// unlabel, both still "open" on the tracker at 12:19.
var (
	closedRowsBase     = time.Date(2026, 10, 25, 11, 40, 0, 0, time.UTC)
	closedRows14159At  = time.Date(2026, 10, 25, 11, 56, 36, 0, time.UTC)
	closedRows14064At  = time.Date(2026, 10, 25, 11, 56, 37, 0, time.UTC)
	unlabel14159At     = time.Date(2026, 10, 25, 12, 0, 41, 0, time.UTC)
	unlabel14064At     = time.Date(2026, 10, 25, 12, 0, 57, 0, time.UTC)
	closedRowsViewedAt = time.Date(2026, 10, 25, 12, 19, 0, 0, time.UTC)
)

// closedRowsFixture lists #14159 (actionable list, "Triage needed") and
// #14064 (sidelined work list, "Needs human decision") open, as the
// enumeration before the close left them.
func closedRowsFixture(t *testing.T) *Server {
	t.Helper()
	s := newTestServer()
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive", Issues: 2,
		ActionableIssues: []any{
			github.Issue{Repo: "hivecommons/hive", Number: 14159, Title: "✨ feature: evaluate Microsoft NVX as a per-task sandbox runtime", Author: "clubanderson", AuthorIsHuman: true,
				State: "open", Labels: []string{"help wanted", "needs-triage", "stage/015-triage-needed"}, UpdatedAt: closedRowsBase},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Repo: "hivecommons/hive", Number: 14064, Title: "[DCO] post-merge trailer failures on v5", Author: "clubanderson",
				Labels: []string{"help wanted", "ci-dco", "stage/045-needs-decision", "needs-decision"}, UpdatedAt: closedRowsBase, Bucket: "needs-decision", Reason: "parked"},
		},
	}}
	s.status = status
	return s
}

func trackerLists(t *testing.T, s *Server, number int) bool {
	t.Helper()
	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	for _, it := range s.buildStageTracker(cur, s.stageStore, closedRowsViewedAt).Items {
		if it.Kind == "issue" && it.Number == number {
			return true
		}
	}
	return false
}

func postIssuesWebhook(t *testing.T, s *Server, secret, event, delivery string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, githubWebhookPath, bytes.NewReader([]byte(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForTest(secret, []byte(body)))
	req.Header.Set("X-GitHub-Delivery", delivery)
	rec := httptest.NewRecorder()
	s.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", event, delivery, rec.Code, rec.Body.String())
	}
	return rec
}

// Both rows must leave on the first delivery that says the item is closed,
// whatever the action — here the stage-label sweep's `unlabeled`, delivered
// after the close with the issue object reading state=closed — even though
// the confirming GET is refused (GitHub 503, as a rate-limited hive sees).
// Before the fix the refresher dropped a failed read on the floor
// (webhook_tracker_refresh.go flush: `if err != nil { continue }`) and the
// payload's own state was never read, so the row stayed until the next
// successful enumeration.
func TestClosedIssueLeavesTrackerOnAnyDeliveryWhenConfirmingReadFails(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return closedRowsViewedAt }))
	s := closedRowsFixture(t)
	var gets atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(ghSrv.Close)
	var mu sync.Mutex
	var handed []github.WebhookItemSnapshot
	s.deps = &Dependencies{
		GHClient: github.NewClientForTest(ghSrv.URL, "hivecommons", []string{"hive"}, s.logger),
		Ctx:      context.Background(), Logger: s.logger, Config: &config.Config{},
		ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) { mu.Lock(); handed = append(handed, snaps...); mu.Unlock() },
	}
	timers := &fakeTimers{}
	s.webhookRefresh = newWebhookTrackerRefresher()
	s.webhookRefresh.newTimer = timers.newTimer
	if !trackerLists(t, s, 14159) || !trackerLists(t, s, 14064) {
		t.Fatal("fixture must list both rows open")
	}

	unlabeled14159 := `{"action":"unlabeled","label":{"name":"stage/015-triage-needed"},` +
		`"issue":{"number":14159,"state":"closed","state_reason":"not_planned","closed_at":"2026-10-25T11:56:36Z","updated_at":"2026-10-25T12:00:41Z",` +
		`"labels":[{"name":"help wanted"},{"name":"needs-triage"}],"title":"✨ feature: evaluate Microsoft NVX as a per-task sandbox runtime"},` +
		`"repository":{"full_name":"hivecommons/hive"}}`
	rec := postIssuesWebhook(t, s, secret, "issues", "d-14159-unlabeled", unlabeled14159)
	if !strings.Contains(rec.Body.String(), `"tracker_refresh":1`) {
		t.Fatalf("unlabeled not queued for the confirming read: %s", rec.Body.String())
	}
	if trackerLists(t, s, 14159) {
		t.Fatal("#14159 (closed, not_planned) still listed after an `unlabeled` delivery whose issue says closed")
	}
	if !trackerLists(t, s, 14064) {
		t.Fatal("#14064 must be untouched by #14159's delivery")
	}

	unlabeled14064 := `{"action":"unlabeled","label":{"name":"stage/045-needs-decision"},` +
		`"issue":{"number":14064,"state":"closed","state_reason":"completed","closed_at":"2026-10-25T11:56:37Z","updated_at":"2026-10-25T12:00:57Z",` +
		`"labels":[{"name":"help wanted"},{"name":"ci-dco"},{"name":"hive/already-done"},{"name":"hive: reporter-confirmed"}],"title":"[DCO] post-merge trailer failures on v5"},` +
		`"repository":{"full_name":"hivecommons/hive"}}`
	postIssuesWebhook(t, s, secret, "issues", "d-14064-unlabeled", unlabeled14064)
	if trackerLists(t, s, 14064) {
		t.Fatal("#14064 (closed, completed, sidelined list) still listed after an `unlabeled` delivery whose issue says closed")
	}
	s.statusMu.RLock()
	if s.status.Repos[0].Issues != 0 {
		t.Fatalf("repo issue count = %d after both evictions, want 0", s.status.Repos[0].Issues)
	}
	s.statusMu.RUnlock()

	mu.Lock()
	if len(handed) != 2 || handed[0].Number != 14159 || handed[0].Open() || handed[0].DeliveryID != "d-14159-unlabeled" ||
		!handed[0].UpdatedAt.Equal(unlabel14159At) || handed[1].Number != 14064 || handed[1].Open() || !handed[1].UpdatedAt.Equal(unlabel14064At) {
		t.Fatalf("governor handed %+v, want both closed evictions tagged with their deliveries", handed)
	}
	mu.Unlock()

	// The confirming reads are refused; the rows stay gone.
	if gets.Load() != 0 {
		t.Fatal("GET before the debounce window closed")
	}
	timers.last().fire()
	if gets.Load() != 2 {
		t.Fatalf("GETs = %d, want one refused read per item", gets.Load())
	}
	if trackerLists(t, s, 14159) || trackerLists(t, s, 14064) {
		t.Fatal("a refused confirming read must not bring a closed row back")
	}

	// A governor cycle that enumerated before the close publishes now: the
	// remembered evictions are replayed over it.
	stale := closedRowsFixture(t).status
	s.UpdateStatus(stale)
	if trackerLists(t, s, 14159) || trackerLists(t, s, 14064) {
		t.Fatal("a stale publish resurrected a closed row")
	}

	// A later comment on the closed issue (payload still says closed) is
	// idempotent, and a payload for an unlisted item changes nothing.
	commented := `{"action":"created","issue":{"number":14159,"state":"closed","updated_at":"2026-10-25T12:05:00Z"},"comment":{"body":"x"},"repository":{"full_name":"hivecommons/hive"}}`
	postIssuesWebhook(t, s, secret, "issue_comment", "d-14159-comment", commented)
	if trackerLists(t, s, 14159) {
		t.Fatal("comment on closed issue relisted it")
	}
}

// Deliveries out of order: the stage-label sweep's `unlabeled` read (older
// snapshot of the item, still open) lands after the `closed` one. The older
// open read must neither re-list the row nor overwrite the remembered close
// that protects the next stale publish; a genuinely newer open read (a
// reopen) does both.
func TestLabeledReadAfterCloseDoesNotResurrectRow(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return closedRowsViewedAt }))
	for _, tc := range []struct {
		name   string
		number int
	}{{"14159 not_planned actionable list", 14159}, {"14064 completed sidelined list", 14064}} {
		t.Run(tc.name, func(t *testing.T) {
			s := closedRowsFixture(t)
			closedAt := closedRows14159At
			if tc.number == 14064 {
				closedAt = closedRows14064At
			}
			s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "hivecommons/hive", Number: tc.number, State: "closed", UpdatedAt: closedAt}})
			if trackerLists(t, s, tc.number) {
				t.Fatal("close did not evict")
			}
			// The out-of-order read: open, from before the close.
			stale := github.WebhookItemSnapshot{Repo: "hivecommons/hive", Number: tc.number, State: "open", Title: "stale", Labels: []string{"needs-triage"}, UpdatedAt: closedAt.Add(-3 * time.Minute)}
			if n := s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{stale}); n != 0 {
				t.Fatalf("stale open read changed %d entries", n)
			}
			if trackerLists(t, s, tc.number) {
				t.Fatal("stale open read re-listed the closed row")
			}
			s.UpdateStatus(closedRowsFixture(t).status)
			if trackerLists(t, s, tc.number) {
				t.Fatal("stale open read un-remembered the close: the next publish resurrected the row")
			}
			// An older read is also rejected while the row is still listed.
			fresh := closedRowsFixture(t)
			fresh.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "hivecommons/hive", Number: tc.number, State: "open", Title: "older", UpdatedAt: closedRowsBase.Add(-time.Hour)}})
			for _, it := range fresh.buildStageTracker(fresh.status, nil, closedRowsViewedAt).Items {
				if it.Number == tc.number && it.Title == "older" {
					t.Fatal("an open read older than the listed copy rewrote the row")
				}
			}

			// A reopen: open and newer than the close. The remembered entry
			// follows it, so the next enumeration's listing survives replay.
			reopened := github.WebhookItemSnapshot{Repo: "hivecommons/hive", Number: tc.number, State: "open", Title: "reopened", UpdatedAt: closedAt.Add(10 * time.Minute)}
			s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{reopened})
			relisted := closedRowsFixture(t).status
			s.UpdateStatus(relisted)
			if !trackerLists(t, s, tc.number) {
				t.Fatal("after a newer open read the relisted row must survive the replay")
			}
			// And a close that arrives after that evicts again, whatever
			// the timestamp order.
			s.applyWebhookItemSnapshots([]github.WebhookItemSnapshot{{Repo: "hivecommons/hive", Number: tc.number, State: "closed", UpdatedAt: closedAt}})
			if trackerLists(t, s, tc.number) {
				t.Fatal("a closed read must always evict")
			}
		})
	}
}

// A `closed` delivery whose payload says closed evicts even with the
// targeted refresh disabled: the eviction costs no read.
func TestClosedDeliveryEvictsWithWebhookRefreshDisabled(t *testing.T) {
	const secret = "s3cr3t"
	t.Setenv(githubWebhookSecretEnvVar, secret)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return closedRowsViewedAt }))
	s := closedRowsFixture(t)
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ghSrv.Close)
	off := false
	cfg := &config.Config{}
	cfg.Dashboard.StageTracker.WebhookRefresh = &off
	s.deps = &Dependencies{GHClient: github.NewClientForTest(ghSrv.URL, "hivecommons", []string{"hive"}, s.logger), Ctx: context.Background(), Logger: s.logger, Config: cfg}
	closed := `{"action":"closed","issue":{"number":14159,"state":"closed","state_reason":"not_planned","updated_at":"2026-10-25T11:56:36Z"},"repository":{"full_name":"hivecommons/hive"}}`
	rec := postIssuesWebhook(t, s, secret, "issues", "d-closed", closed)
	if strings.Contains(rec.Body.String(), "tracker_refresh") {
		t.Fatalf("refresh disabled must schedule no read: %s", rec.Body.String())
	}
	if trackerLists(t, s, 14159) {
		t.Fatal("closed delivery did not evict with webhook_refresh=false")
	}
	// An open payload never adds or rewrites anything.
	open := `{"action":"labeled","issue":{"number":14064,"state":"open","title":"PAYLOAD LIES","updated_at":"2026-10-25T12:30:00Z"},"repository":{"full_name":"hivecommons/hive"}}`
	postIssuesWebhook(t, s, secret, "issues", "d-open", open)
	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	if it := trackerItem(t, s.buildStageTracker(cur, nil, closedRowsViewedAt), "issue", 14064); strings.Contains(it.Title, "PAYLOAD") {
		t.Fatalf("payload fields applied to an open row: %+v", it)
	}
}

// The backstop sweep: with no delivery at all, the next tracker view lists
// what GitHub closed since the last look and evicts the rows still shown.
func TestClosedSweepEvictsRowsGitHubReportsClosed(t *testing.T) {
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return closedRowsViewedAt }))
	s := closedRowsFixture(t)
	var mu sync.Mutex
	var handed []github.WebhookItemSnapshot
	var sinces []time.Time
	s.deps = &Dependencies{Ctx: context.Background(), Logger: s.logger, Config: &config.Config{},
		ApplyWebhookItems: func(snaps []github.WebhookItemSnapshot) { mu.Lock(); handed = append(handed, snaps...); mu.Unlock() }}
	fail := false
	s.closedSweep.list = func(_ context.Context, repo string, since time.Time) ([]github.WebhookItemSnapshot, error) {
		mu.Lock()
		sinces = append(sinces, since)
		mu.Unlock()
		if repo != "hivecommons/hive" {
			t.Errorf("sweep listed %q", repo)
		}
		if fail {
			return nil, errors.New("503")
		}
		return []github.WebhookItemSnapshot{
			{Repo: repo, Number: 14159, State: "closed", UpdatedAt: unlabel14159At},
			{Repo: repo, Number: 14064, State: "closed", UpdatedAt: unlabel14064At},
			// Closed items the tracker never listed are not patched in.
			{Repo: repo, Number: 14000, State: "closed", UpdatedAt: unlabel14064At},
			{Repo: repo, Number: 13999, IsPR: true, State: "closed", Merged: true, UpdatedAt: unlabel14064At},
		}, nil
	}
	// Synchronous run.
	evicted, err := s.sweepClosedRows(context.Background(), closedRowsViewedAt)
	if err != nil || evicted != 2 {
		t.Fatalf("sweep evicted %d (%v), want the two listed rows", evicted, err)
	}
	if trackerLists(t, s, 14159) || trackerLists(t, s, 14064) {
		t.Fatal("sweep left a closed row listed")
	}
	mu.Lock()
	if len(handed) != 2 || len(sinces) != 1 || !sinces[0].Equal(closedRowsViewedAt.Add(-closedSweepFirstLookback)) {
		t.Fatalf("handed=%d sinces=%v, want two evictions from one list since the first lookback", len(handed), sinces)
	}
	mu.Unlock()
	// The next sweep lists from the previous one (minus the overlap); a
	// failed read keeps `since` where it was so nothing is skipped.
	fail = true
	later := closedRowsViewedAt.Add(5 * time.Minute)
	if _, err := s.sweepClosedRows(context.Background(), later); err == nil {
		t.Fatal("list failure must surface")
	}
	fail = false
	if _, err := s.sweepClosedRows(context.Background(), later.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	want := closedRowsViewedAt.Add(-closedSweepOverlap)
	if len(sinces) != 3 || !sinces[1].Equal(want) || !sinces[2].Equal(want) {
		t.Fatalf("sinces = %v, want the retry and the next sweep both from the last successful sweep minus the overlap (%v)", sinces, want)
	}
	mu.Unlock()
}

// maybeSweepClosedRows: throttled to the interval, off when disabled, off
// without a GitHub client, one at a time.
func TestClosedSweepThrottleAndConfig(t *testing.T) {
	now := closedRowsViewedAt
	s := closedRowsFixture(t)
	if s.maybeSweepClosedRows(now) {
		t.Fatal("no deps: nothing to sweep with")
	}
	cfg := &config.Config{}
	s.deps = &Dependencies{Ctx: context.Background(), Logger: s.logger, Config: cfg}
	if s.maybeSweepClosedRows(now) {
		t.Fatal("no GitHub client and no injected list: must not start")
	}
	var calls atomic.Int32
	release := make(chan struct{})
	s.closedSweep.list = func(ctx context.Context, repo string, since time.Time) ([]github.WebhookItemSnapshot, error) {
		calls.Add(1)
		<-release
		return nil, nil
	}
	cfg.Dashboard.StageTracker.ClosedSweepIntervalS = -1
	if s.maybeSweepClosedRows(now) {
		t.Fatal("a negative interval disables the sweep")
	}
	cfg.Dashboard.StageTracker.ClosedSweepIntervalS = 0
	if (*config.Config)(nil).StageTrackerClosedSweepInterval() != 5*time.Minute || cfg.StageTrackerClosedSweepInterval() != 5*time.Minute {
		t.Fatal("default closed sweep interval is 5m")
	}
	cfg.Dashboard.StageTracker.ClosedSweepIntervalS = 120
	if cfg.StageTrackerClosedSweepInterval() != 2*time.Minute {
		t.Fatal("configured interval not honoured")
	}
	if !s.maybeSweepClosedRows(now) {
		t.Fatal("first view must start a sweep")
	}
	if s.maybeSweepClosedRows(now.Add(time.Second)) {
		t.Fatal("a running sweep must not start another")
	}
	close(release)
	s.closedSweep.mu.Lock()
	done := s.closedSweep.done
	s.closedSweep.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep did not finish")
	}
	if calls.Load() != 1 {
		t.Fatalf("list calls = %d, want 1", calls.Load())
	}
	if s.maybeSweepClosedRows(now.Add(time.Minute)) {
		t.Fatal("inside the interval: no sweep")
	}
	if !s.maybeSweepClosedRows(now.Add(3 * time.Minute)) {
		t.Fatal("past the interval: sweep due")
	}
}

// GET /api/stage-tracker kicks the sweep.
// TestStatusPublishStartsClosedSweep: the sweep is driven by the governor's
// status publish, not by GET /api/stage-tracker, which must never reach
// GitHub (TestStageTrackerCacheServesWithoutGitHub).
func TestStatusPublishStartsClosedSweep(t *testing.T) {
	s := closedRowsFixture(t)
	started := make(chan struct{}, 1)
	s.deps = &Dependencies{Ctx: context.Background(), Logger: s.logger, Config: &config.Config{}}
	s.closedSweep.list = func(context.Context, string, time.Time) ([]github.WebhookItemSnapshot, error) {
		started <- struct{}{}
		return nil, nil
	}
	rec := httptest.NewRecorder()
	s.handleStageTracker(rec, httptest.NewRequest(http.MethodGet, "/api/stage-tracker", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stage tracker: %d", rec.Code)
	}
	select {
	case <-started:
		t.Fatal("GET /api/stage-tracker must not start the closed sweep")
	case <-time.After(50 * time.Millisecond):
	}
	// The governor publishes a fresh enumeration that still lists the repo.
	next := minimalPayload()
	next.Repos = append([]FrontendRepo(nil), closedRowsFixture(t).status.Repos...)
	s.UpdateStatus(next)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("status publish did not start the closed sweep")
	}
}

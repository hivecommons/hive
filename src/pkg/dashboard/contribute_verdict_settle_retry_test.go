package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The unverified already-done cleanup is retried on a later pass when GitHub
// fails it (contribute_already_done_cleanup.go). A contributor relay put
// hive/already-done on o/r#7 with the task credential, the verdict did not
// verify, and markAlreadyDoneUnverified tried to take the label off. That one
// DELETE failed (here a 502, live a pacing deadline), and before the retry
// nothing ever tried again: the label withholds the issue from offers and
// kicks, so no second verdict came to retry it.
//
// These tests drive a real ghpkg.Client against settleGHFixture through a
// proxy that fails chosen requests, then run the retry pass the hub's
// unverifiedAlreadyDoneCleanupLoop runs.

// retryFlake answers the nth label DELETE and the nth comment POST on o/r#7
// with deleteStatuses[n-1] and commentStatuses[n-1] (a 502 by default in the
// tests); every other request, and every one past the list, reaches the
// fixture.
type retryFlake struct {
	deletes         atomic.Int32
	comments        atomic.Int32
	deleteStatuses  []int
	commentStatuses []int
}

// statusFor returns the failure status for the nth request, or 0 to pass it.
func statusFor(statuses []int, n int32) int {
	if n < 1 || int(n) > len(statuses) {
		return 0
	}
	return statuses[n-1]
}

// settleRetryHub wires a covK2Hub to a GHClient that reaches fx through the
// flaky proxy, with the hive App as the hive identity.
func settleRetryHub(t *testing.T, fx *settleGHFixture, flake *retryFlake) (*ContributeWSHub, *Server) {
	t.Helper()
	backend := fx.server(t)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := 0
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/7/labels/"):
			status = statusFor(flake.deleteStatuses, flake.deletes.Add(1))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			status = statusFor(flake.commentStatuses, flake.comments.Add(1))
		}
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Server Error"}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	hub, s := covK2Hub(t)
	s.deps.GHClient = ghpkg.NewClientForTest(front.URL, "o", []string{"r"}, hub.logger)
	s.deps.GHClient.SetAppBotLogin("hive-app[bot]")
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	hub.issuePRClaimMarker = nil
	hub.alreadyDoneMarker = nil
	return hub, s
}

// relayLabeled is the labeled event the relay's task credential leaves.
func relayLabeled(actor string, at time.Time) map[string]any {
	return map[string]any{
		"id": 1, "event": "labeled", "created_at": at.UTC().Format(time.RFC3339),
		"actor": map[string]any{"login": actor},
		"label": map[string]any{"name": "hive/already-done"},
	}
}

func (f *settleGHFixture) hasLabel(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.issueLabels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func pendingAlreadyDoneCleanup(hub *ContributeWSHub) (unverifiedAlreadyDoneCleanup, bool) {
	hub.completedMu.Lock()
	defer hub.completedMu.Unlock()
	rec, ok := hub.unverifiedAlreadyDoneCleanups["o/r#7"]
	return rec, ok
}

// settleUnverifiedWithFailure runs the one settle that books the cleanup and
// checks the first pass left the label on and the cleanup pending.
func settleUnverifiedWithFailure(t *testing.T, hub *ContributeWSHub, fx *settleGHFixture, dispatched time.Time) unverifiedAlreadyDoneCleanup {
	t.Helper()
	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "already resolved upstream", verdictReasonKindAlreadyDone, nil, dispatched, "ct")
	if got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneUnverified)
	}
	if !fx.hasLabel("hive/already-done") {
		t.Fatal("precondition: the failed DELETE must leave the relay's label on")
	}
	rec, ok := pendingAlreadyDoneCleanup(hub)
	if !ok || !rec.Label {
		t.Fatalf("precondition: a failed removal must book a pending cleanup, got ok=%v rec=%+v", ok, rec)
	}
	return rec
}

// Removal and comment both fail once, then GitHub recovers: the later pass
// takes the hive-added label off, posts the comment, drops the cleanup and
// keeps the unverified hold.
func TestAlreadyDoneVerdict_UnverifiedLabelRemovalRetriesAfterFailure(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}, commentStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)

	rec := settleUnverifiedWithFailure(t, hub, fx, dispatched)
	if !rec.Comment || rec.Reporter != "ct" || rec.HoldHours != float64(30*24) || !rec.DispatchedAt.Equal(dispatched) {
		t.Fatalf("pending cleanup = %+v, want the comment pending with the reporter, hold and dispatch time", rec)
	}
	fx.mu.Lock()
	posted := len(fx.comments)
	fx.mu.Unlock()
	if posted != 0 {
		t.Fatalf("precondition: the failed POST must leave no comment, got %d", posted)
	}

	// GitHub stamps the retry's own writes as issue activity at about this
	// time, after the hold was first booked.
	retryAt := time.Now()
	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 1 {
		t.Fatalf("sweep finished %d cleanups, want 1", got)
	}
	if fx.hasLabel("hive/already-done") {
		t.Fatalf("unverified hive/already-done is still on o/r#7 after the retry (DELETE attempts = %d)", flake.deletes.Load())
	}
	fx.mu.Lock()
	comments := append([]string(nil), fx.comments...)
	fx.mu.Unlock()
	if len(comments) != 1 || !strings.Contains(comments[0], "not labelled `hive/already-done`") {
		t.Fatalf("comments = %v, want the unverified comment posted once by the retry", comments)
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("a finished cleanup must be dropped")
	}
	hub.completedMu.Lock()
	hold, ok := hub.noWorkVerdicts["o/r#7"]
	hub.completedMu.Unlock()
	if !ok || !hold.AlreadyDoneUnverified {
		t.Fatalf("the unverified hold must survive the retry's own writes, got ok=%v rec=%+v", ok, hold)
	}
	if !hub.isSuppressedByNoWorkVerdict("o/r", 7, retryAt) {
		t.Fatal("the retry's own label removal and comment must not lift the unverified hold")
	}

	// A further pass has nothing to do and calls nothing.
	deletes := flake.deletes.Load()
	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 0 || flake.deletes.Load() != deletes {
		t.Fatalf("an empty ledger must cost nothing: finished %d, DELETE attempts %d -> %d", got, deletes, flake.deletes.Load())
	}
}

// A person who re-adds the label after the hive's failed removal keeps it:
// the retry never takes off a label a person added, and the cleanup ends.
func TestAlreadyDoneVerdict_UnverifiedRetryKeepsALabelAPersonAdded(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)

	fx.mu.Lock()
	fx.issueEvents = append(fx.issueEvents, relayLabeled("maintainer", dispatched.Add(2*time.Minute)))
	fx.mu.Unlock()

	hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now())
	if !fx.hasLabel("hive/already-done") {
		t.Fatal("the retry removed a label a person added")
	}
	if n := flake.deletes.Load(); n != 1 {
		t.Fatalf("DELETE attempts = %d, want only the first failed one", n)
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("the cleanup must end once a person owns the label")
	}
}

// A verified merged claim that now covers the issue keeps the label: the
// cleanup is dropped and nothing is removed.
func TestAlreadyDoneVerdict_UnverifiedRetryLeavesAVerifiedClaimsLabel(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}}
	hub, s := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)

	s.deps.IssueClaimed = func(repo string, number int) (ghpkg.IssueClaim, bool) {
		if repo == "o/r" && number == 7 {
			return ghpkg.IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 41, MergedPR: true, MergedAt: time.Now().Add(-time.Hour)}, true
		}
		return ghpkg.IssueClaim{}, false
	}
	hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now())
	if !fx.hasLabel("hive/already-done") {
		t.Fatal("the retry removed the label a verified merged claim stands behind")
	}
	fx.mu.Lock()
	removed := append([]string(nil), fx.labelsRemoved...)
	fx.mu.Unlock()
	if len(removed) != 0 || flake.deletes.Load() != 1 {
		t.Fatalf("labelsRemoved = %v, DELETE attempts = %d; want nothing removed after the first failure", removed, flake.deletes.Load())
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("the cleanup must be dropped once a verified claim covers the issue")
	}
}

// Once the label is gone (a person took it off by hand) the cleanup ends
// without another DELETE.
func TestAlreadyDoneVerdict_UnverifiedRetryEndsWhenTheLabelIsGone(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)

	fx.mu.Lock()
	fx.issueLabels = []string{"kind/bug"}
	fx.mu.Unlock()

	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 1 {
		t.Fatalf("sweep finished %d cleanups, want 1", got)
	}
	if n := flake.deletes.Load(); n != 1 {
		t.Fatalf("DELETE attempts = %d, want only the first failed one", n)
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("the cleanup must be dropped once the label is gone")
	}
}

// A cleanup that keeps failing is given up after its maximum age, without
// another GitHub call.
func TestAlreadyDoneVerdict_UnverifiedRetryGivesUpAfterMaxAge(t *testing.T) {
	fx := &settleGHFixture{issueLabels: []string{"hive/already-done"}}
	flake := &retryFlake{}
	hub, _ := settleRetryHub(t, fx, flake)
	hub.queueUnverifiedAlreadyDoneCleanup(unverifiedAlreadyDoneCleanup{
		Repo: "o/r", Number: 7, DispatchedAt: time.Now().Add(-26 * time.Hour), Label: true,
		RecordedAt: time.Now().Add(-unverifiedAlreadyDoneCleanupMaxAge - time.Minute),
	})

	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 0 {
		t.Fatalf("sweep finished %d cleanups, want 0 (given up, not finished)", got)
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("an expired cleanup must be dropped")
	}
	if !fx.hasLabel("hive/already-done") || flake.deletes.Load() != 0 {
		t.Fatalf("an expired cleanup must not call GitHub (DELETE attempts = %d)", flake.deletes.Load())
	}
}

// The pending cleanup is persisted, so a hub restart still retries it.
func TestAlreadyDoneVerdict_UnverifiedCleanupSurvivesRestart(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)
	hub.persistTaskLedgers = true
	want := settleUnverifiedWithFailure(t, hub, fx, dispatched)

	path := hub.unverifiedAlreadyDoneCleanupsPath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the cleanup ledger must be written beside the no-PR streaks: %v", err)
	}
	hub2 := &ContributeWSHub{logger: covBLogger(), persistTaskLedgers: true, unverifiedAlreadyDoneCleanupsFile: path}
	hub2.loadUnverifiedAlreadyDoneCleanups()
	got, ok := pendingAlreadyDoneCleanup(hub2)
	if !ok || got.Repo != "o/r" || got.Number != 7 || !got.Label || !got.DispatchedAt.Equal(want.DispatchedAt) {
		t.Fatalf("reloaded cleanup = %+v (ok=%v), want %+v", got, ok, want)
	}
}

// A person who touches the issue between the failed first pass and the retry
// keeps the right to lift the hold: the retry still finishes, but does not
// move the hold past that activity.
func TestAlreadyDoneVerdict_UnverifiedRetryKeepsAPersonsActivityLiftingTheHold(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}, commentStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)

	hub.completedMu.Lock()
	booked := hub.noWorkVerdicts["o/r#7"].RecordedAt
	hub.completedMu.Unlock()
	// A maintainer comments "work remains" after the hold was booked.
	personAt := booked.Add(time.Second)
	fx.mu.Lock()
	fx.issueUpdatedAt = personAt
	fx.mu.Unlock()

	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 1 {
		t.Fatalf("sweep finished %d cleanups, want 1", got)
	}
	if fx.hasLabel("hive/already-done") {
		t.Fatal("the retry must still take the hive-added label off")
	}
	hub.completedMu.Lock()
	hold, ok := hub.noWorkVerdicts["o/r#7"]
	hub.completedMu.Unlock()
	if !ok || !hold.RecordedAt.Equal(booked) {
		t.Fatalf("hold = %+v (ok=%v), want it left at %s: the person's activity must still lift it", hold, ok, booked)
	}
	fx.mu.Lock()
	comments := append([]string(nil), fx.comments...)
	fx.mu.Unlock()
	if len(comments) != 0 || flake.comments.Load() != 1 {
		t.Fatalf("comments = %v (POST attempts %d); the comment must not announce a hold the person's activity lifts", comments, flake.comments.Load())
	}
	if hub.isSuppressedByNoWorkVerdict("o/r", 7, personAt) {
		t.Fatal("the person's activity after the hold must lift it")
	}
}

// When the hold the comment announces is gone (here a later completion with a
// PR cleared it), the pending comment is not posted, but the stuck label
// still comes off.
func TestAlreadyDoneVerdict_UnverifiedRetryDropsTheCommentWhenTheHoldIsGone(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway}, commentStatuses: []int{http.StatusBadGateway}}
	hub, _ := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)
	hub.clearNoWorkVerdict("o/r", 7)

	if got := hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now()); got != 1 {
		t.Fatalf("sweep finished %d cleanups, want 1", got)
	}
	if fx.hasLabel("hive/already-done") {
		t.Fatal("the stuck hive-added label must still come off")
	}
	fx.mu.Lock()
	comments := append([]string(nil), fx.comments...)
	fx.mu.Unlock()
	if len(comments) != 0 || flake.comments.Load() != 1 {
		t.Fatalf("comments = %v (POST attempts %d); the comment must not be posted once its hold is gone", comments, flake.comments.Load())
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("the cleanup must be dropped")
	}
}

// GitHub refusing the removal for good ends that part at once rather than
// retrying it for a day.
func TestAlreadyDoneVerdict_UnverifiedRetryGivesUpOnARefusal(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	flake := &retryFlake{deleteStatuses: []int{http.StatusBadGateway, http.StatusForbidden}}
	hub, _ := settleRetryHub(t, fx, flake)
	settleUnverifiedWithFailure(t, hub, fx, dispatched)

	hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now())
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("a refused removal must not stay pending")
	}
	hub.sweepUnverifiedAlreadyDoneCleanups(context.Background(), time.Now())
	if n := flake.deletes.Load(); n != 2 {
		t.Fatalf("DELETE attempts = %d, want 2 (the 502, then the 403, then nothing)", n)
	}
}

// A removal GitHub refuses on the first pass already is not queued at all.
func TestAlreadyDoneVerdict_UnverifiedRefusalOnTheFirstPassIsNotQueued(t *testing.T) {
	dispatched := time.Now().Add(-3 * time.Minute)
	fx := &settleGHFixture{
		issueLabels: []string{"kind/bug", "hive/already-done"},
		issueEvents: []map[string]any{relayLabeled("hive-app[bot]", dispatched.Add(time.Minute))},
	}
	hub, _ := settleRetryHub(t, fx, &retryFlake{deleteStatuses: []int{http.StatusUnprocessableEntity}})
	if got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "already resolved upstream", verdictReasonKindAlreadyDone, nil, dispatched, "ct"); got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneUnverified)
	}
	if _, ok := pendingAlreadyDoneCleanup(hub); ok {
		t.Fatal("a removal refused on the first pass must not be queued")
	}
}

// The live pacing deadline, a 5xx and an error with no GitHub status are
// retried; 403, 404, 410 and 422 are refusals.
func TestUnverifiedAlreadyDoneRefused(t *testing.T) {
	status := func(code int) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: code, Request: &http.Request{Method: http.MethodDelete, URL: &url.URL{Path: "/x"}}}}
	}
	pacing := &url.Error{Op: "Delete", URL: "https://api.github.com/x",
		Err: fmt.Errorf("github pacing: slot would miss request deadline: %w", context.DeadlineExceeded)}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"pacing deadline", pacing, false},
		{"bad gateway", status(http.StatusBadGateway), false},
		{"no github status", errors.New("op is Planned"), false},
		{"forbidden", status(http.StatusForbidden), true},
		{"not found", fmt.Errorf("wrapped: %w", status(http.StatusNotFound)), true},
		{"gone", status(http.StatusGone), true},
		{"unprocessable", status(http.StatusUnprocessableEntity), true},
	} {
		if got := unverifiedAlreadyDoneRefused(tc.err); got != tc.want {
			t.Errorf("%s: refused = %v, want %v", tc.name, got, tc.want)
		}
	}
}

package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// An issue with an open sub-issue is a parent waiting on its children. A
// maintainer moved an approved design issue's remaining work into three
// native sub-issues; the parent had no tracker label, no tracker title and no
// child list in its body, so it was offered to a contributor, came back
// "already done", and was labelled done and sent to be closed with every
// child still open. These tests pin both halves: the parent is not offered,
// and a verdict on it writes nothing.

const parentKey = "acme/widgets#601"

// parentHub seeds acme/widgets with #601 (the candidate under test, carrying
// the given `children` block when it is non-nil) and, when withControl is
// set, #700 as ordinary ready work that must stay on offer.
func parentHub(t *testing.T, children map[string]any, withControl bool) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	parent := map[string]any{
		"number": float64(601),
		"title":  "widget pipeline design",
		"url":    "https://github.com/acme/widgets/issues/601",
		"author": "alice",
	}
	if children != nil {
		parent["children"] = children
	}
	issues := []any{parent}
	if withControl {
		issues = append(issues, map[string]any{
			"number": float64(700),
			"title":  "unrelated ready work",
			"url":    "https://github.com/acme/widgets/issues/700",
			"author": "alice",
		})
	}
	s.statusMu.Lock()
	s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "widgets", Full: "acme/widgets", ActionableIssues: issues}}}
	s.statusMu.Unlock()
	return hub, s
}

func TestParentWithOpenSubIssues_IsNotOfferedOnEitherLadder(t *testing.T) {
	hub, _ := parentHub(t, map[string]any{"total": float64(3), "completed": float64(0)}, true)

	// The queue projection leaves the parent out and keeps the control.
	assertQueue(t, hub, 700)
	// The assignment path hands out the control, never the parent.
	assertAssigns(t, hub, 700)

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	item, ok := findWithheld(snap.withheld, parentKey)
	if !ok {
		t.Fatalf("#601 is withheld but unexplained; collected %v", snap.withheld)
	}
	if item.Reason != withheldReasonTracker {
		t.Fatalf("reason = %q, want %q", item.Reason, withheldReasonTracker)
	}
	if !strings.Contains(item.Detail, "has 3 open sub-issues") {
		t.Fatalf("detail = %q, want it to say how many sub-issues are open", item.Detail)
	}
	if item.Title == "" || item.URL == "" {
		t.Error("a withheld row must carry enough metadata to be a link")
	}
	if _, present := findWithheld(snap.withheld, "acme/widgets#700"); present {
		t.Error("the offerable control must not appear in the withheld list")
	}
}

func TestParentWithClosedOrNoSubIssues_IsOfferedAsBefore(t *testing.T) {
	cases := []struct {
		name     string
		children map[string]any
	}{
		{name: "all sub-issues closed", children: map[string]any{"total": float64(3), "completed": float64(3)}},
		{name: "no sub-issues", children: nil},
		{name: "empty block", children: map[string]any{"total": float64(0), "completed": float64(0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub, _ := parentHub(t, tc.children, false)
			assertQueue(t, hub, 601)
			assertAssigns(t, hub, 601)
			snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
			if item, present := findWithheld(snap.withheld, parentKey); present {
				t.Fatalf("#601 must be on offer, but it is withheld: %+v", item)
			}
		})
	}
}

// One rule, read by both ladders: the enumerator's flag keeps its meaning and
// its plain Withheld text; open children are the added case.
func TestContributeTrackerHoldFor(t *testing.T) {
	open := map[string]any{"total": float64(2), "completed": float64(1)}
	cases := []struct {
		name       string
		issue      map[string]any
		held       bool
		openKids   int
		wantDetail string
	}{
		{name: "ordinary issue", issue: map[string]any{}},
		{name: "flagged tracker", issue: map[string]any{"is_tracker": true}, held: true, wantDetail: "Tracker / umbrella issue"},
		{name: "flagged tracker with open children keeps the flag's text", issue: map[string]any{"is_tracker": true, "children": open}, held: true, wantDetail: "Tracker / umbrella issue"},
		{name: "one open child", issue: map[string]any{"children": open}, held: true, openKids: 1, wantDetail: "Tracker / umbrella issue: has 1 open sub-issue"},
		{name: "all children closed", issue: map[string]any{"children": map[string]any{"total": float64(2), "completed": float64(2)}}},
		{name: "malformed block", issue: map[string]any{"children": "3"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hold := contributeTrackerHoldFor(tc.issue)
			if hold.held != tc.held || hold.openSubIssues != tc.openKids {
				t.Fatalf("hold = %+v, want held=%v openSubIssues=%d", hold, tc.held, tc.openKids)
			}
			if !tc.held {
				return
			}
			if got := withheldTrackerItem(withheldCandidate{repoFull: "acme/widgets"}, hold).Detail; got != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", got, tc.wantDetail)
			}
		})
	}
}

// parentSettle is the verdict-settle fixture: a verifier that accepts merged
// PR #41 for acme/widgets, plus counters on the three things the settle path
// can write (the claim, the likely-done label seam, the already-done seam).
type parentSettle struct {
	hub        *ContributeWSHub
	s          *Server
	fx         *settleFixture
	logs       *lockedBuffer
	mu         sync.Mutex
	claim      int
	done       int
	closeAsked bool
}

func newParentSettle(t *testing.T) *parentSettle {
	t.Helper()
	hub, s := covK2Hub(t)
	p := &parentSettle{hub: hub, s: s, fx: &settleFixture{}, logs: &lockedBuffer{}}
	hub.logger = slog.New(slog.NewTextHandler(p.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s.deps.RecordIssueClaim = p.fx.record
	hub.settleVerifier = p.fx.verifier(map[string]ghpkg.SettleVerification{
		"acme/widgets#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "acme/widgets", PRURL: "https://github.com/acme/widgets/pull/41",
			PRAuthor: "bob", MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
		}},
	}, nil)
	hub.issuePRClaimMarker = func(context.Context, string, int, ghpkg.IssueClaim) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.claim++
		return nil
	}
	hub.alreadyDoneMarker = func(_ context.Context, _ string, _ int, _ ghpkg.IssueClaim, _ string, closeIssue bool) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.done++
		p.closeAsked = closeIssue
		return nil
	}
	// The completion books this hold before the settle runs; a held parent
	// must keep it.
	hub.completedMu.Lock()
	hub.noWorkVerdicts["acme/widgets#7"] = noWorkVerdictRecord{RecordedAt: time.Now()}
	hub.completedMu.Unlock()
	return p
}

// row publishes acme/widgets#7 in the status with the given children.
func (p *parentSettle) row(children *ghpkg.IssueChildren) {
	p.s.statusMu.Lock()
	p.s.status = &StatusPayload{Repos: []FrontendRepo{{Name: "widgets", Full: "acme/widgets", ActionableIssues: []any{
		ghpkg.Issue{Repo: "acme/widgets", Number: 7, Title: "widget pipeline design", Children: children},
	}}}}
	p.s.statusMu.Unlock()
}

// github points the hub at a fake GitHub that serves only the issue read,
// answering with the given sub-issue summary (nil: none on the payload) or,
// when fail is set, a 500. Any other call fails the test.
func (p *parentSettle) github(t *testing.T, summary *ghpkg.IssueChildren, fail bool) *int {
	t.Helper()
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widgets/issues/7" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.mu.Lock()
		reads++
		p.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
			return
		}
		payload := map[string]any{"number": 7, "state": "open", "title": "widget pipeline design"}
		if summary != nil {
			payload["sub_issues_summary"] = summary
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	p.s.deps.GHClient = ghpkg.NewClientForTest(srv.URL, "acme", []string{"widgets"}, p.hub.logger)
	// No split-parents ledger on this machine can answer for the fixture.
	prev := ghpkg.SplitParentsPath
	ghpkg.SplitParentsPath = filepath.Join(t.TempDir(), "split-parents.json")
	t.Cleanup(func() { ghpkg.SplitParentsPath = prev })
	return &reads
}

func (p *parentSettle) settle() string {
	return p.hub.settleIssueFromVerdictWithEvidence("acme/widgets", 7,
		"merged PR #41 delivers the design; the remaining work is in the open sub-issues",
		verdictReasonKindAlreadyDone, &VerdictEvidence{PR: 41}, time.Now(), "alice")
}

// assertUntouched: no claim recorded, no label seam called, no comment or
// close requested, and the completion's offer hold still in force.
func (p *parentSettle) assertUntouched(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fx.recorded) != 0 {
		t.Errorf("a claim was recorded for a parent that is not done: %+v", p.fx.recorded)
	}
	if p.claim != 0 {
		t.Errorf("the likely-done label path ran %d time(s), want 0", p.claim)
	}
	if p.done != 0 {
		t.Errorf("the already-done comment/label/close path ran %d time(s) (close=%v), want 0", p.done, p.closeAsked)
	}
	p.hub.completedMu.Lock()
	_, held := p.hub.noWorkVerdicts["acme/widgets#7"]
	p.hub.completedMu.Unlock()
	if !held {
		t.Error("the no_work_needed offer hold was lifted; a held parent must keep it")
	}
}

// assertMarkedAsBefore: the claim, the likely-done label and the already-done
// comment/label/close all happen exactly as they did before the check.
func (p *parentSettle) assertMarkedAsBefore(t *testing.T, got string) {
	t.Helper()
	if got != verdictDispositionAlreadyDoneClosed {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneClosed)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fx.recorded) != 1 || p.fx.recorded[0].PRNumber != 41 || p.fx.recorded[0].Source != ghpkg.ClaimSourceVerdict {
		t.Fatalf("recorded claims = %+v, want the verified merged PR #41", p.fx.recorded)
	}
	if p.claim != 1 {
		t.Fatalf("the likely-done label path ran %d time(s), want 1", p.claim)
	}
	if p.done != 1 || !p.closeAsked {
		t.Fatalf("already-done path ran %d time(s) with close=%v, want once with a close", p.done, p.closeAsked)
	}
	p.hub.completedMu.Lock()
	_, held := p.hub.noWorkVerdicts["acme/widgets#7"]
	p.hub.completedMu.Unlock()
	if held {
		t.Fatal("a recorded PR claim replaces the no_work_needed hold, as before")
	}
}

func TestVerdictSettle_ParentWithOpenSubIssueIsNotMarkedDone(t *testing.T) {
	p := newParentSettle(t)
	p.row(&ghpkg.IssueChildren{Total: 3, Completed: 0})
	// A GitHub client is wired and must not be asked: the row already says.
	reads := p.github(t, nil, false)

	got := p.settle()

	if got != verdictDispositionAlreadyDoneOpenSubIssues {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneOpenSubIssues)
	}
	p.assertUntouched(t)
	if *reads != 0 {
		t.Fatalf("GitHub was read %d time(s); the enumeration row already showed an open child", *reads)
	}
	logged := p.logs.String()
	if n := strings.Count(logged, "parent has open sub-issues; not marking done"); n != 1 {
		t.Fatalf("want exactly one log line, got %d in:\n%s", n, logged)
	}
	for _, want := range []string{"repo=acme/widgets", "number=7", "open=3", "total=3"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line is missing %q:\n%s", want, logged)
		}
	}
}

// The enumeration row can be minutes old. When it shows no open child the
// children are re-read before anything is written.
func TestVerdictSettle_FreshReadFindsOpenSubIssue(t *testing.T) {
	for _, row := range []struct {
		name     string
		children *ghpkg.IssueChildren
		listed   bool
	}{
		{name: "row says no children", listed: true},
		{name: "row says all children closed", children: &ghpkg.IssueChildren{Total: 1, Completed: 1}, listed: true},
		{name: "issue is not in the status at all"},
	} {
		t.Run(row.name, func(t *testing.T) {
			p := newParentSettle(t)
			if row.listed {
				p.row(row.children)
			}
			reads := p.github(t, &ghpkg.IssueChildren{Total: 1, Completed: 0}, false)

			got := p.settle()

			if got != verdictDispositionAlreadyDoneOpenSubIssues {
				t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneOpenSubIssues)
			}
			p.assertUntouched(t)
			if *reads != 1 {
				t.Fatalf("GitHub issue reads = %d, want exactly 1", *reads)
			}
			if logged := p.logs.String(); strings.Count(logged, "parent has open sub-issues; not marking done") != 1 ||
				!strings.Contains(logged, "open=1") || !strings.Contains(logged, "total=1") {
				t.Fatalf("want one log line with open=1 total=1, got:\n%s", logged)
			}
		})
	}
}

// A re-read that fails decides nothing: no label, no comment, no close.
func TestVerdictSettle_UnreadableSubIssuesWritesNothing(t *testing.T) {
	p := newParentSettle(t)
	p.row(nil)
	p.github(t, nil, true)

	got := p.settle()

	if got != verdictDispositionAlreadyDoneVerified {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneVerified)
	}
	p.assertUntouched(t)
	if logged := p.logs.String(); !strings.Contains(logged, "sub-issues unreadable; not marking done") {
		t.Fatalf("want the unreadable warning, got:\n%s", logged)
	}
}

// Unchanged: an issue with no open child is recorded, labelled and closed as
// before, whether the answer comes from GitHub or no client is wired.
func TestVerdictSettle_NoOpenSubIssueMarksDoneAsBefore(t *testing.T) {
	cases := []struct {
		name     string
		children *ghpkg.IssueChildren
		summary  *ghpkg.IssueChildren
		client   bool
	}{
		{name: "no children, no GitHub client"},
		{name: "all children closed on the row, no GitHub client", children: &ghpkg.IssueChildren{Total: 3, Completed: 3}},
		{name: "no children, GitHub confirms none", client: true},
		{name: "no children, GitHub reports an empty block", client: true, summary: &ghpkg.IssueChildren{}},
		{name: "all children closed, GitHub confirms", children: &ghpkg.IssueChildren{Total: 3, Completed: 3}, summary: &ghpkg.IssueChildren{Total: 3, Completed: 3}, client: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newParentSettle(t)
			p.row(tc.children)
			if tc.client {
				p.github(t, tc.summary, false)
			}

			p.assertMarkedAsBefore(t, p.settle())

			if logged := p.logs.String(); strings.Contains(logged, "not marking done") {
				t.Fatalf("nothing was held, so nothing may be logged as held:\n%s", logged)
			}
		})
	}
}

// Unchanged: an open pull request cited by the verdict is not a "done" mark,
// so it is recorded and staged as covered whatever the issue's children say.
func TestVerdictSettle_OpenPRClaimOnParentIsUnchanged(t *testing.T) {
	p := newParentSettle(t)
	p.row(&ghpkg.IssueChildren{Total: 3, Completed: 0})
	p.hub.settleVerifier = p.fx.verifier(map[string]ghpkg.SettleVerification{
		"acme/widgets#41": {Settled: true, Claim: ghpkg.IssueClaim{
			PRNumber: 41, PRRepo: "acme/widgets", PRAuthor: "bob", Reference: true, ExternalAuthor: true,
		}},
	}, nil)

	got := p.settle()

	if got != verdictDispositionAlreadyDoneVerified {
		t.Fatalf("disposition = %q, want %q", got, verdictDispositionAlreadyDoneVerified)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fx.recorded) != 1 || p.claim != 1 || p.done != 0 {
		t.Fatalf("open PR claim: recorded=%d covered-label calls=%d already-done calls=%d, want 1/1/0", len(p.fx.recorded), p.claim, p.done)
	}
}

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/redmain"
)

func redMainTestServer(t *testing.T, red *bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "widget", "default_branch": "main"})
	})
	mux.HandleFunc("/repos/acme/widget/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"strict": false, "contexts": []string{"unit"}})
	})
	mux.HandleFunc("/repos/acme/widget/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/commits/main/check-runs", func(w http.ResponseWriter, r *http.Request) {
		unit := "success"
		if *red {
			unit = "failure"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{"id": 11, "name": "unit", "status": "completed", "conclusion": unit, "html_url": "https://github.test/acme/widget/runs/11", "started_at": "2026-10-22T07:50:00Z", "completed_at": "2026-10-22T08:00:00Z"},
			// Red but not required: never trips the breaker.
			{"id": 12, "name": "flaky-optional", "status": "completed", "conclusion": "failure", "started_at": "2026-10-22T07:50:00Z", "completed_at": "2026-10-22T08:00:00Z"},
		}})
	})
	mux.HandleFunc("/repos/acme/widget/check-runs/11/annotations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"path": "pkg/foo/foo_test.go", "annotation_level": "failure", "message": "TestFoo failed"},
			{"path": ".github", "annotation_level": "failure", "message": "Process completed with exit code 1."},
		})
	})
	mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "open" {
			t.Errorf("tracking issue lookup must list open issues, got state=%q", r.URL.Query().Get("state"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 501, "title": "[shared-ci] lint failing across acme/widget"},
			{"number": 502, "title": "[shared-ci] unit failing across acme/widget"},
			{"number": 503, "title": "[shared-ci] unit failing across acme/other"},
		})
	})
	return httptest.NewServer(mux)
}

// Detection end to end: only a REQUIRED check red on the default branch
// counts, it trips only past the threshold, it carries the run URL, the
// failing files and the tracking issue, and it clears when main is green.
func TestSweepRedMainDetectsAndClears(t *testing.T) {
	red := true
	srv := redMainTestServer(t, &red)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	completed := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)

	res, err := c.SweepRedMain(context.Background(), completed.Add(time.Minute))
	if err != nil || res.Red != 1 || res.Skipped != 0 {
		t.Fatalf("SweepRedMain = %+v, %v", res, err)
	}
	if f := c.RedMainFact("widget", completed.Add(6*time.Hour-time.Second)); f != nil {
		t.Fatalf("tripped before the threshold: %+v", f)
	}
	f := c.RedMainFact("widget", completed.Add(6*time.Hour+time.Second))
	if f == nil {
		t.Fatal("not tripped after the threshold")
	}
	if f.Check != "unit" || f.Branch != "main" || f.TrackingIssue != 502 || f.RunURL != "https://github.test/acme/widget/runs/11" || !f.Since.Equal(completed) {
		t.Fatalf("fact = %+v", f)
	}
	if len(f.Paths) != 1 || f.Paths[0] != "pkg/foo/foo_test.go" {
		t.Fatalf("annotated paths = %v", f.Paths)
	}

	// Per-repo threshold override.
	c.SetRedMainThresholdFunc(func(string) time.Duration { return time.Hour })
	if c.RedMainFact("acme/widget", completed.Add(61*time.Minute)) == nil {
		t.Fatal("a 1h repo threshold must trip after 61m")
	}
	c.SetRedMainThresholdFunc(nil)

	red = false
	if _, err := c.SweepRedMain(context.Background(), completed.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if f := c.RedMainFact("widget", completed.Add(8*time.Hour)); f != nil {
		t.Fatalf("green main still tripped: %+v", f)
	}
}

func TestSharedCITrackingCheck(t *testing.T) {
	cases := []struct {
		title string
		want  string
		ok    bool
	}{
		{"[shared-ci] unit failing across acme/widget", "unit", true},
		{"[shared-ci] test (rest 1/5) failing across ACME/Widget", "test (rest 1/5)", true},
		{"[shared-ci] unit failing across acme/other", "", false},
		{"[shared-ci] unit failing", "unit", true},
		{"[shared-ci] fix the thing", "", false},
		{"unit failing across acme/widget", "", false},
	}
	for _, tc := range cases {
		got, ok := SharedCITrackingCheck(tc.title, "acme/widget")
		if got != tc.want || ok != tc.ok {
			t.Errorf("SharedCITrackingCheck(%q) = %q,%v want %q,%v", tc.title, got, ok, tc.want, tc.ok)
		}
	}
}

func redMainTestClient(now time.Time) *Client {
	c := &Client{org: "acme", repos: []string{"widget"}}
	c.ObserveRedMain("widget", "main", map[string]redmain.RedRun{"unit": {URL: "run-url", TrackingIssue: 42, Paths: []string{"pkg/foo/foo_test.go"}}}, now.Add(-7*time.Hour))
	return c
}

func TestDetectRedMainFixPRs(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	c := redMainTestClient(now)
	fact := c.RedMainFact("widget", now)
	if fact == nil {
		t.Fatal("fact not tripped")
	}
	paths := map[int][]string{
		4: {"pkg/foo/foo.go"},
		5: {"docs/readme.md"},
	}
	prs := []PullRequest{
		{Repo: "acme/widget", Number: 1, Labels: []string{"fixes-main"}, CIStatus: "pending"},
		{Repo: "widget", Number: 2, LinkedIssues: []PRLinkedIssue{{Number: 42}}, CIStatus: "success", RequestedReviewers: []string{"alice"}},
		{Repo: "acme/widget", Number: 3, ReferencedIssues: []PRLinkedIssue{{Number: 42, Via: PRLinkViaRefs}}, CIStatus: "success"},
		{Repo: "acme/widget", Number: 4, CIStatus: "failure", Author: "bob", FailingChecks: []string{"unit"}},
		{Repo: "acme/widget", Number: 5, CIStatus: "success"},
		{Repo: "acme/other", Number: 6, Labels: []string{"fixes-main"}},
	}
	fixes := DetectRedMainFixPRs(fact, prs, func(_ string, n int, _ string) []string { return paths[n] })
	var got []int
	for _, f := range fixes {
		got = append(got, f.Number)
	}
	// #2 is confirmed (green while main is red) so it leads; #3 only Refs the
	// tracking issue; #5 is green but green alone is not detection; #6 is
	// another repo.
	if want := []int{2, 1, 4}; len(got) != len(want) || got[0] != 2 || got[1] != 1 || got[2] != 4 {
		t.Fatalf("fix PRs = %v, want %v", got, want)
	}
	if fixes[0].Via != redmain.ViaTrackingIssue || !fixes[0].Confirmed || fixes[0].WaitingOn != "@alice" {
		t.Fatalf("fix #2 = %+v", fixes[0])
	}
	if fixes[1].Via != redmain.ViaLabel || fixes[1].WaitingOn != "CI" {
		t.Fatalf("fix #1 = %+v", fixes[1])
	}
	if fixes[2].Via != redmain.ViaFileOverlap || fixes[2].WaitingOn != "@bob (CI red)" {
		t.Fatalf("fix #4 = %+v", fixes[2])
	}
}

// Pause and resume at the hive-open-pr choke point: a new PR is refused while
// main is red past the threshold (naming the check and the fix PR), a PR that
// fixes main still opens, and the pause lifts once main is green.
func TestRedMainPRRequestPauseAndResume(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	c := redMainTestClient(now)
	actionable := &ActionableResult{PRs: PRResult{Items: []PullRequest{
		{Repo: "acme/widget", Number: 77, Labels: []string{"fixes-main"}, CIStatus: "success"},
		{Repo: "acme/widget", Number: 78},
	}}}
	c.StampRedMain(actionable, nil, now)
	if f := RedMainFactFor(actionable.RedMain, "widget"); f == nil || len(f.FixPRs) != 1 || f.FixPRs[0].Number != 77 {
		t.Fatalf("stamped red-main = %+v", actionable.RedMain)
	}

	reason := c.redMainPRRequestRefusal(PRRequest{Repo: "widget", Title: "feat: new thing", Body: "Fixes #9"}, now)
	for _, want := range []string{`"unit"`, "#77", "Fixes #42"} {
		if !strings.Contains(reason, want) {
			t.Errorf("refusal missing %q: %s", want, reason)
		}
	}
	if r := c.redMainPRRequestRefusal(PRRequest{Repo: "widget", IssueN: []int{42}}, now); r != "" {
		t.Errorf("a PR declaring the tracking issue must open: %s", r)
	}
	if r := c.redMainPRRequestRefusal(PRRequest{Repo: "acme/widget", Body: "Fixes #42"}, now); r != "" {
		t.Errorf("a PR closing the tracking issue must open: %s", r)
	}
	if r := c.redMainPRRequestRefusal(PRRequest{Repo: "other", Body: "x"}, now); r != "" {
		t.Errorf("another repo is not paused: %s", r)
	}
	// Below the threshold nothing is paused.
	if r := c.redMainPRRequestRefusal(PRRequest{Repo: "widget"}, now.Add(-2*time.Hour)); r != "" {
		t.Errorf("paused before the threshold: %s", r)
	}

	// Main goes green: pause and stamp lift with nothing to undo.
	c.ObserveRedMain("widget", "main", nil, now)
	if r := c.redMainPRRequestRefusal(PRRequest{Repo: "widget"}, now); r != "" {
		t.Errorf("pause did not lift on green: %s", r)
	}
	c.StampRedMain(actionable, nil, now)
	if actionable.RedMain != nil {
		t.Errorf("green main still stamped: %+v", actionable.RedMain)
	}
}

// Fix-main PRs rank ahead of everything else, including a T0 fix.
func TestReviewQueueFixesMainFirst(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)
	fact := &redmain.Fact{Repo: "acme/widget", Check: "unit", Since: now.Add(-8 * time.Hour), TrackingIssue: 42}
	prs := []PullRequest{
		{Repo: "widget", Number: 10, Title: "🐛 fix: crash", HeadSHA: "a", CIStatus: "success", CreatedAt: now.Add(-48 * time.Hour)},
		{Repo: "widget", Number: 11, Title: "🧪 test: more coverage", HeadSHA: "b", CIStatus: "success", CreatedAt: now.Add(-time.Hour), LinkedIssues: []PRLinkedIssue{{Number: 42}}},
		{Repo: "other", Number: 12, Title: "🧪 test: coverage", HeadSHA: "c", Labels: []string{"fixes-main"}, CreatedAt: now.Add(-time.Hour)},
	}
	q := BuildReviewQueue(prs, ReviewQueueOptions{Org: "acme", Now: now, RedMain: map[string]*redmain.Fact{"acme/widget": fact}})
	if len(q) != 3 || q[0].Number != 11 || !q[0].FixesMain || q[0].FixesMainVia != redmain.ViaTrackingIssue {
		t.Fatalf("queue = %+v, want #11 first as fixes-main", q)
	}
	if !strings.HasPrefix(q[0].Reasons[0], "fixes red main (unit red since") || !strings.Contains(q[0].Reasons[0], "confirmed") {
		t.Fatalf("reasons = %v", q[0].Reasons)
	}
	if q[1].Number != 10 || q[1].FixesMain || q[2].FixesMain {
		t.Fatalf("queue = %+v: the label on a PR in a green repo must not count", q)
	}

	// Main green again: ordinary order.
	q = BuildReviewQueue(prs, ReviewQueueOptions{Org: "acme", Now: now})
	if q[0].Number != 10 {
		t.Fatalf("green main queue = %+v, want the T0 fix first", q)
	}
	// ReviewQueueFromActionable reads the snapshot's stamp.
	a := &ActionableResult{PRs: PRResult{Items: prs}, RedMain: map[string]*redmain.Fact{"acme/widget": fact}}
	if q := ReviewQueueFromActionable(a, ReviewQueueOptions{Org: "acme", Now: now}); q[0].Number != 11 {
		t.Fatalf("snapshot queue = %+v, want #11 first", q)
	}
}

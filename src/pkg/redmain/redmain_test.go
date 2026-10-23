package redmain

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)

func TestTrackerThresholdEdges(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {URL: "https://example.test/run/1"}}, t0)

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"just red", t0, false},
		{"just under N", t0.Add(DefaultThreshold - time.Second), false},
		{"exactly N is not more than N", t0.Add(DefaultThreshold), false},
		{"just over N", t0.Add(DefaultThreshold + time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tr.Active("acme/widget", 0, tc.at)
			if (got != nil) != tc.want {
				t.Fatalf("Active at %s = %+v, want tripped=%v", tc.at.Sub(t0), got, tc.want)
			}
			if got != nil && (got.Check != "unit" || !got.Since.Equal(t0) || got.RunURL != "https://example.test/run/1" || got.Branch != "main") {
				t.Fatalf("fact = %+v", got)
			}
		})
	}
}

func TestTrackerPerRepoThreshold(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {}}, t0)
	at := t0.Add(2*time.Hour + time.Minute)
	if f := tr.Active("acme/widget", 2*time.Hour, at); f == nil {
		t.Fatal("a 2h repo threshold must trip after 2h01m")
	}
	if f := tr.Active("acme/widget", 0, at); f != nil {
		t.Fatalf("the 6h default must not trip after 2h01m: %+v", f)
	}
}

func TestTrackerKeepsFirstRedAndClearsOnGreen(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {URL: "run-1"}}, t0)
	// Still red hours later on a newer run: dated from the first sighting.
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {URL: "run-2"}}, t0.Add(5*time.Hour))
	f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour))
	if f == nil || !f.Since.Equal(t0) || f.RunURL != "run-2" {
		t.Fatalf("fact = %+v, want since the first red with the latest run URL", f)
	}
	// Green: the fact clears at once.
	tr.Observe("acme/widget", "main", nil, t0.Add(7*time.Hour))
	if f := tr.Active("acme/widget", 0, t0.Add(8*time.Hour)); f != nil {
		t.Fatalf("green main still reports %+v", f)
	}
	// Red again: the clock restarts.
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {}}, t0.Add(9*time.Hour))
	if f := tr.Active("acme/widget", 0, t0.Add(10*time.Hour)); f != nil {
		t.Fatalf("a fresh red must not inherit the old start: %+v", f)
	}
}

func TestTrackerDatesFromRunCompletion(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {CompletedAt: t0.Add(-7 * time.Hour)}}, t0)
	if f := tr.Active("acme/widget", 0, t0); f == nil || !f.Since.Equal(t0.Add(-7*time.Hour)) {
		t.Fatalf("first sighting must date from the failing run's completion: %+v", f)
	}
	tr.Observe("acme/other", "main", map[string]RedRun{"unit": {CompletedAt: t0.Add(time.Hour)}}, t0)
	if f := tr.Active("acme/other", time.Minute, t0.Add(2*time.Minute)); f == nil || !f.Since.Equal(t0) {
		t.Fatalf("a completion time in the future must not be used: %+v", f)
	}
}

func TestTrackerPicksLongestRedCheck(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"lint": {}}, t0)
	tr.Observe("acme/widget", "main", map[string]RedRun{"lint": {}, "unit": {}}, t0.Add(time.Hour))
	f := tr.Active("acme/widget", 0, t0.Add(8*time.Hour))
	if f == nil || f.Check != "lint" {
		t.Fatalf("fact = %+v, want the longest-red check", f)
	}
	if f := tr.Active("ACME/Widget", 0, t0.Add(8*time.Hour)); f == nil {
		t.Fatal("repo lookup must be case-insensitive")
	}
	if f := tr.Active("acme/green", 0, t0.Add(8*time.Hour)); f != nil {
		t.Fatalf("an unobserved repo reports %+v", f)
	}
}

func TestFixesMain(t *testing.T) {
	fact := &Fact{Repo: "acme/widget", Check: "unit", TrackingIssue: 42, Paths: []string{"pkg/foo/foo_test.go"}}
	cases := []struct {
		name    string
		in      PRInput
		wantVia string
	}{
		{"explicit label", PRInput{Labels: []string{"Fixes-Main"}}, ViaLabel},
		{"fixes the tracking issue", PRInput{ClosingIssues: []int{7, 42}}, ViaTrackingIssue},
		{"touches the failing test", PRInput{Paths: []string{"pkg/foo/foo_test.go"}}, ViaFileOverlap},
		{"touches code beside the failing test", PRInput{Paths: []string{"pkg/foo/foo.go"}}, ViaFileOverlap},
		{"touches the workflow", PRInput{Paths: []string{".github/workflows/ci.yml"}}, ViaFileOverlap},
		{"unrelated PR", PRInput{Paths: []string{"docs/readme.md"}, ClosingIssues: []int{7}}, ""},
		{"green checks alone are only confirmation", PRInput{CIStatus: "success"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			via, ok := FixesMain(fact, tc.in)
			if ok != (tc.wantVia != "") || via != tc.wantVia {
				t.Fatalf("FixesMain = %q,%v want %q", via, ok, tc.wantVia)
			}
		})
	}
	if _, ok := FixesMain(nil, PRInput{Labels: []string{FixesMainLabel}}); ok {
		t.Fatal("no fact, no fix")
	}
	if _, ok := FixesMain(&Fact{Check: "unit"}, PRInput{ClosingIssues: []int{0}}); ok {
		t.Fatal("no tracking issue must not match a zero reference")
	}
}

func TestConfirmed(t *testing.T) {
	fact := &Fact{Check: "unit"}
	if !Confirmed(fact, PRInput{CIStatus: "success"}) {
		t.Fatal("green PR while main is red confirms")
	}
	if Confirmed(fact, PRInput{CIStatus: "failure", FailingChecks: []string{"unit"}}) {
		t.Fatal("PR red on the same check does not confirm")
	}
	if Confirmed(fact, PRInput{CIStatus: "pending"}) {
		t.Fatal("pending CI does not confirm")
	}
}

func TestMessagesNameCheckAndFix(t *testing.T) {
	f := &Fact{Repo: "acme/widget", Branch: "main", Check: "unit", Since: t0, RunURL: "https://example.test/run/1", TrackingIssue: 42}
	if got, want := f.CardLine(), "main red since 2026-10-22 08:00 UTC on unit; fix: no fix PR yet"; got != want {
		t.Fatalf("CardLine = %q, want %q", got, want)
	}
	f.FixPRs = []FixPR{{Number: 7, WaitingOn: "@alice"}, {Number: 9, Confirmed: true, WaitingOn: "@bob"}}
	if got, want := f.CardLine(), "main red since 2026-10-22 08:00 UTC on unit; fix: #9 waiting on @bob"; got != want {
		t.Fatalf("CardLine = %q, want %q (confirmed fix first)", got, want)
	}
	reason := f.PauseReason()
	for _, want := range []string{"acme/widget", `"unit"`, "#9", "Fixes #42", "Existing PRs keep going", "lifts automatically"} {
		if !strings.Contains(reason, want) {
			t.Errorf("PauseReason missing %q:\n%s", want, reason)
		}
	}
	f.TrackingIssue = 0
	if r := f.PauseReason(); !strings.Contains(r, "[shared-ci] unit failing across acme/widget") {
		t.Errorf("without a tracking issue the reason must say how to file one:\n%s", r)
	}
}

func TestSortFixPRs(t *testing.T) {
	prs := []FixPR{{Number: 9}, {Number: 3}, {Number: 5, Confirmed: true}}
	SortFixPRs(prs)
	if prs[0].Number != 5 || prs[1].Number != 3 || prs[2].Number != 9 {
		t.Fatalf("order = %+v", prs)
	}
}

// A check whose tip run has not completed (a push to the red branch) is
// neither red nor green: it keeps its entry, since and run unchanged, and
// only a completed green observation clears it.
func TestTrackerPendingCarriesOver(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {URL: "run-1", TrackingIssue: 7}}, t0)
	for _, at := range []time.Duration{3 * time.Hour, 5 * time.Hour} {
		tr.ObservePending("acme/widget", "main", nil, map[string]bool{"unit": true, "lint": true}, t0.Add(at))
	}
	f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour))
	if f == nil || !f.Since.Equal(t0) || f.RunURL != "run-1" || f.TrackingIssue != 7 || f.Check != "unit" {
		t.Fatalf("fact = %+v, want the pre-push entry unchanged", f)
	}
	// A pending check that was never red is not invented.
	tr.ObservePending("acme/other", "main", nil, map[string]bool{"unit": true}, t0)
	if f := tr.Active("acme/other", time.Minute, t0.Add(time.Hour)); f != nil {
		t.Fatalf("pending without a prior red tripped: %+v", f)
	}
	// Still red once the new run completes: dated from the first sighting.
	tr.ObservePending("acme/widget", "main", map[string]RedRun{"unit": {URL: "run-2", CompletedAt: t0.Add(6 * time.Hour)}}, map[string]bool{"lint": true}, t0.Add(6*time.Hour))
	if f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour)); f == nil || !f.Since.Equal(t0) || f.RunURL != "run-2" {
		t.Fatalf("fact = %+v, want since the first red with the new run", f)
	}
	// Completed green: cleared.
	tr.ObservePending("acme/widget", "main", nil, nil, t0.Add(7*time.Hour))
	if f := tr.Active("acme/widget", 0, t0.Add(8*time.Hour)); f != nil {
		t.Fatalf("green main still reports %+v", f)
	}
}

func TestTrackerTrackingIssue(t *testing.T) {
	tr := NewTracker()
	tr.SetTrackingIssue("acme/widget", "unit", 9)
	if n := tr.TrackingIssue("acme/widget", "unit"); n != 0 {
		t.Fatalf("untracked check got tracking issue %d", n)
	}
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {}}, t0)
	tr.SetTrackingIssue("ACME/Widget", "unit", 9)
	if n := tr.TrackingIssue("acme/widget", "unit"); n != 9 {
		t.Fatalf("TrackingIssue = %d, want 9", n)
	}
	if f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour)); f == nil || f.TrackingIssue != 9 {
		t.Fatalf("fact = %+v, want tracking issue 9", f)
	}
	var nilTracker *Tracker
	nilTracker.SetTrackingIssue("acme/widget", "unit", 1)
	if nilTracker.TrackingIssue("acme/widget", "unit") != 0 {
		t.Fatal("nil tracker must report 0")
	}
}

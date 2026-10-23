package redmain

import (
	"strings"
	"testing"
	"time"
)

func TestNilReceiversAreInert(t *testing.T) {
	var f *Fact
	if f.PrimaryFix() != nil || f.IsFixPR(1) || f.CardLine() != "" || f.PauseReason() != "" {
		t.Fatal("a nil fact must report nothing")
	}
	if Confirmed(nil, PRInput{CIStatus: "success"}) {
		t.Fatal("a nil fact confirms nothing")
	}
	var tr *Tracker
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {}}, t0)
	tr.Forget("acme/widget")
	if tr.Active("acme/widget", 0, t0.Add(24*time.Hour)) != nil {
		t.Fatal("a nil tracker must report nothing")
	}
}

func TestIsFixPRAndPrimaryFix(t *testing.T) {
	f := &Fact{Check: "unit"}
	if f.PrimaryFix() != nil || f.IsFixPR(7) {
		t.Fatal("no fix PRs yet")
	}
	f.FixPRs = []FixPR{{Number: 7}, {Number: 9}}
	if !f.IsFixPR(7) || !f.IsFixPR(9) || f.IsFixPR(8) {
		t.Fatalf("IsFixPR over %+v", f.FixPRs)
	}
	if fix := f.PrimaryFix(); fix == nil || fix.Number != 7 {
		t.Fatalf("without a confirmed fix the first PR is primary, got %+v", fix)
	}
	f.FixPRs[1].Confirmed = true
	if fix := f.PrimaryFix(); fix == nil || fix.Number != 9 {
		t.Fatalf("a confirmed fix wins, got %+v", fix)
	}
}

func TestPauseReasonWithoutRunFixOrBranch(t *testing.T) {
	f := &Fact{Repo: "acme/widget", Check: "unit", Since: t0}
	reason := f.PauseReason()
	for _, want := range []string{"the default branch", "No fix PR is open yet", "[shared-ci] unit failing across acme/widget"} {
		if !strings.Contains(reason, want) {
			t.Errorf("PauseReason missing %q:\n%s", want, reason)
		}
	}
	if strings.Contains(reason, "(") && strings.Contains(reason, "http") {
		t.Errorf("no run URL must mean no run link:\n%s", reason)
	}
	f.FixPRs = []FixPR{{Number: 7}}
	if got := f.CardLine(); !strings.HasSuffix(got, "fix: #7 waiting on review") {
		t.Fatalf("an unnamed waiter defaults to review: %q", got)
	}
	if r := f.PauseReason(); !strings.Contains(r, "#7 (waiting on review)") {
		t.Errorf("PauseReason must name the fix and the default waiter:\n%s", r)
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Fatalf("all blank must give empty, got %q", got)
	}
}

func TestPathsOverlapEdges(t *testing.T) {
	if PathsOverlap([]string{"pkg/foo/foo_test.go"}, nil) {
		t.Fatal("no changed files, no overlap")
	}
	if PathsOverlap([]string{"", " ", ".github"}, []string{"docs/readme.md", ""}) {
		t.Fatal("blank and bare .github failing paths must be ignored")
	}
	if !PathsOverlap([]string{`.\pkg\foo\foo_test.go`}, []string{"./pkg/foo/foo.go"}) {
		t.Fatal("paths must be normalised before comparison")
	}
	if PathsOverlap([]string{"README.md"}, []string{"LICENSE"}) {
		t.Fatal("a root-level failing file must not make every root file overlap")
	}
	if !PathsOverlap([]string{"README.md"}, []string{"README.md"}) {
		t.Fatal("the same root-level file overlaps")
	}
	if !PathsOverlap(nil, []string{".github/workflows/ci.yml"}) {
		t.Fatal("a workflow change overlaps even without annotated paths")
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{
		"  ./pkg/foo.go ": "pkg/foo.go",
		`pkg\foo\bar.go`:  "pkg/foo/bar.go",
		`.\pkg\foo.go`:    "pkg/foo.go",
		"":                "",
		"docs/readme.md":  "docs/readme.md",
	}
	for in, want := range cases {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackerIgnoresBlankRepoAndForgets(t *testing.T) {
	tr := NewTracker()
	tr.Observe("  ", "main", map[string]RedRun{"unit": {}}, t0)
	if len(tr.repos) != 0 {
		t.Fatal("a blank repo must not be recorded")
	}
	tr.Observe("acme/widget", "", map[string]RedRun{"unit": {}}, t0)
	f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour))
	if f == nil || f.Branch != "" {
		t.Fatalf("an unknown branch stays empty: %+v", f)
	}
	tr.Observe("acme/widget", "release", map[string]RedRun{"unit": {}}, t0.Add(time.Hour))
	if f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour)); f == nil || f.Branch != "release" {
		t.Fatalf("the latest observed branch is reported: %+v", f)
	}
	tr.Forget("ACME/widget")
	if f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour)); f != nil {
		t.Fatalf("Forget must clear the repo: %+v", f)
	}
	if _, ok := tr.branches["acme/widget"]; ok {
		t.Fatal("Forget must drop the branch too")
	}
}

func TestTrackerTieBreaksOnCheckName(t *testing.T) {
	tr := NewTracker()
	tr.Observe("acme/widget", "main", map[string]RedRun{
		"unit": {TrackingIssue: 42, Paths: []string{"pkg/a/a_test.go"}},
		"lint": {TrackingIssue: 43},
	}, t0)
	f := tr.Active("acme/widget", 0, t0.Add(7*time.Hour))
	if f == nil || f.Check != "lint" || f.TrackingIssue != 43 {
		t.Fatalf("equal start times must pick the lexically first check: %+v", f)
	}
	// Active copies the paths so callers cannot mutate tracker state.
	tr.Observe("acme/widget", "main", map[string]RedRun{"unit": {Paths: []string{"pkg/a/a_test.go"}}}, t0)
	f = tr.Active("acme/widget", 0, t0.Add(7*time.Hour))
	if f == nil || len(f.Paths) != 1 {
		t.Fatalf("fact = %+v", f)
	}
	f.Paths[0] = "mutated"
	if g := tr.Active("acme/widget", 0, t0.Add(7*time.Hour)); g.Paths[0] != "pkg/a/a_test.go" {
		t.Fatalf("Active must return a copy of the paths, got %q", g.Paths[0])
	}
}

func TestSortFixPRsIsStableAcrossConfirmed(t *testing.T) {
	prs := []FixPR{{Number: 9, Confirmed: true}, {Number: 3}, {Number: 5, Confirmed: true}, {Number: 1}}
	SortFixPRs(prs)
	want := []int{5, 9, 1, 3}
	for i, n := range want {
		if prs[i].Number != n {
			t.Fatalf("order = %+v, want %v", prs, want)
		}
	}
}

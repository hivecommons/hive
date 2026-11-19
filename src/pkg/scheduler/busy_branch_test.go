package scheduler

// hivecommons/hive#18488: the scanner pushed its own commits onto PR
// branches another worker was still pushing to — an owner-session PR it
// reached as an unattributed "yours" row in FIX-BEFORE-NEW — and the
// "Sign-off survives the squash" check went red (#18419). These tests pin the
// invariant: a non-App branch that is busy (a live claim by someone else, or
// a head pushed inside the busy window) is never listed as a push target in
// either repair queue, and the same branch once idle is listed exactly as
// before.

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

var busyTestNow = time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)

func busyPushedAt(ago time.Duration) *time.Time {
	at := busyTestNow.Add(-ago)
	return &at
}

func identityBase(name string) string { return name }

func TestBusyBranchReason(t *testing.T) {
	window := time.Duration(DefaultBusyBranchMinutes) * time.Minute
	leaseOn := func(repo string, number int, holder, holderAgent string) PRLeaseLookup {
		return func(r string, n int) (string, string, bool) {
			if r == repo && n == number {
				return holder, holderAgent, true
			}
			return "", "", false
		}
	}
	const repo = "test-org/hive"
	cases := []struct {
		name  string
		row   busyBranchRow
		lease PRLeaseLookup
		busy  bool
	}{
		{"idle: no claim, old push", busyBranchRow{Repo: repo, Number: 8, HeadPushedAt: busyPushedAt(2 * window)}, nil, false},
		{"idle: no claim, no push evidence", busyBranchRow{Repo: repo, Number: 8}, leaseOn(repo, 99, "x", ""), false},
		{"busy: head pushed inside the window", busyBranchRow{Repo: repo, Number: 8, HeadPushedAt: busyPushedAt(5 * time.Minute)}, nil, true},
		{"busy: PR claimed by a person", busyBranchRow{Repo: repo, Number: 8}, leaseOn(repo, 8, "clubanderson (human)", ""), true},
		{"busy: linked issue claimed by another agent", busyBranchRow{Repo: repo, Number: 8, Issues: []int{18488}}, leaseOn(repo, 18488, "quality (agent)", "quality"), true},
		{"idle: the kicked agent's own claim", busyBranchRow{Repo: repo, Number: 8, Issues: []int{18488}}, leaseOn(repo, 18488, "scanner (agent)", "scanner"), false},
		{"own row: its own recent push is not another worker", busyBranchRow{Repo: repo, Number: 8, Own: true, HeadPushedAt: busyPushedAt(time.Minute)}, nil, false},
		{"own row: another worker's claim still counts", busyBranchRow{Repo: repo, Number: 8, Own: true}, leaseOn(repo, 8, "clubanderson (human)", ""), true},
		{"App-authored branch is never checked", busyBranchRow{Repo: repo, Number: 8, AppAuthored: true, HeadPushedAt: busyPushedAt(time.Minute)}, leaseOn(repo, 8, "clubanderson (human)", ""), false},
		{"adopted branch is never checked", busyBranchRow{Repo: repo, Number: 8, Adopted: true, HeadPushedAt: busyPushedAt(time.Minute)}, leaseOn(repo, 8, "clubanderson (human)", ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			why := busyBranchReason(tc.row, "scanner", tc.lease, identityBase, window, busyTestNow)
			if got := why != ""; got != tc.busy {
				t.Fatalf("busy = %v (reason %q), want %v", got, why, tc.busy)
			}
		})
	}
}

func TestBusyBranchWindow_EnvOverride(t *testing.T) {
	t.Setenv(BusyBranchMinutesEnv, "")
	if got := busyBranchWindow(); got != time.Duration(DefaultBusyBranchMinutes)*time.Minute {
		t.Fatalf("default window = %s", got)
	}
	t.Setenv(BusyBranchMinutesEnv, "15")
	if got := busyBranchWindow(); got != 15*time.Minute {
		t.Fatalf("override window = %s, want 15m", got)
	}
	for _, bad := range []string{"0", "-5", "soon"} {
		t.Setenv(BusyBranchMinutesEnv, bad)
		if got := busyBranchWindow(); got != time.Duration(DefaultBusyBranchMinutes)*time.Minute {
			t.Fatalf("%q must keep the default, got %s", bad, got)
		}
	}
}

// busyQueueFixture: three unattributed same-repo PRs that are not App-authored
// (each defaults to scanner) — #18419 pushed 5 minutes ago, #18420 whose
// linked issue a person holds, #18421 idle — plus an App-authored #18422
// pushed a minute ago, which stays the hive's own push target.
func busyQueueFixture() string {
	ts := func(ago time.Duration) string { return busyTestNow.Add(-ago).Format(time.RFC3339) }
	return fmt.Sprintf(`{"ci_failing":[
  {"number":18419,"repo":"test-org/hive","title":"owner session PR","author":"clubanderson","reachable_action":"push",
   "head_pushed_at":%q,"failing_checks":["Sign-off survives the squash"],"excerpt":"undefined: newMinimalServer"},
  {"number":18420,"repo":"test-org/hive","title":"claimed PR","author":"clubanderson","reachable_action":"push",
   "head_pushed_at":%q,"issues":[18488],"failing_checks":["build"]},
  {"number":18421,"repo":"test-org/hive","title":"idle PR","author":"clubanderson","reachable_action":"push",
   "head_pushed_at":%q,"failing_checks":["lint"]},
  {"number":18422,"repo":"test-org/hive","title":"hive lane PR","author":"hivecommons-hive[bot]","app_authored":true,
   "reachable_action":"push","head_pushed_at":%q,"failing_checks":["test"]}
]}`, ts(5*time.Minute), ts(3*time.Hour), ts(3*time.Hour), ts(time.Minute))
}

func withBusyQueue(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	orig, origNow := ciFailingPath, busyBranchNow
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	busyBranchNow = func() time.Time { return busyTestNow }
	t.Cleanup(func() { ciFailingPath, busyBranchNow = orig, origNow })
	t.Setenv(BusyBranchMinutesEnv, "")
	if err := os.WriteFile(ciFailingPath, []byte(busyQueueFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newScheduler()
	s.SetPRLeaseLookup(func(repo string, number int) (string, string, bool) {
		if repo == "test-org/hive" && number == 18488 {
			return "clubanderson (human) until 2026-11-16T18:00:00Z", "", true
		}
		return "", "", false
	})
	return s
}

func TestFormatRedPRFixData_BusyBranchesAreNotPushTargets(t *testing.T) {
	s := withBusyQueue(t)
	out := s.redPRFixSection("scanner")

	busyIdx := strings.Index(out, "BUSY BRANCHES")
	if busyIdx < 0 {
		t.Fatalf("busy rows must be named in their own block:\n%s", out)
	}
	gate, busy := out[:busyIdx], out[busyIdx:]
	if !strings.Contains(gate, "FIX-BEFORE-NEW") || !strings.Contains(gate, "(2)") {
		t.Fatalf("the push gate must hold exactly the idle and App-authored PRs:\n%s", out)
	}
	for _, want := range []string{"#18421 ", "#18422 "} {
		if !strings.Contains(gate, want) {
			t.Errorf("%s must stay a push target:\n%s", want, out)
		}
	}
	for _, reject := range []string{"#18419 ", "#18420 "} {
		if strings.Contains(gate, reject) {
			t.Errorf("busy %s must never be listed under the push gate:\n%s", reject, out)
		}
		if !strings.Contains(busy, reject) {
			t.Errorf("busy %s must be listed as suggest-only:\n%s", reject, out)
		}
	}
	for _, want := range []string{"Do NOT push", "suggestion", "edit it rather than adding another", "head pushed 5m0s ago", "linked issue #18488 is claimed by clubanderson", "undefined: newMinimalServer"} {
		if !strings.Contains(busy, want) {
			t.Errorf("busy block missing %q:\n%s", want, busy)
		}
	}

	// The same branches once idle (no claim, head older than the window)
	// are push targets again, exactly as before the guard.
	s.SetPRLeaseLookup(nil)
	busyBranchNow = func() time.Time { return busyTestNow.Add(2 * time.Hour) }
	idle := s.redPRFixSection("scanner")
	if strings.Contains(idle, "BUSY BRANCHES") {
		t.Fatalf("idle branches must not be reported busy:\n%s", idle)
	}
	for _, want := range []string{"(4)", "#18419 ", "#18420 ", "#18421 ", "#18422 ", "gh pr checkout"} {
		if !strings.Contains(idle, want) {
			t.Errorf("idle gate missing %q:\n%s", want, idle)
		}
	}
}

// Only busy rows: they are listed, but no FIX-BEFORE-NEW gate is raised —
// nothing the agent may push to must not block its new work.
func TestFormatRedPRFixData_OnlyBusyRowsRaiseNoGate(t *testing.T) {
	ts := busyTestNow.Add(-time.Minute).Format(time.RFC3339)
	data := fmt.Sprintf(`{"ci_failing":[{"number":5,"repo":"o/r","title":"t","author":"alice","reachable_action":"push","head_pushed_at":%q}]}`, ts)
	check := func(row busyBranchRow) string {
		return busyBranchReason(row, "scanner", nil, identityBase, time.Hour, busyTestNow)
	}
	out := formatRedPRFixDataGuarded([]byte(data), "scanner", nil, check)
	if strings.Contains(out, "FIX-BEFORE-NEW") {
		t.Fatalf("a queue of only busy rows must not raise the gate:\n%s", out)
	}
	if !strings.Contains(out, "BUSY BRANCHES") || !strings.Contains(out, "#5 ") {
		t.Fatalf("the busy row must still be named:\n%s", out)
	}
}

func TestBuildCIFailingList_BusyBranchesAreCommentOnly(t *testing.T) {
	s := withBusyQueue(t)
	out := s.buildCIFailingListForAgent("scanner", nil)

	busyIdx := strings.Index(out, "BUSY BRANCHES (2 — another worker is on these branches; suggest the fix, do not push)")
	if busyIdx < 0 {
		t.Fatalf("busy section missing:\n%s", out)
	}
	push, busy := out[:busyIdx], out[busyIdx:]
	for _, want := range []string{"#18421 ", "#18422 "} {
		if !strings.Contains(push, want) {
			t.Errorf("%s must stay in the push queue:\n%s", want, out)
		}
	}
	for _, reject := range []string{"#18419 ", "#18420 "} {
		if strings.Contains(push, reject) {
			t.Errorf("busy %s must never be in the push queue:\n%s", reject, out)
		}
		if !strings.Contains(busy, reject) {
			t.Errorf("busy %s must be listed as suggest-only:\n%s", reject, out)
		}
	}

	s.SetPRLeaseLookup(nil)
	busyBranchNow = func() time.Time { return busyTestNow.Add(2 * time.Hour) }
	idle := s.buildCIFailingListForAgent("scanner", nil)
	if strings.Contains(idle, "BUSY BRANCHES") {
		t.Fatalf("idle branches must not be reported busy:\n%s", idle)
	}
	for _, want := range []string{"#18419 ", "#18420 ", "#18421 ", "#18422 "} {
		if !strings.Contains(idle, want) {
			t.Errorf("idle queue missing %s:\n%s", want, idle)
		}
	}
}

// The busy evidence cmd/hive stamps on ci-failing.json rows.

func TestPRAppAuthored(t *testing.T) {
	cases := []struct {
		pr   github.PullRequest
		want bool
	}{
		{github.PullRequest{AppAuthored: true}, true},
		{github.PullRequest{Author: "app/hivecommons-hive"}, true},
		// A PR a hive agent opened on a person's credentials is that
		// person's branch, not the App's.
		{github.PullRequest{Author: "clubanderson", HiveAttributed: true, HiveAgent: "scanner"}, false},
		{github.PullRequest{Author: "alice"}, false},
	}
	for _, tc := range cases {
		if got := PRAppAuthored(tc.pr); got != tc.want {
			t.Errorf("PRAppAuthored(%+v) = %v, want %v", tc.pr, got, tc.want)
		}
	}
}

func TestPRHeadPushedAt_EarliestActivityOnCurrentHead(t *testing.T) {
	at := func(m int) *time.Time {
		v := time.Date(2026, 11, 16, 15, m, 0, 0, time.UTC)
		return &v
	}
	pr := github.PullRequest{HeadSHA: "abc", CIActivity: &github.PRCIActivity{Head: "abc", Checks: []github.PRActivityCheck{
		{Name: "build", Head: "abc", Since: at(30)},
		{Name: "dco", Head: "abc", Since: at(12)},
		{Name: "old head", Head: "zzz", Since: at(1)}, // a previous head's run is not this push
		{Name: "queued", Head: "abc"},
	}}}
	got := PRHeadPushedAt(pr)
	if got == nil || !got.Equal(*at(12)) {
		t.Fatalf("head pushed at = %v, want %v", got, at(12))
	}
	if PRHeadPushedAt(github.PullRequest{HeadSHA: "abc"}) != nil {
		t.Fatal("no CI activity must report no push time, never a guess")
	}
}

func TestPRIssues_SameRepoDeduplicated(t *testing.T) {
	pr := github.PullRequest{Repo: "hive", Number: 18419,
		LinkedIssues:     []github.PRLinkedIssue{{Number: 18488, Repo: "hivecommons/hive"}, {Number: 7, Repo: "other/repo"}},
		ReferencedIssues: []github.PRLinkedIssue{{Number: 18441}, {Number: 18488}, {Number: 18419}},
	}
	if got, want := PRIssueNumbers(pr), []int{18441, 18488}; !reflect.DeepEqual(got, want) {
		t.Fatalf("issues = %v, want %v", got, want)
	}
}

// ${PR_LIST} carries the same verdict inline, next to the fork marker, so an
// agent working the PR list (CI repair step 4, conflict rebases) learns from
// the list itself that a busy branch is not its to push to.
func TestFormatPRList_AnnotatesBusyBranches(t *testing.T) {
	orig := busyBranchNow
	busyBranchNow = func() time.Time { return busyTestNow }
	t.Cleanup(func() { busyBranchNow = orig })
	t.Setenv(BusyBranchMinutesEnv, "")
	activity := func(ago time.Duration) *github.PRCIActivity {
		return &github.PRCIActivity{Head: "h", Checks: []github.PRActivityCheck{{Name: "dco", Head: "h", Since: busyPushedAt(ago)}}}
	}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "hive", Number: 18419, Title: "owner session PR", Author: "clubanderson", HeadSHA: "h", CIActivity: activity(5 * time.Minute)},
		{Repo: "hive", Number: 18421, Title: "idle owner PR", Author: "clubanderson", HeadSHA: "h", CIActivity: activity(3 * time.Hour)},
		{Repo: "hive", Number: 18422, Title: "hive lane PR", Author: "hivecommons-hive[bot]", AppAuthored: true, HeadSHA: "h", CIActivity: activity(time.Minute)},
	}}}
	s := newScheduler()
	out, _ := s.formatPRListWithPolicyForAgent(actionable, "scanner")
	lines := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		for _, n := range []int{18419, 18421, 18422} {
			if strings.Contains(line, fmt.Sprintf("#%d ", n)) {
				lines[n] = line
			}
		}
	}
	if !strings.Contains(lines[18419], "[busy: head pushed 5m0s ago") || !strings.Contains(lines[18419], "do not push") {
		t.Errorf("busy PR must be annotated do-not-push: %q", lines[18419])
	}
	for _, n := range []int{18421, 18422} {
		if lines[n] == "" || strings.Contains(lines[n], "[busy:") {
			t.Errorf("#%d must be listed without a busy marker: %q\n%s", n, lines[n], out)
		}
	}
}

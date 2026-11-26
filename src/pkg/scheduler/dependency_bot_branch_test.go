package scheduler

// The scanner merged the base into Renovate branches with plain git: its
// shared-CI triage said MERGE_BASE for a red PR that was behind, and once the
// busy window passed nothing in the work list said the branch was a
// dependency bot's (projectbluefin/utah#1471; Renovate then refused to rebase
// it). These tests pin the invariant: a dependency bot's branch is never a
// push target in either repair queue and carries a do-not-push marker in
// ${PR_LIST}, whatever the age of its head, while App-authored and adopted
// rows, fork and busy rows, and ordinary rows render as before.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestDependencyBotBranchRow(t *testing.T) {
	cases := []struct {
		name                 string
		headRef, author      string
		appAuthored, adopted bool
		want                 bool
	}{
		{"dependency-bot author on a renovate/ branch", "renovate/human-merge-paths-actions", "mergeraptor[bot]", false, false, true},
		{"dependency-bot author on any branch", "bump-go", "renovate[bot]", false, false, true},
		{"renovate/ head ref under a non-bot account", "renovate/go-deps", "ci-runner", false, false, true},
		{"dependabot/ head ref alone is not a dependency-bot branch", "dependabot/go_modules/x", "alice", false, false, false},
		{"ordinary branch", "fix/thing", "alice", false, false, false},
		{"App-authored branch is never checked", "renovate/hive-lane", "hivecommons-hive[bot]", true, false, false},
		{"adopted branch is never checked", "renovate/go-deps", "renovate[bot]", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dependencyBotBranchRow(tc.headRef, tc.author, tc.appAuthored, tc.adopted); got != tc.want {
				t.Fatalf("dependencyBotBranchRow(%q, %q, %v, %v) = %v, want %v", tc.headRef, tc.author, tc.appAuthored, tc.adopted, got, tc.want)
			}
		})
	}
}

// depBotQueueFixture: unattributed same-repo rows (each defaults to scanner),
// heads pushed two hours ago unless noted, no claims —
//   - #1471 a mergeraptor Renovate branch, red and behind;
//   - #1472 a renovate/ branch opened under a non-bot account;
//   - #1473 an App-authored row on a renovate/ head ref (the hive's own);
//   - #1474 an adopted Renovate PR (stage 074: the hive owns the branch);
//   - #1475 an idle ordinary PR;
//   - #1476 an ordinary PR pushed five minutes ago (busy).
func depBotQueueFixture() string {
	ts := func(ago time.Duration) string { return busyTestNow.Add(-ago).Format(time.RFC3339) }
	old, recent := ts(2*time.Hour), ts(5*time.Minute)
	return fmt.Sprintf(`{"ci_failing":[
  {"number":1471,"repo":"test-org/utah","title":"chore(deps): update actions","author":"mergeraptor[bot]","head_ref":"renovate/human-merge-paths-actions",
   "head_sha":"a1","reachable_action":"push","mergeable_state":"behind","head_pushed_at":%q,"failing_checks":["contract"],"excerpt":"contract: schema mismatch"},
  {"number":1472,"repo":"test-org/utah","title":"chore(deps): update go deps","author":"ci-runner","head_ref":"renovate/go-deps",
   "head_sha":"a2","reachable_action":"push","head_pushed_at":%q,"failing_checks":["build"]},
  {"number":1473,"repo":"test-org/utah","title":"hive lane PR","author":"hivecommons-hive[bot]","app_authored":true,"head_ref":"renovate/hive-lane",
   "head_sha":"a3","reachable_action":"push","head_pushed_at":%q,"failing_checks":["test"]},
  {"number":1474,"repo":"test-org/utah","title":"adopted bump","author":"renovate[bot]","adopted":true,"head_ref":"renovate/adopted",
   "reachable_action":"push","head_pushed_at":%q,"failing_checks":["build"]},
  {"number":1475,"repo":"test-org/utah","title":"idle PR","author":"alice","head_ref":"fix/thing",
   "head_sha":"a5","reachable_action":"push","head_pushed_at":%q,"failing_checks":["lint"]},
  {"number":1476,"repo":"test-org/utah","title":"busy PR","author":"alice","head_ref":"fix/busy",
   "head_sha":"a6","reachable_action":"push","head_pushed_at":%q,"failing_checks":["lint"]}
]}`, old, old, old, old, old, recent)
}

func withDepBotQueue(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	orig, origNow := ciFailingPath, busyBranchNow
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	busyBranchNow = func() time.Time { return busyTestNow }
	t.Cleanup(func() { ciFailingPath, busyBranchNow = orig, origNow })
	t.Setenv(BusyBranchMinutesEnv, "")
	if err := os.WriteFile(ciFailingPath, []byte(depBotQueueFixture()), 0o644); err != nil {
		t.Fatal(err)
	}
	return newScheduler()
}

func TestFormatRedPRFixData_DependencyBotBranchesAreNotPushTargets(t *testing.T) {
	s := withDepBotQueue(t)
	out := s.redPRFixSection("scanner")

	busyIdx := strings.Index(out, "## ⏸ BUSY BRANCHES")
	depIdx := strings.Index(out, "## ⏸ DEPENDENCY-BOT BRANCHES - red PRs you must NOT push to (2)")
	if busyIdx < 0 || depIdx < 0 || depIdx < busyIdx {
		t.Fatalf("busy and dependency-bot rows must each be named in their own block after the gate:\n%s", out)
	}
	gate, busy, dep := out[:busyIdx], out[busyIdx:depIdx], out[depIdx:]
	if !strings.Contains(gate, "FIX-BEFORE-NEW") || !strings.Contains(gate, "(3)") {
		t.Fatalf("the push gate must hold exactly the App-authored, adopted and idle ordinary PRs:\n%s", out)
	}
	for _, want := range []string{"#1473 ", "#1474 ", "#1475 ", "ADOPTED contributor PR"} {
		if !strings.Contains(gate, want) {
			t.Errorf("gate missing %q:\n%s", want, out)
		}
	}
	for _, reject := range []string{"#1471 ", "#1472 "} {
		if strings.Contains(gate, reject) || strings.Contains(busy, reject) {
			t.Errorf("dependency-bot %s must never be listed as a push target or a busy branch:\n%s", reject, out)
		}
		if !strings.Contains(dep, reject) {
			t.Errorf("dependency-bot %s must be listed as comment-only:\n%s", reject, out)
		}
	}
	if !strings.Contains(busy, "#1476 ") || !strings.Contains(busy, "busy: head pushed 5m0s ago (busy window 1h0m0s) — suggest, do not push") {
		t.Errorf("the busy row must render exactly as before:\n%s", busy)
	}
	for _, want := range []string{
		dependencyBotBranchInstruction,
		"dependency bot: mergeraptor[bot] - comment only, do not push",
		"dependency bot: ci-runner - comment only, do not push",
		"failing: contract",
		"evidence: contract: schema mismatch",
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("dependency-bot block missing %q:\n%s", want, dep)
		}
	}
	// Still not a push target once the busy window is long past.
	busyBranchNow = func() time.Time { return busyTestNow.Add(24 * time.Hour) }
	later := s.redPRFixSection("scanner")
	if laterGate := later[:strings.Index(later, "## ⏸ DEPENDENCY-BOT BRANCHES")]; strings.Contains(laterGate, "#1471 ") || !strings.Contains(laterGate, "#1476 ") {
		t.Errorf("a day later the dependency-bot row stays out of the gate and the idle busy row is back in it:\n%s", later)
	}
}

// Only dependency-bot rows: they are listed, but no FIX-BEFORE-NEW gate is
// raised, since nothing the agent may push to must not block its new work.
func TestFormatRedPRFixData_OnlyDependencyBotRowsRaiseNoGate(t *testing.T) {
	data := `{"ci_failing":[{"number":5,"repo":"o/r","title":"t","author":"renovate[bot]","head_ref":"renovate/x","reachable_action":"push"}]}`
	out := formatRedPRFixDataGuarded([]byte(data), "scanner", nil, nil)
	if strings.Contains(out, "FIX-BEFORE-NEW") {
		t.Fatalf("a queue of only dependency-bot rows must not raise the gate:\n%s", out)
	}
	if !strings.Contains(out, "DEPENDENCY-BOT BRANCHES") || !strings.Contains(out, "#5 ") {
		t.Fatalf("the dependency-bot row must still be named:\n%s", out)
	}
}

func TestBuildCIFailingList_DependencyBotBranchesAreCommentOnly(t *testing.T) {
	s := withDepBotQueue(t)
	out := s.buildCIFailingListForAgent("scanner", nil)

	busyIdx := strings.Index(out, "BUSY BRANCHES (1 — another worker is on these branches; suggest the fix, do not push)")
	depIdx := strings.Index(out, "DEPENDENCY-BOT BRANCHES (2 - a dependency bot owns these branches; comment only, do not push or merge the base in)")
	if busyIdx < 0 || depIdx < 0 || depIdx < busyIdx {
		t.Fatalf("busy and dependency-bot sections missing or out of order:\n%s", out)
	}
	push, busy, dep := out[:busyIdx], out[busyIdx:depIdx], out[depIdx:]
	for _, want := range []string{"#1473 ", "#1474 ", "#1475 "} {
		if !strings.Contains(push, want) {
			t.Errorf("%s must stay in the push queue:\n%s", want, out)
		}
	}
	for _, reject := range []string{"#1471 ", "#1472 "} {
		if strings.Contains(push, reject) || strings.Contains(busy, reject) {
			t.Errorf("dependency-bot %s must never be in the push queue or the busy list:\n%s", reject, out)
		}
	}
	if !strings.Contains(push, "#1474 test-org/utah by @renovate[bot] (sha:) [ADOPTED dependency-bot branch: the hive owns this repair, push as listed]") {
		t.Errorf("the adopted dependency-bot row must say the hive owns its repair:\n%s", push)
	}
	if strings.Contains(push, "#1475 test-org/utah by @alice (sha:a5) [ADOPTED") {
		t.Errorf("an ordinary row carries no adoption marker:\n%s", push)
	}
	if !strings.Contains(busy, "#1476 test-org/utah [busy: head pushed 5m0s ago") {
		t.Errorf("the busy row must render exactly as before:\n%s", busy)
	}
	for _, want := range []string{
		"  #1471 test-org/utah (sha:a1) [dependency bot: mergeraptor[bot] - comment only, do not push] - chore(deps): update actions - failing: contract",
		"  #1472 test-org/utah (sha:a2) [dependency bot: ci-runner - comment only, do not push]",
		dependencyBotBranchInstruction,
	} {
		if !strings.Contains(dep, want) {
			t.Errorf("dependency-bot section missing %q:\n%s", want, dep)
		}
	}
}

// ${PR_LIST} carries the marker inline, next to the fork and busy markers,
// so an agent working the PR list learns from the list itself that a
// dependency bot's branch is not its to push to.
func TestFormatPRList_AnnotatesDependencyBotBranches(t *testing.T) {
	orig, origArmed := busyBranchNow, rebaseMergeArmed
	busyBranchNow = func() time.Time { return busyTestNow }
	// An operator armed "rebase, then merge as maintainer" on #1478: the
	// hive owns that rebase, as merge eligibility routes it.
	rebaseMergeArmed = func(repo string, number int) bool { return repo == "test-org/utah" && number == 1478 }
	t.Cleanup(func() { busyBranchNow, rebaseMergeArmed = orig, origArmed })
	t.Setenv(BusyBranchMinutesEnv, "")
	activity := func(ago time.Duration) *github.PRCIActivity {
		return &github.PRCIActivity{Head: "h", Checks: []github.PRActivityCheck{{Name: "dco", Head: "h", Since: busyPushedAt(ago)}}}
	}
	adopted := stagelabels.AdoptedByHive.Label()
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "utah", Number: 1471, Title: "bump", Author: "mergeraptor[bot]", HeadRef: "renovate/human-merge-paths-actions", HeadSHA: "h", MergeableState: "behind", CIActivity: activity(2 * time.Hour)},
		{Repo: "utah", Number: 1472, Title: "bump", Author: "ci-runner", HeadRef: "renovate/go-deps", HeadSHA: "h", CIActivity: activity(2 * time.Hour)},
		{Repo: "utah", Number: 1473, Title: "hive lane PR", Author: "hivecommons-hive[bot]", AppAuthored: true, HeadRef: "renovate/hive-lane", HeadSHA: "h"},
		{Repo: "utah", Number: 1474, Title: "adopted bump", Author: "renovate[bot]", HeadRef: "renovate/adopted", Labels: []string{adopted}, HeadSHA: "h"},
		{Repo: "utah", Number: 1475, Title: "fork bump", Author: "renovate[bot]", HeadRef: "renovate/x", FromFork: true, HeadRepo: "someone/utah", HeadSHA: "h"},
		{Repo: "utah", Number: 1476, Title: "busy PR", Author: "alice", HeadRef: "fix/busy", HeadSHA: "h", CIActivity: activity(5 * time.Minute)},
		{Repo: "utah", Number: 1477, Title: "idle PR", Author: "alice", HeadRef: "fix/thing", HeadSHA: "h", CIActivity: activity(3 * time.Hour)},
		{Repo: "utah", Number: 1478, Title: "armed bump", Author: "renovate[bot]", HeadRef: "renovate/armed", HeadSHA: "h", CIActivity: activity(3 * time.Hour)},
	}}}
	s := newScheduler()
	s.cfg.Project.Org = "test-org"
	out, _ := s.formatPRListWithPolicyForAgent(actionable, "scanner")
	lines := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		for n := 1471; n <= 1478; n++ {
			if strings.Contains(line, fmt.Sprintf("#%d ", n)) {
				lines[n] = line
			}
		}
	}
	for _, n := range []int{1471, 1472} {
		if !strings.Contains(lines[n], dependencyBotMarker) || strings.Contains(lines[n], "[busy:") {
			t.Errorf("#%d must carry the dependency-bot marker and no busy marker: %q", n, lines[n])
		}
	}
	for _, n := range []int{1473, 1474, 1475, 1476, 1477, 1478} {
		if lines[n] == "" || strings.Contains(lines[n], "[dependency bot") {
			t.Errorf("#%d must be listed without a dependency-bot marker: %q\n%s", n, lines[n], out)
		}
	}
	if !strings.Contains(lines[1475], forkAnnotation(actionable.PRs.Items[4])) {
		t.Errorf("the fork row keeps its fork marker: %q", lines[1475])
	}
	if !strings.Contains(lines[1476], "[busy: head pushed 5m0s ago") {
		t.Errorf("the busy row keeps its busy marker: %q", lines[1476])
	}
	if strings.Contains(lines[1477], "[busy:") || strings.Contains(lines[1477], "[fork:") {
		t.Errorf("an idle ordinary row carries no do-not-push marker: %q", lines[1477])
	}
}

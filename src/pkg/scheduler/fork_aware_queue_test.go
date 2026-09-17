package scheduler

// hivecommons/hive#7386: the repair queue had no fork awareness. On the
// projectbluefin spoke 66 of 107 CI-failing PRs had their head in a fork the
// agent cannot push to; nothing in the kick said so, the scanner discovered
// it by pushing, and the push created a stray branch on the base repo under
// the fork's head-ref name. These tests pin the three consumer changes: the
// FIX-BEFORE-NEW gate never lists a fork PR as "yours", CI_FAILING splits
// fork PRs into a comment-only section, and every PR list annotates them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

const forkQueueFixture = `{"generated_at":"2026-09-17T00:00:00Z","ci_failing":[
  {"number":839,"repo":"projectbluefin/testsuite","title":"fork: dashboard","author":"alice","head_sha":"f1",
   "head_ref":"sec-check-dashboard","head_repo":"alice/testsuite","from_fork":true,"reachable_action":"comment-only",
   "failing_checks":["build"]},
  {"number":840,"repo":"projectbluefin/testsuite","title":"hive fix","author":"hive[bot]","head_sha":"f2",
   "head_ref":"hive/fix-840","head_repo":"projectbluefin/testsuite","reachable_action":"push","agent":"scanner",
   "failing_checks":["build"]},
  {"number":841,"repo":"projectbluefin/testsuite","title":"fork deleted","author":"bob","head_sha":"f3",
   "head_ref":"gone","head_repo":"","from_fork":true,"reachable_action":"comment-only"},
  {"number":842,"repo":"projectbluefin/testsuite","title":"contributor same-repo","author":"carol","head_sha":"f4",
   "head_ref":"carol/fix","head_repo":"projectbluefin/testsuite","reachable_action":"push"}
]}`

// An unattributed fork PR used to default to scanner's FIX-BEFORE-NEW gate —
// the compounding factor: the directive ordered repairs on PRs that cannot be
// repaired. Fork PRs are never "yours", whatever the attribution.
func TestFormatRedPRFixData_ForkPRsNeverYours(t *testing.T) {
	out := formatRedPRFixData([]byte(forkQueueFixture), "scanner")
	if !strings.Contains(out, "#840 ") || !strings.Contains(out, "#842 ") {
		t.Errorf("same-repo red PRs (attributed and unattributed) must be listed:\n%s", out)
	}
	for _, reject := range []string{"#839 ", "#841 "} {
		if strings.Contains(out, reject) {
			t.Errorf("fork PR %s must never appear in FIX-BEFORE-NEW:\n%s", reject, out)
		}
	}
	if !strings.Contains(out, "2 red PR(s) from forks are NOT listed") {
		t.Errorf("the block must say fork PRs were withheld and why:\n%s", out)
	}
	if !strings.Contains(out, "(2)") {
		t.Errorf("header count must exclude fork PRs:\n%s", out)
	}

	// Only fork PRs red: no gate at all — nothing the agent can push to.
	onlyForks := `{"ci_failing":[{"number":1,"repo":"o/r","title":"t","from_fork":true,"head_repo":"x/r"}]}`
	if got := formatRedPRFixData([]byte(onlyForks), "scanner"); got != "" {
		t.Errorf("a queue of only fork PRs must not raise a FIX-BEFORE-NEW gate, got:\n%s", got)
	}
}

// CI_FAILING renders two queues: push-repairable first, then a clearly
// separate fork section with the head repo and the do-not-push warning.
func TestBuildCIFailingList_SplitsForkPRs(t *testing.T) {
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if err := os.WriteFile(ciFailingPath, []byte(forkQueueFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newScheduler()
	out := s.buildCIFailingList()

	forkIdx := strings.Index(out, "FORK PRs (2 — review/comment only, you CANNOT push to these)")
	if forkIdx < 0 {
		t.Fatalf("fork section missing:\n%s", out)
	}
	push := out[:forkIdx]
	forks := out[forkIdx:]
	for _, want := range []string{"#840 ", "#842 "} {
		if !strings.Contains(push, want) {
			t.Errorf("push-repairable section missing %s:\n%s", want, push)
		}
	}
	for _, reject := range []string{"#839 ", "#841 "} {
		if strings.Contains(push, reject) {
			t.Errorf("fork PR %s leaked into the push-repairable section:\n%s", reject, push)
		}
	}
	for _, want := range []string{
		"#839 projectbluefin/testsuite by @alice [fork: alice/testsuite:sec-check-dashboard — comment only]",
		"#841 projectbluefin/testsuite by @bob [fork: (fork deleted):gone — comment only]",
		"NEVER `git push origin HEAD:<head_ref>`",
		"stray branch",
	} {
		if !strings.Contains(forks, want) {
			t.Errorf("fork section missing %q:\n%s", want, forks)
		}
	}

	// Only fork PRs: the push queue says so explicitly instead of looking
	// empty-by-accident.
	onlyForks := `{"ci_failing":[{"number":1,"repo":"o/r","title":"t","author":"a","from_fork":true,"head_repo":"x/r","head_ref":"b"}]}`
	if err := os.WriteFile(ciFailingPath, []byte(onlyForks), 0o644); err != nil {
		t.Fatal(err)
	}
	out = s.buildCIFailingList()
	if !strings.Contains(out, "(none you can push to)") || !strings.Contains(out, "FORK PRs (1") {
		t.Errorf("all-fork queue must render an empty push queue plus the fork section:\n%s", out)
	}
}

// ${PR_LIST} and the scanner fallback annotate fork PRs inline with the head
// repository, so "comment only" is known before any checkout or push.
func TestPRLists_AnnotateForkPRs(t *testing.T) {
	s := newScheduler()
	prs := []github.PullRequest{
		{Repo: "projectbluefin/testsuite", Number: 839, Title: "fork: dashboard", Author: "alice", FromFork: true, HeadRepo: "alice/testsuite", HeadRef: "sec-check-dashboard"},
		{Repo: "projectbluefin/testsuite", Number: 840, Title: "hive fix", Author: "hive[bot]", HeadRepo: "projectbluefin/testsuite"},
		{Repo: "projectbluefin/testsuite", Number: 841, Title: "fork deleted", Author: "bob", FromFork: true},
	}
	actionable := &github.ActionableResult{PRs: github.PRResult{Count: 3, Items: prs}}

	for name, out := range map[string]string{
		"PR_LIST":          s.formatPRList(actionable),
		"scanner fallback": s.buildScannerMessage(nil, actionable),
	} {
		if !strings.Contains(out, "#839 by @alice [fork: alice/testsuite — comment only, cannot push] fork: dashboard") {
			t.Errorf("%s: fork PR not annotated:\n%s", name, out)
		}
		if !strings.Contains(out, "#841 by @bob [fork: fork deleted — comment only, cannot push]") {
			t.Errorf("%s: deleted-fork PR not annotated:\n%s", name, out)
		}
		if strings.Contains(out, "#840 by @hive[bot] [fork") {
			t.Errorf("%s: same-repo PR wrongly annotated:\n%s", name, out)
		}
	}
}

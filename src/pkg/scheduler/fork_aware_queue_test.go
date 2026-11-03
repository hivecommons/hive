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

	forkIdx := strings.Index(out, "FORK PRs (2 — review/comment only, do not push unless an explicit contributor-PR gate says this hive may)")
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
		"#839 projectbluefin/testsuite by @alice [fork: alice/testsuite:sec-check-dashboard — comment/check-report only]",
		"#841 projectbluefin/testsuite by @bob [fork: (fork deleted):gone — comment/check-report only]",
		"same-named branch on the base repo",
		"review.contributor_prs.base_sync",
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
		if !strings.Contains(out, "#839 by @alice [fork: alice/testsuite — comment only, cannot push]") {
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

// Stage 074 in-place adoption (hivecommons/hive#12527): a fork PR the hive
// adopted IS listed — maintainer_can_modify lets the fixer push to the
// contributor's branch — with a line saying so; other fork PRs stay withheld.
func TestFormatRedPRFixData_AdoptedForkPRIsYours(t *testing.T) {
	fixture := `{"ci_failing":[
  {"number":12527,"repo":"o/r","title":"adopted","author":"karibhou","head_repo":"karibhou/r","from_fork":true,"adopted":true,"reachable_action":"push","agent":"scanner","conflict":true,"mergeable_state":"dirty"},
  {"number":839,"repo":"o/r","title":"fork","author":"alice","head_repo":"alice/r","from_fork":true,"reachable_action":"comment-only","failing_checks":["build"]}
]}`
	out := formatRedPRFixData([]byte(fixture), "scanner")
	if !strings.Contains(out, "#12527 ") || !strings.Contains(out, "ADOPTED contributor PR (stage 074, head repo karibhou/r)") {
		t.Errorf("adopted fork PR must be listed with the adoption note:\n%s", out)
	}
	if strings.Contains(out, "#839 ") || !strings.Contains(out, "1 red PR(s) from forks are NOT listed") {
		t.Errorf("non-adopted fork PR must stay withheld:\n%s", out)
	}
}

// reachable_action describes token capability: since #12899 a fork that
// allows maintainer edits (GitHub's default for contributor PRs) reports
// push. The shared CI_FAILING queue must still gate on policy, like
// FIX-BEFORE-NEW does: only an adopted (stage 074) fork PR is pushable; every
// other fork row stays in the comment-only section with its head repo named,
// whatever its reachable_action says.
func TestBuildCIFailingList_NonAdoptedForkWithMaintainerEditsStaysCommentOnly(t *testing.T) {
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	fixture := `{"ci_failing":[
  {"number":841,"repo":"projectbluefin/testsuite","title":"contributor fork, maintainer edits on","author":"arteeh","head_sha":"f3",
   "head_ref":"main","head_repo":"arteeh/testsuite","from_fork":true,"reachable_action":"push","failing_checks":["signoff"]},
  {"number":12527,"repo":"projectbluefin/testsuite","title":"adopted","author":"karibhou","head_sha":"f5",
   "head_ref":"fix","head_repo":"karibhou/testsuite","from_fork":true,"adopted":true,"reachable_action":"push","failing_checks":["build"]},
  {"number":12528,"repo":"projectbluefin/testsuite","title":"adopted but fork edits off","author":"dana","head_sha":"f6",
   "head_ref":"fix2","head_repo":"dana/testsuite","from_fork":true,"adopted":true,"reachable_action":"comment-only","failing_checks":["build"]},
  {"number":840,"repo":"projectbluefin/testsuite","title":"hive fix","author":"hive[bot]","head_sha":"f2",
   "head_ref":"hive/fix-840","head_repo":"projectbluefin/testsuite","reachable_action":"push","failing_checks":["build"]}
]}`
	if err := os.WriteFile(ciFailingPath, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := newScheduler().buildCIFailingList()

	forkIdx := strings.Index(out, "FORK PRs (2 — review/comment only")
	if forkIdx < 0 {
		t.Fatalf("fork section missing or wrong count:\n%s", out)
	}
	push, forks := out[:forkIdx], out[forkIdx:]
	if strings.Contains(push, "#841 ") || strings.Contains(push, "#12528 ") {
		t.Errorf("non-adopted or unreachable fork PR leaked into the push-repairable section:\n%s", push)
	}
	if !strings.Contains(forks, "#841 projectbluefin/testsuite by @arteeh [fork: arteeh/testsuite:main — comment/check-report only]") {
		t.Errorf("non-adopted fork PR must be listed comment-only with its head repo:\n%s", forks)
	}
	if !strings.Contains(forks, "#12528 ") {
		t.Errorf("adopted fork without maintainer edits is not pushable:\n%s", forks)
	}
	if !strings.Contains(push, "#840 ") {
		t.Errorf("same-repo hive PR must stay pushable:\n%s", push)
	}
	if !strings.Contains(push, "#12527 projectbluefin/testsuite by @karibhou (sha:f5) [adopted fork: karibhou/testsuite:fix — push to the contributor's branch, never to a same-named base branch]") {
		t.Errorf("adopted fork PR must be pushable and name the fork head:\n%s", push)
	}
	for _, want := range []string{"rebase onto the base", "preserve original authors and Signed-off-by", "--force-with-lease against the observed head", "never overwrite their new work"} {
		if !strings.Contains(push, want) {
			t.Errorf("adopted fork rebase instruction missing %q:\n%s", want, push)
		}
	}
}

// An adopted (stage 074) contributor PR is the hive's to repair: its row is
// listed under FIX-BEFORE-NEW with push instructions to the PR's own branch,
// and a still-present sentinel alert is named as a label to leave alone. A
// contributor fork PR the hive did not adopt is never listed as push work.
func TestFormatRedPRFixData_AdoptedPRListedNonAdoptedForkIsNot(t *testing.T) {
	fixture := `{"ci_failing":[
	 {"number":13251,"repo":"hivecommons/hive","title":"reviewer confidence","agent":"scanner","from_fork":true,"reachable_action":"push","adopted":true,"head_repo":"arteeh/hive","conflict":true,"mergeable_state":"dirty"},
	 {"number":13252,"repo":"hivecommons/hive","title":"contribute-watch","agent":"scanner","from_fork":true,"reachable_action":"push","adopted":true,"sentinel_alert":true,"head_repo":"mendezr/hive","failing_checks":["test (rest 5/5)","coverage"]},
	 {"number":13213,"repo":"hivecommons/hive","title":"not adopted","agent":"scanner","from_fork":true,"reachable_action":"comment-only","head_repo":"hanthor/hive","failing_checks":["test"]}
	]}`
	out := formatRedPRFixData([]byte(fixture), "scanner")
	for _, want := range []string{
		"#13251 hivecommons/hive",
		"conflict: mergeable_state=dirty — merge the base branch or rebase the PR branch",
		"#13252 hivecommons/hive",
		"ADOPTED contributor PR (stage 074, head repo mendezr/hive)",
		adoptedSentinelNote,
		"failing: test (rest 5/5), coverage",
		"(1 red PR(s) from forks are NOT listed here",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fix block missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#13213") {
		t.Errorf("non-adopted fork PR listed as push work:\n%s", out)
	}
}

// hivecommons/hive#15103: the conflict kick tells the fixer how to
// rebase-resolve, which attempt this is, and when the hive stops.
func TestRebaseResolveInstructions(t *testing.T) {
	first := rebaseResolveInstructions("v5", 1, 2, false)
	for _, want := range []string{"attempt 1/2", "git rebase origin/v5", "--force-with-lease", "OWN branch", "Do not open a new PR"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first attempt missing %q: %s", want, first)
		}
	}
	if strings.Contains(first, "Last attempt") || strings.Contains(first, "trusted author") {
		t.Fatalf("first attempt over-explains: %s", first)
	}
	last := rebaseResolveInstructions("", 2, 2, true)
	for _, want := range []string{"attempt 2/2", "git rebase the base branch", "Last attempt", "trusted author's PR"} {
		if !strings.Contains(last, want) {
			t.Fatalf("last attempt missing %q: %s", want, last)
		}
	}
}

package scheduler

import (
	"os"
	"strings"
	"testing"
)

// heldRedPRFixture: four red PRs, all owned by agents that can push. Two carry
// the hold the ACMM level gate applies at PR creation.
const heldRedPRFixture = `{"generated_at":"2026-09-17T19:17:30Z","ci_failing":[
  {"number":188,"repo":"test-org/zfs-kinoite-complex","title":"fix: git diff gate","agent":"sec-check",
   "held":true,"failing_checks":["Python Unit Tests"],
   "excerpt":"tests/test_git_diff_gate.py::test_gate FAILED"},
  {"number":22,"repo":"test-org/console","title":"ordinary red","agent":"sec-check","failing_checks":["build-gate"]},
  {"number":33,"repo":"test-org/console","title":"held and escalated","agent":"sec-check","held":true,"escalated":true},
  {"number":44,"repo":"test-org/console","title":"held outreach PR","agent":"outreach","held":true,"failing_checks":["link-check"]}
]}`

// The bug (hivecommons/hive#7438): a level-held red PR never reached its
// authoring agent, so it could not be repaired, so it stayed red, so it stayed
// held. It must now render in the owning agent's block — carrying the note
// that keeps the repair from turning into a release.
func TestFormatRedPRFixData_HeldPRIsListedWithKeepTheHoldNote(t *testing.T) {
	got := formatRedPRFixData([]byte(heldRedPRFixture), "sec-check")

	if !strings.Contains(got, "#188 test-org/zfs-kinoite-complex") {
		t.Fatalf("held red PR missing from its author's fix block:\n%s", got)
	}
	if !strings.Contains(got, heldRedPRNote) {
		t.Errorf("held entry missing %q:\n%s", heldRedPRNote, got)
	}
	if !strings.Contains(got, "Python Unit Tests") || !strings.Contains(got, "test_git_diff_gate.py") {
		t.Errorf("held entry must carry the same CI evidence as any red PR:\n%s", got)
	}

	// The note belongs to the held entry only, and the unheld PR is unchanged.
	if !strings.Contains(got, "#22 test-org/console") {
		t.Errorf("unheld red PR must still be listed:\n%s", got)
	}
	if n := strings.Count(got, heldRedPRNote); n != 1 {
		t.Errorf("held note appears %d times, want 1 — only #188 is held for this agent:\n%s", n, got)
	}
}

// Escalation still wins: a needs-human PR belongs to a human whether or not it
// is also held.
func TestFormatRedPRFixData_HeldAndEscalatedStaysExcluded(t *testing.T) {
	got := formatRedPRFixData([]byte(heldRedPRFixture), "sec-check")
	if strings.Contains(got, "#33 ") {
		t.Errorf("escalated PR must never be listed, held or not:\n%s", got)
	}
}

// Outreach is the documented exception: the level-hold comment promises a
// human reviews every outreach PR, so a held outreach PR is already someone's
// reading material and must not be edited underneath them. An outreach PR with
// NO hold is still ordinary repair work.
func TestFormatRedPRFixData_OutreachHeldPRIsExcludedButUnheldIsNot(t *testing.T) {
	if got := formatRedPRFixData([]byte(heldRedPRFixture), "outreach"); got != "" {
		t.Errorf("outreach must not be asked to repair its held PR, got:\n%s", got)
	}

	unheldOutreach := `{"ci_failing":[
  {"number":44,"repo":"test-org/console","title":"outreach red","agent":"outreach","failing_checks":["link-check"]}
]}`
	got := formatRedPRFixData([]byte(unheldOutreach), "outreach")
	if !strings.Contains(got, "#44 test-org/console") {
		t.Errorf("an outreach PR with no hold is ordinary repair work:\n%s", got)
	}
	if strings.Contains(got, heldRedPRNote) {
		t.Errorf("unheld entry must not carry the held note:\n%s", got)
	}
}

// Held PRs ride ci-failing.json for their AUTHOR's fix-before-new block, but
// the shared CI_FAILING work list excludes them (merged #7440 semantics): any
// other agent reading their presence could take repairing a held PR as
// permission to touch it. The list carries a summary line saying how many
// were withheld and why.
func TestBuildCIFailingList_MarksHeldPRs(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	payload := `{"ci_failing":[
  {"number":188,"repo":"test-org/zfs-kinoite-complex","title":"held red","author":"a","head_sha":"deadbeef","held":true},
  {"number":22,"repo":"test-org/console","title":"ordinary red","author":"a","head_sha":"cafe"}
]}`
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	got := s.buildCIFailingList()
	if strings.Contains(got, "#188") {
		t.Errorf("held PR must stay out of the shared repair list:\n%s", got)
	}
	if !strings.Contains(got, "#22 test-org/console") {
		t.Errorf("ordinary red PR must be listed:\n%s", got)
	}
	if !strings.Contains(got, "1 held red PR(s) are not listed") {
		t.Errorf("list must say a held PR was withheld and why:\n%s", got)
	}
}

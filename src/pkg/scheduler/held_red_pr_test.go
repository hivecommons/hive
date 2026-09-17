package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hivecommons/hive#7438: a level-held agent PR with a failing check used to
// vanish from ci-failing.json, so its author never got a fix-before-new block
// for it and a human ended up doing the agent's repair. The hold is a merge
// checkpoint, not a repair checkpoint: a held red PR is listed in its OWNING
// agent's block with an explicit "do not remove the hold" note, escalated
// rows stay out, outreach's held rows stay out (a human may be reading
// them), and the shared CI_FAILING queue never offers a held PR to anyone
// else.

const heldRedFixture = `{"generated_at":"2026-09-17T19:17:30Z","ci_failing":[
  {"number":188,"repo":"Danathar/zfs-kinoite-complex","title":"sec-check: git diff gate","agent":"sec-check",
   "failing_checks":["Python Unit Tests"],"excerpt":"tests/test_git_diff_gate.py::test_gate FAILED","held":true},
  {"number":190,"repo":"Danathar/zfs-kinoite-complex","title":"sec-check: unheld red","agent":"sec-check",
   "failing_checks":["lint"]},
  {"number":191,"repo":"Danathar/zfs-kinoite-complex","title":"escalated held","agent":"sec-check",
   "failing_checks":["lint"],"held":true,"escalated":true},
  {"number":192,"repo":"Danathar/zfs-kinoite-complex","title":"outreach held","agent":"outreach",
   "failing_checks":["lint"],"held":true},
  {"number":193,"repo":"Danathar/zfs-kinoite-complex","title":"outreach unheld","agent":"outreach",
   "failing_checks":["lint"]}
]}`

func TestFormatRedPRFixData_HeldPRsGoBackToTheirAuthor(t *testing.T) {
	out := formatRedPRFixData([]byte(heldRedFixture), "sec-check")
	if !strings.Contains(out, "#188 Danathar/zfs-kinoite-complex") {
		t.Fatalf("held red PR #188 missing from its author's fix block (#7438):\n%s", out)
	}
	if !strings.Contains(out, heldRedPRNote) {
		t.Errorf("held PR entry lacks the %q note:\n%s", heldRedPRNote, out)
	}
	// The note is attached to the held entry, not the unheld one.
	held := strings.Index(out, "#188 ")
	unheld := strings.Index(out, "#190 ")
	note := strings.Index(out, heldRedPRNote)
	if held < 0 || unheld < 0 || note < held || note > unheld {
		t.Errorf("held note is not directly under #188 (held=%d note=%d unheld=%d):\n%s", held, note, unheld, out)
	}
	if strings.Count(out, heldRedPRNote) != 1 {
		t.Errorf("held note appears %d times, want once (only #188 is held):\n%s", strings.Count(out, heldRedPRNote), out)
	}
	if strings.Contains(out, "#191 ") {
		t.Error("an escalated PR must stay out of the fix block even when held")
	}
	if !strings.Contains(out, "FIX-BEFORE-NEW — your open PRs with failing CI (2)") {
		t.Errorf("count should be 2 (held #188 + unheld #190):\n%s", out)
	}

	outreach := formatRedPRFixData([]byte(heldRedFixture), "outreach")
	if strings.Contains(outreach, "#192 ") {
		t.Errorf("outreach's held PR must not be routed back for repair (a human may be reading it):\n%s", outreach)
	}
	if !strings.Contains(outreach, "#193 ") {
		t.Errorf("outreach's UNHELD red PR is still its own to fix:\n%s", outreach)
	}
}

func TestBuildCIFailingList_HeldPRsAreNotOfferedToOtherAgents(t *testing.T) {
	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if err := os.WriteFile(ciFailingPath, []byte(heldRedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newScheduler()
	out := s.buildCIFailingList()
	for _, held := range []string{"#188 ", "#191 ", "#192 "} {
		if strings.Contains(out, held) {
			t.Errorf("held PR %s offered in the shared CI_FAILING queue; every policy says never touch a held item:\n%s", strings.TrimSpace(held), out)
		}
	}
	for _, unheld := range []string{"#190 ", "#193 "} {
		if !strings.Contains(out, unheld) {
			t.Errorf("unheld red PR %s missing from the shared queue:\n%s", strings.TrimSpace(unheld), out)
		}
	}
	if !strings.Contains(out, "3 held red PR(s) are not listed") {
		t.Errorf("the queue does not say how many held PRs it withheld:\n%s", out)
	}

	// Only held rows: the queue says so rather than pretending to be empty.
	onlyHeld := `{"ci_failing":[{"number":1,"repo":"o/r","title":"t","agent":"scanner","held":true}]}`
	if err := os.WriteFile(ciFailingPath, []byte(onlyHeld), 0o644); err != nil {
		t.Fatal(err)
	}
	out = s.buildCIFailingList()
	if !strings.Contains(out, "(none you can push to)") || !strings.Contains(out, "1 held red PR(s) are not listed") {
		t.Errorf("only-held queue rendered wrong:\n%s", out)
	}
}

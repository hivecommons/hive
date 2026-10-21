package scheduler

import (
	"os"
	"strings"
	"testing"
)

// hivecommons/hive 2026-10-20: v5's Docker build was red, so #13258's three
// fix attempts re-pushed against a red baseline and escalated. A row whose
// every failing check is red on the base branch carries base_red in
// ci-failing.json and is never a repair target; a mixed row stays one.
const baseRedFixture = `{"ci_failing":[
  {"number":13258,"repo":"hivecommons/hive","title":"base red","author":"a","head_sha":"b1","agent":"scanner",
   "failing_checks":["docker (hive)","overlayfs-exec-guard"],"base_red_checks":["docker (hive)","overlayfs-exec-guard"],"base_red":true},
  {"number":13260,"repo":"hivecommons/hive","title":"mixed","author":"a","head_sha":"m1","agent":"scanner",
   "failing_checks":["docker (hive)","lint"],"base_red_checks":["docker (hive)"],"excerpt":"lint: unused var"}
]}`

func TestFormatRedPRFixData_DropsBaseRedPRs(t *testing.T) {
	out := formatRedPRFixData([]byte(baseRedFixture), "scanner")
	if !strings.Contains(out, "FIX-BEFORE-NEW") || !strings.Contains(out, "#13260 hivecommons/hive — mixed") {
		t.Fatalf("a PR with a PR-local failure must be listed for repair:\n%s", out)
	}
	if strings.Contains(out, "#13258 hivecommons/hive —") {
		t.Errorf("base-red PR listed as a repair target:\n%s", out)
	}
	if !strings.Contains(out, "hivecommons/hive#13258 (docker (hive), overlayfs-exec-guard)") || !strings.Contains(out, "red on the base branch") {
		t.Errorf("base-red PR must be named with its checks and why it is skipped:\n%s", out)
	}
}

func TestFormatRedPRFixData_NoBannerWhenEveryPRBaseRed(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":1,"repo":"o/r","title":"a","agent":"scanner","failing_checks":["build"],"base_red_checks":["build"],"base_red":true}
	]}`
	if got := formatRedPRFixData([]byte(data), "scanner"); got != "" {
		t.Errorf("a fleet red only on the base branch must emit no FIX-BEFORE-NEW block, got:\n%s", got)
	}
}

func TestBuildCIFailingList_SeparatesBaseRedPRs(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	if err := os.WriteFile(path, []byte(baseRedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := s.buildCIFailingList()
	head, baseSection, ok := strings.Cut(out, "RED ONLY BECAUSE THE BASE BRANCH IS RED (1")
	if !ok {
		t.Fatalf("base-red PRs need their own heading:\n%s", out)
	}
	if !strings.Contains(head, "#13260 hivecommons/hive by @a (sha:m1) — mixed") {
		t.Errorf("mixed PR belongs in the repair queue:\n%s", out)
	}
	if strings.Contains(head, "#13258 ") {
		t.Errorf("base-red PR listed in the repair queue:\n%s", out)
	}
	if !strings.Contains(baseSection, "#13258 hivecommons/hive — base branch red on docker (hive), overlayfs-exec-guard") {
		t.Errorf("base-red PR missing from its section:\n%s", out)
	}
}

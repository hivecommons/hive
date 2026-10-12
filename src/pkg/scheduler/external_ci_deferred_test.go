package scheduler

import (
	"os"
	"strings"
	"testing"
)

// A red PR whose failure an agent already traced to the upstream repository
// at its current head carries deferred_external in ci-failing.json
// (hivecommons/hive#11609). The governor sets it only while the markers match
// the head and cover every failing check, so #66 is deferred and #67, pushed
// since, is an ordinary red row again.
const externalCIDeferredFixture = `{"ci_failing":[
  {"number":66,"repo":"o/akmods","title":"chore: sync with upstream","author":"a","head_sha":"54ff622","agent":"scanner",
   "failing_checks":["Check ogc-lts (44)"],"excerpt":"stale mirror","deferred_external":true},
  {"number":67,"repo":"o/akmods","title":"pushed since","author":"a","head_sha":"e1f3964","agent":"scanner",
   "failing_checks":["Check ogc-lts (44)"],"excerpt":"real failure"}
]}`

func TestFormatRedPRFixData_DropsExternalDeferredPRs(t *testing.T) {
	out := formatRedPRFixData([]byte(externalCIDeferredFixture), "scanner")
	if !strings.Contains(out, "#67 o/akmods — pushed since") {
		t.Fatalf("a PR without a current external triage must be listed for repair:\n%s", out)
	}
	if strings.Contains(out, "#66 o/akmods —") || strings.Contains(out, "stale mirror") {
		t.Errorf("external-deferred PR listed as a repair target:\n%s", out)
	}
	if !strings.Contains(out, "triaged as failing upstream at their current head") || !strings.Contains(out, "o/akmods#66") {
		t.Errorf("external-deferred PR must be named as deferred:\n%s", out)
	}

	only := `{"ci_failing":[{"number":66,"repo":"o/akmods","title":"t","agent":"scanner","failing_checks":["build"],"deferred_external":true}]}`
	if got := formatRedPRFixData([]byte(only), "scanner"); got != "" {
		t.Errorf("every red PR deferred as external must emit no FIX-BEFORE-NEW block, got:\n%s", got)
	}
}

func TestBuildCIFailingList_SeparatesExternalDeferredPRs(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	if err := os.WriteFile(path, []byte(externalCIDeferredFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := s.buildCIFailingList()
	head, external, ok := strings.Cut(out, "DEFERRED AS FAILING UPSTREAM (1")
	if !ok {
		t.Fatalf("external-deferred PRs need their own heading:\n%s", out)
	}
	if !strings.Contains(head, "#67 o/akmods by @a (sha:e1f3964) — pushed since") {
		t.Errorf("a PR without a current external triage belongs in the repair queue:\n%s", out)
	}
	if strings.Contains(head, "#66 ") {
		t.Errorf("external-deferred PR listed in the repair queue:\n%s", out)
	}
	if !strings.Contains(external, "#66 o/akmods (sha:54ff622) — chore: sync with upstream — failing: Check ogc-lts (44)") {
		t.Errorf("external-deferred PR missing from its section:\n%s", out)
	}
}

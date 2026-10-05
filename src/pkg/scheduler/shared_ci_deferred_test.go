package scheduler

import (
	"os"
	"strings"
	"testing"
)

// A red PR the fix lane already deferred to a still-open shared-CI incident
// carries deferred_incident in ci-failing.json (hivecommons/hive#10528). The
// governor sets it only while the incident is open; once the incident closes
// the row comes back without it. #10511 is deferred to open incident #10512;
// #10475 was deferred to an incident that has since closed, so it is an
// ordinary red row again.
const sharedCIDeferredFixture = `{"ci_failing":[
  {"number":10511,"repo":"hivecommons/hive","title":"deferred red","author":"a","head_sha":"d1","agent":"scanner",
   "failing_checks":["build-and-test"],"excerpt":"runner lost","deferred_incident":10512},
  {"number":10514,"repo":"hivecommons/hive","title":"deferred red conflict","author":"a","head_sha":"d2","agent":"scanner",
   "failing_checks":["build-and-test"],"conflict":true,"mergeable_state":"dirty","deferred_incident":10512},
  {"number":10475,"repo":"hivecommons/hive","title":"incident closed","author":"a","head_sha":"c1","agent":"scanner",
   "failing_checks":["build-and-test"],"excerpt":"real failure"}
]}`

func TestFormatRedPRFixData_DropsOpenIncidentDeferredPRs(t *testing.T) {
	out := formatRedPRFixData([]byte(sharedCIDeferredFixture), "scanner")
	if !strings.Contains(out, "FIX-BEFORE-NEW") || !strings.Contains(out, "#10475 hivecommons/hive — incident closed") {
		t.Fatalf("a PR whose incident closed must be listed for repair:\n%s", out)
	}
	for _, deferred := range []string{"#10511 hivecommons/hive —", "#10514 hivecommons/hive —", "runner lost"} {
		if strings.Contains(out, deferred) {
			t.Errorf("open-incident deferred PR listed as a repair target (%q):\n%s", deferred, out)
		}
	}
	for _, note := range []string{"hivecommons/hive#10511 → incident #10512", "hivecommons/hive#10514 → incident #10512"} {
		if !strings.Contains(out, note) {
			t.Errorf("deferred PR must be named with its incident (%q):\n%s", note, out)
		}
	}
}

func TestFormatRedPRFixData_NoBannerWhenEveryPRDeferred(t *testing.T) {
	data := `{"ci_failing":[
	  {"number":1,"repo":"o/r","title":"a","agent":"scanner","failing_checks":["build"],"deferred_incident":9},
	  {"number":2,"repo":"o/r","title":"b","agent":"scanner","failing_checks":["build"],"deferred_incident":9}
	]}`
	if got := formatRedPRFixData([]byte(data), "scanner"); got != "" {
		t.Errorf("every red PR deferred to an open incident must emit no FIX-BEFORE-NEW block, got:\n%s", got)
	}
}

func TestBuildCIFailingList_SeparatesOpenIncidentDeferredPRs(t *testing.T) {
	s := newCIFailingScheduler(t)
	path := overrideCIFailingPath(t)
	if err := os.WriteFile(path, []byte(sharedCIDeferredFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	out := s.buildCIFailingList()
	head, deferredSection, ok := strings.Cut(out, "DEFERRED TO OPEN SHARED-CI INCIDENTS (2")
	if !ok {
		t.Fatalf("deferred PRs need their own heading:\n%s", out)
	}
	if !strings.Contains(head, "#10475 hivecommons/hive by @a (sha:c1) — incident closed") {
		t.Errorf("a PR whose incident closed belongs in the repair queue:\n%s", out)
	}
	for _, n := range []string{"#10511 ", "#10514 "} {
		if strings.Contains(head, n) {
			t.Errorf("open-incident deferred PR %s listed in the repair queue:\n%s", n, out)
		}
		if !strings.Contains(deferredSection, n+"hivecommons/hive — deferred to incident #10512") {
			t.Errorf("deferred PR %s missing from the deferred section:\n%s", n, out)
		}
	}
}

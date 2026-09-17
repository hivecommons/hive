package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// stateFileForTest redirects fleet-report state to a temp file and returns a
// Server to exercise the lifecycle helpers against it.
func stateFileForTest(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet-report-state.json")
	SetFleetReportStatePathForTest(t, path)
	return &Server{}, path
}

// The posted -> recovered -> cleared fingerprint lifecycle is what decides
// whether the hive files a duplicate fleet-report issue or posts a recovery
// comment to the wrong issue, so each transition is pinned here.
func TestMarkFleetReportPostedRoundTrips(t *testing.T) {
	s, _ := stateFileForTest(t)

	if _, ok := s.FleetReportOpenIssue("fp-a"); ok {
		t.Fatal("empty state should report no open issue")
	}

	s.MarkFleetReportPosted("fp-a", 42, "https://example.test/i/42", true, "body-1")

	open, ok := s.FleetReportOpenIssue("fp-a")
	if !ok {
		t.Fatal("posted fingerprint should be retrievable")
	}
	if open.Number != 42 || open.URL != "https://example.test/i/42" || !open.OpenedByHive {
		t.Fatalf("unexpected open issue: %#v", open)
	}
	if open.BodyHash != fleetreport.StableBodyHash("body-1") {
		t.Fatalf("body hash not recorded: %#v", open)
	}
}

// OpenedByHive gates whether a recovery CLOSES the issue, so a re-post with
// openedByHive=false must never demote an issue the hive itself opened.
func TestMarkFleetReportPostedNeverDemotesOpenedByHive(t *testing.T) {
	s, _ := stateFileForTest(t)

	s.MarkFleetReportPosted("fp-a", 42, "u1", true, "body-1")
	s.MarkFleetReportPosted("fp-a", 42, "u1", false, "body-2")

	open, ok := s.FleetReportOpenIssue("fp-a")
	if !ok {
		t.Fatal("fingerprint should still be open")
	}
	if !open.OpenedByHive {
		t.Fatal("re-post with openedByHive=false demoted OpenedByHive")
	}
	if open.BodyHash != fleetreport.StableBodyHash("body-2") {
		t.Fatal("re-post should refresh the body hash")
	}
}

func TestMarkFleetReportRecovered(t *testing.T) {
	s, _ := stateFileForTest(t)

	// Recovering with no state at all must not create phantom entries.
	s.MarkFleetReportRecovered("fp-missing")
	if _, ok := s.FleetReportOpenIssue("fp-missing"); ok {
		t.Fatal("recovering an unknown fingerprint must not create an entry")
	}

	s.MarkFleetReportPosted("fp-a", 7, "u", false, "b")

	// Recovering a different fingerprint must not touch fp-a.
	s.MarkFleetReportRecovered("fp-other")
	open, _ := s.FleetReportOpenIssue("fp-a")
	if open.Recovered {
		t.Fatal("recovering fp-other marked fp-a recovered")
	}

	s.MarkFleetReportRecovered("fp-a")
	open, ok := s.FleetReportOpenIssue("fp-a")
	if !ok || !open.Recovered {
		t.Fatalf("fp-a should be marked recovered: %#v", open)
	}
	if open.Number != 7 {
		t.Fatalf("recovery must preserve the issue number: %#v", open)
	}
}

func TestClearFleetReportOpen(t *testing.T) {
	s, _ := stateFileForTest(t)

	// Clearing with no state must be a no-op, not a panic or a file write of
	// a phantom map.
	s.ClearFleetReportOpen("fp-missing")

	s.MarkFleetReportPosted("fp-a", 1, "u", false, "b")
	s.MarkFleetReportPosted("fp-b", 2, "u", false, "b")

	s.ClearFleetReportOpen("fp-a")

	if _, ok := s.FleetReportOpenIssue("fp-a"); ok {
		t.Fatal("cleared fingerprint should be gone")
	}
	if _, ok := s.FleetReportOpenIssue("fp-b"); !ok {
		t.Fatal("clearing fp-a must not drop fp-b")
	}
}

// A corrupt or unreadable state file must degrade to empty state (the
// documented load behavior) rather than poisoning the lifecycle.
func TestLoadFleetReportStateToleratesCorruptFile(t *testing.T) {
	s, path := stateFileForTest(t)

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.FleetReportOpenIssue("fp-a"); ok {
		t.Fatal("corrupt state file should read as empty state")
	}

	// And the lifecycle must recover by overwriting it.
	s.MarkFleetReportPosted("fp-a", 3, "u", false, "b")
	if open, ok := s.FleetReportOpenIssue("fp-a"); !ok || open.Number != 3 {
		t.Fatalf("state should be writable after corruption: %#v, %v", open, ok)
	}
}

func TestSetFleetReportBuildInfo(t *testing.T) {
	origVersion, origCommit := fleetReportBuildInfo()
	t.Cleanup(func() { SetFleetReportBuildInfo(origVersion, origCommit) })

	SetFleetReportBuildInfo("v9.9.9", "abc1234")
	version, commit := fleetReportBuildInfo()
	if version != "v9.9.9" || commit != "abc1234" {
		t.Fatalf("got %q/%q, want v9.9.9/abc1234", version, commit)
	}
}

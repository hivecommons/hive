package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestIssueBandPartiallyAddressedStaysActionable11931 pins that
// hive/partially-addressed, unlike hive/likely-done, does not park an issue in
// the done band: the remainder is still work.
func TestIssueBandPartiallyAddressedStaysActionable11931(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	partial := IssueBand(github.Issue{Repo: "octo/demo", Number: 1, Labels: []string{github.PartiallyAddressedLabel}}, false, config.DashboardIssueBandsConfig{}, now)
	if partial.Band != "ready" {
		t.Fatalf("partially-addressed band = %q, want ready (info=%+v)", partial.Band, partial)
	}
	likely := IssueBand(github.Issue{Repo: "octo/demo", Number: 2, Labels: []string{github.LikelyDoneLabel}}, false, config.DashboardIssueBandsConfig{}, now)
	if likely.Band != "done" {
		t.Fatalf("likely-done band = %q, want done", likely.Band)
	}
}

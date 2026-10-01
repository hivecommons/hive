package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestIssueBandInheritedAcknowledgement9840: a hive-filed child that
// inherits its parent's acknowledgement leaves the agent-filed band and the
// role badge tooltip names the parent it inherits from.
func TestIssueBandInheritedAcknowledgement9840(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	issue := github.Issue{
		Repo:              "o/r",
		Number:            9809,
		Labels:            []string{"agent/scanner"},
		HumanAcknowledged: true,
		AckParent:         9802,
		CreatedAt:         now.Add(-time.Hour),
		UpdatedAt:         now.Add(-time.Hour),
	}
	got := IssueBand(issue, false, config.DashboardIssueBandsConfig{}, now)
	if got.Band != "ready" || !got.Acknowledged {
		t.Fatalf("band=%q acknowledged=%v, want ready/true", got.Band, got.Acknowledged)
	}
	var roleLabel string
	for _, s := range got.Signals {
		if s.Role != "" {
			roleLabel = s.Label
		}
	}
	if !strings.Contains(roleLabel, "acknowledged via parent #9802") {
		t.Fatalf("role signal label = %q, want it to name parent #9802", roleLabel)
	}
}

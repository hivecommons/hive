package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestPRBandStageNeedsHumanIsNotTheNeedsHumanGate pins that the dashboard's
// "needs human" gate reads the human-applied `needs-human` label only: a
// maintainer PR that transiently carries `stage/090-needs-human` (a stage
// the automerge lanes own and recompute every cycle) with `author/trusted`
// is neither parked in the waiting band nor flagged as needing a human.
func TestPRBandStageNeedsHumanIsNotTheNeedsHumanGate(t *testing.T) {
	pr := github.PullRequest{Labels: []string{"stage/090-needs-human", "author/trusted", "stage/regressed"}}
	info := PRBand(pr, false, config.DashboardIssueBandsConfig{}, time.Now())
	if info.Band == "waiting" {
		t.Fatalf("stage/090 alone parked the PR in the waiting band: %+v", info)
	}
	for _, s := range info.Signals {
		if s.Glyph == "⚠" {
			t.Fatalf("stage/090 alone raised the needs-human signal: %+v", info.Signals)
		}
	}
	pr.Labels = append(pr.Labels, "needs-human")
	info = PRBand(pr, false, config.DashboardIssueBandsConfig{}, time.Now())
	if info.Band != "waiting" {
		t.Fatalf("needs-human label no longer gates: %+v", info)
	}
}

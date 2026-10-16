package agent

import (
	"testing"
	"time"
)

// TestTrajectoryIdle pins the agent-state idle predicate behind
// trajectory.AgentView.Idle (hivecommons/hive#12132): only a settled kick
// outcome, an AwaitingCI prompt, or "never kicked" count as idle. A kick
// whose turn has not ended is NOT idle however stale the pane is.
func TestTrajectoryIdle(t *testing.T) {
	kick := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	earlier := kick.Add(-time.Hour)
	cases := []struct {
		name string
		a    *AgentProcess
		want bool
	}{
		{"never kicked", &AgentProcess{}, true},
		{"kicked, turn in progress", &AgentProcess{LastKick: &kick}, false},
		{"kicked, turn in progress, pane stale for hours", &AgentProcess{LastKick: &kick, LastPaneChange: earlier}, false},
		{"kicked, outcome settled", &AgentProcess{LastKick: &kick, KickOutcome: KickOutcome{Kind: KickOutcomeEnded, KickAt: kick}}, true},
		{"kicked, outcome from an earlier kick", &AgentProcess{LastKick: &kick, KickOutcome: KickOutcome{Kind: KickOutcomeEnded, KickAt: earlier}}, false},
		{"kicked, awaiting CI at prompt", &AgentProcess{LastKick: &kick, AwaitingCI: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trajectoryIdle(tc.a); got != tc.want {
				t.Fatalf("trajectoryIdle = %v, want %v", got, tc.want)
			}
		})
	}
}

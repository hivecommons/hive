package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// The turn-loss measurement (#4002 open question 3) is worthless unless it
// survives the restart it measures, so the conversion across the state-file
// boundary is load-bearing rather than incidental. pkg/snapshot deliberately
// does not import pkg/agent, so these two functions are the only thing keeping
// the persisted shape and the in-memory shape in agreement.

func TestTurnLossRoundTripsThroughPersistedState(t *testing.T) {
	since := 90 * time.Second
	in := agent.TurnLoss{
		Interruptions: 3,
		Producing:     2,
		UpperBound:    7*time.Minute + 30*time.Second,
		Bytes:         4096,
		Recent: []agent.TurnInterruption{
			{
				At:          time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC),
				Reason:      "restart",
				SinceKick:   4 * time.Minute,
				SinceOutput: &since,
				Producing:   true,
				Bytes:       2048,
			},
			{
				At:        time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC),
				Reason:    "shutdown",
				SinceKick: 3*time.Minute + 30*time.Second,
				Producing: false,
				Bytes:     2048,
			},
		},
	}

	out := turnLossFromSnapshot(turnLossToSnapshot(in))

	if out.Interruptions != in.Interruptions || out.Producing != in.Producing {
		t.Errorf("counters = %d/%d, want %d/%d", out.Interruptions, out.Producing, in.Interruptions, in.Producing)
	}
	if out.UpperBound != in.UpperBound {
		t.Errorf("UpperBound = %v, want %v", out.UpperBound, in.UpperBound)
	}
	if out.Bytes != in.Bytes {
		t.Errorf("Bytes = %d, want %d", out.Bytes, in.Bytes)
	}
	if len(out.Recent) != len(in.Recent) {
		t.Fatalf("len(Recent) = %d, want %d", len(out.Recent), len(in.Recent))
	}
	if out.Recent[0].SinceOutput == nil || *out.Recent[0].SinceOutput != since {
		t.Errorf("Recent[0].SinceOutput = %v, want %v", out.Recent[0].SinceOutput, since)
	}
	// Unknown must stay unknown across the boundary. Materializing it as zero
	// would turn "we never saw the pane" into "the pane changed just now",
	// which reads as maximum activity — the overclaim direction.
	if out.Recent[1].SinceOutput != nil {
		t.Errorf("Recent[1].SinceOutput = %v, want nil to preserve unknown", out.Recent[1].SinceOutput)
	}
	if out.Recent[1].Reason != "shutdown" || out.Recent[0].Reason != "restart" {
		t.Error("Recent order or reasons did not survive the round trip")
	}
}

// TestUninterruptedAgentPersistsNothing: turn_loss is omitempty, and the
// overwhelming majority of agents have never been interrupted. Emitting an
// empty record for each would bloat every hive's state file and become its own
// argument for deleting the measurement.
func TestUninterruptedAgentPersistsNothing(t *testing.T) {
	if got := turnLossToSnapshot(agent.TurnLoss{}); got != nil {
		t.Errorf("turnLossToSnapshot(zero) = %+v, want nil", got)
	}
}

// TestNilPersistedTurnLossRestoresEmpty covers the upgrade path: every state
// file written before this change has no turn_loss key at all.
func TestNilPersistedTurnLossRestoresEmpty(t *testing.T) {
	got := turnLossFromSnapshot(nil)
	if got.Interruptions != 0 || len(got.Recent) != 0 {
		t.Errorf("turnLossFromSnapshot(nil) = %+v, want the zero value", got)
	}
}

// TestPersistedTurnLossIsJSONLegible pins the seconds-as-float choice. This
// file is read by operators with `jq`; a time.Duration would serialize as a
// nanosecond integer that nobody can read at a glance, and the whole point of
// the record is that a human can size the problem from it.
func TestPersistedTurnLossIsJSONLegible(t *testing.T) {
	out := turnLossToSnapshot(agent.TurnLoss{
		Interruptions: 1,
		UpperBound:    90 * time.Second,
		Recent:        []agent.TurnInterruption{{SinceKick: 90 * time.Second}},
	})
	if out.UpperBoundS != 90 {
		t.Errorf("UpperBoundS = %v, want 90", out.UpperBoundS)
	}
	if out.Recent[0].SinceKickS != 90 {
		t.Errorf("Recent[0].SinceKickS = %v, want 90", out.Recent[0].SinceKickS)
	}
}

package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/snapshot"
)

// Restart telemetry only matters because it survives the restart it records,
// so the conversion across the state-file boundary is load-bearing.
// pkg/snapshot deliberately does not import pkg/agent, so
// restartEventsToSnapshot / restartEventsFromSnapshot are the only thing
// keeping the persisted shape and the in-memory shape in agreement — the same
// contract turn_loss_persist_test.go pins for the turn-loss record.

func TestRestartEventsRoundTripThroughPersistedState(t *testing.T) {
	in := []agent.RestartEvent{
		{At: time.Now().Add(-2 * time.Hour), Reason: "output stalled"},
		{At: time.Now().Add(-30 * time.Minute), Reason: "config reload"},
	}

	out := restartEventsFromSnapshot(restartEventsToSnapshot(in))

	if len(out) != len(in) {
		t.Fatalf("round trip kept %d events, want %d", len(out), len(in))
	}
	for i := range in {
		if !out[i].At.Equal(in[i].At) {
			t.Errorf("event %d At = %v, want %v", i, out[i].At, in[i].At)
		}
		if out[i].Reason != in[i].Reason {
			t.Errorf("event %d Reason = %q, want %q", i, out[i].Reason, in[i].Reason)
		}
	}
}

func TestRestartEventsToSnapshotDropsStaleAndZeroTimeEvents(t *testing.T) {
	fresh := time.Now().Add(-time.Hour)
	in := []agent.RestartEvent{
		{At: time.Time{}, Reason: "zero time must not persist"},
		{At: time.Now().Add(-25 * time.Hour), Reason: "older than the 24h window"},
		{At: fresh, Reason: "still inside the window"},
	}

	out := restartEventsToSnapshot(in)

	if len(out) != 1 {
		t.Fatalf("persisted %d events, want only the fresh one: %+v", len(out), out)
	}
	if out[0].Reason != "still inside the window" || !out[0].At.Equal(fresh) {
		t.Errorf("survivor = %+v, want the fresh event", out[0])
	}
}

func TestRestartEventsFromSnapshotDropsStaleAndZeroTimeEvents(t *testing.T) {
	// The filter must hold on restore too: a state file written before a long
	// outage carries events that were fresh at persist time but are stale now.
	fresh := time.Now().Add(-time.Hour)
	in := []snapshot.AgentRestartEvent{
		{At: time.Time{}, Reason: "zero time"},
		{At: time.Now().Add(-48 * time.Hour), Reason: "stale after downtime"},
		{At: fresh, Reason: "recent"},
	}

	out := restartEventsFromSnapshot(in)

	if len(out) != 1 {
		t.Fatalf("restored %d events, want only the recent one: %+v", len(out), out)
	}
	if out[0].Reason != "recent" || !out[0].At.Equal(fresh) {
		t.Errorf("survivor = %+v, want the recent event", out[0])
	}
}

func TestRestartEventsConversionMapsEmptyToNil(t *testing.T) {
	if got := restartEventsToSnapshot(nil); got != nil {
		t.Errorf("restartEventsToSnapshot(nil) = %+v, want nil", got)
	}
	if got := restartEventsToSnapshot([]agent.RestartEvent{}); got != nil {
		t.Errorf("restartEventsToSnapshot(empty) = %+v, want nil so the JSON field is omitted", got)
	}
	if got := restartEventsFromSnapshot(nil); got != nil {
		t.Errorf("restartEventsFromSnapshot(nil) = %+v, want nil", got)
	}
	if got := restartEventsFromSnapshot([]snapshot.AgentRestartEvent{}); got != nil {
		t.Errorf("restartEventsFromSnapshot(empty) = %+v, want nil", got)
	}
}

func TestRestartEventsToSnapshotAllStaleYieldsEmptyNotNilPanic(t *testing.T) {
	in := []agent.RestartEvent{
		{At: time.Now().Add(-30 * time.Hour), Reason: "stale"},
	}
	out := restartEventsToSnapshot(in)
	if len(out) != 0 {
		t.Errorf("all-stale input persisted %d events, want none", len(out))
	}
}

func TestFirstNonEmptySkipsBlankValues(t *testing.T) {
	if got := firstNonEmpty("", "   ", "\t\n", "primary", "fallback"); got != "primary" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "primary")
	}
	// The winner is returned verbatim, not trimmed: callers own presentation.
	if got := firstNonEmpty("", " padded "); got != " padded " {
		t.Errorf("firstNonEmpty = %q, want the original %q", got, " padded ")
	}
	if got := firstNonEmpty("", "   "); got != "" {
		t.Errorf("firstNonEmpty(all blank) = %q, want empty", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
	}
}

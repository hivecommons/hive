package escalation

import (
	"path/filepath"
	"testing"
	"time"
)

func transientObs(head string, triaged bool, labels ...string) Observation {
	return Observation{Repo: "o/r", Number: 1, HeadSHA: head, Red: true, FailingChecks: []string{"build"}, TransientTriaged: triaged, Labels: labels, Labeled: HasNeedsHumanLabel(labels)}
}

func TestTransientRerunOwedOncePerHead(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "l.json"))
	if s.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("owed with no observation")
	}
	s.ObserveRed([]Observation{transientObs("a", false)})
	if s.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("owed on an untriaged head")
	}
	s.ObserveRed([]Observation{transientObs("a", true)})
	if !s.TransientRerunOwed("o/r", 1, "a") || !s.TransientRerunOwed("o/r", 1, "") {
		t.Fatal("triaged head must owe a re-run (by sha and by the ledger's current head)")
	}
	if s.TransientRerunOwed("o/r", 1, "b") {
		t.Fatal("owed for a head the triage does not cover")
	}
	s.RecordTransientRerun("o/r", 1, "a")
	if s.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("still owed after the re-run")
	}
	// Survives a restart: the ledger is on the PVC.
	s2 := Load(s.path)
	s2.ObserveRed([]Observation{transientObs("a", true)})
	if s2.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("a restart re-armed the spent re-run")
	}
	// A new head with its own triage owes its own re-run.
	s2.ObserveRed([]Observation{transientObs("b", true)})
	if !s2.TransientRerunOwed("o/r", 1, "b") {
		t.Fatal("new triaged head owes no re-run")
	}
	// Markers gone stale (the re-run made new run ids): nothing owed.
	s2.ObserveRed([]Observation{transientObs("b", false)})
	if s2.TransientRerunOwed("o/r", 1, "b") {
		t.Fatal("owed after the triage went stale")
	}
}

// A person (or the reviewer lane) returning an escalated PR to the lane
// gives a still-triaged head one more re-run.
func TestTransientRerunReArmedWhenUnparked(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "l.json"))
	now := time.Date(2026, 11, 6, 21, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	s.ObserveRed([]Observation{transientObs("a", true)})
	s.RecordTransientRerun("o/r", 1, "a")
	s.Sweep([]Observation{transientObs("a", true, NeedsHumanLabel)}, DefaultThreshold)
	if s.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("an escalated PR must owe nothing")
	}
	now = now.Add(LabelUnparkGrace + time.Minute)
	s.ObserveRed([]Observation{transientObs("a", true, ReviewerPassedLabel)})
	s.Sweep([]Observation{transientObs("a", true, ReviewerPassedLabel)}, DefaultThreshold)
	if !s.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("un-parked PR with a fresh transient triage must owe one more re-run")
	}
}

// The outcome of a spent re-run survives a restart, so a dependency-bot
// hand-off can say whether the re-run happened or was not allowed.
func TestTransientRerunOutcomePersists(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "l.json"))
	if spent, _ := s.TransientRerunOutcome("o/r", 1, "a"); spent {
		t.Fatal("spent with no record")
	}
	s.ObserveRed([]Observation{transientObs("a", true)})
	s.RecordTransientRerunOutcome("o/r", 1, "a", "needs-opt-in-label")
	s2 := Load(s.path)
	if spent, skipped := s2.TransientRerunOutcome("o/r", 1, "a"); !spent || skipped != "needs-opt-in-label" {
		t.Fatalf("outcome = (%v, %q), want spent and refused by the gate", spent, skipped)
	}
	if s2.TransientRerunOwed("o/r", 1, "a") {
		t.Fatal("a refused re-run is still owed")
	}
	if spent, _ := s2.TransientRerunOutcome("o/r", 1, "b"); spent {
		t.Fatal("another head reads as spent")
	}
	s2.RecordTransientRerun("o/r", 1, "b")
	if spent, skipped := s2.TransientRerunOutcome("o/r", 1, "b"); !spent || skipped != "" {
		t.Fatalf("outcome = (%v, %q), want re-run with no skip reason", spent, skipped)
	}
}

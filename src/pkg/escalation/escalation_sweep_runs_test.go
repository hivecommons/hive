package escalation

import (
	"strings"
	"testing"
	"time"

	escalatepkg "github.com/hivecommons/hive/pkg/escalate"
)

func TestSweepRunsPagesOncePerGenerationWithInjectedClock(t *testing.T) {
	store := Load("")
	now := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	obs := []RunObservation{{
		Key:          "hivecommons/hive#8312",
		Title:        "checkpoint policy",
		Repo:         "hivecommons/hive",
		Stage:        "plan",
		Gen:          7,
		WaitingOn:    "human",
		WaitingSince: now.Add(-2 * time.Hour),
		Link:         "https://example.invalid/runs/hivecommons%2Fhive%238312",
	}}

	got := store.SweepRuns(obs, time.Hour, escalatepkg.SeverityPage)
	if len(got) != 1 {
		t.Fatalf("first sweep events = %d, want 1: %+v", len(got), got)
	}
	if got[0].Event.Severity != escalatepkg.SeverityPage || got[0].Event.Link == "" ||
		!strings.Contains(got[0].Event.Title, "hivecommons/hive#8312") ||
		!strings.Contains(got[0].Event.Body, "Generation: 7") {
		t.Fatalf("event shape = %+v", got[0].Event)
	}
	store.MarkRunWaitEscalated(got[0].Key, got[0].Gen)
	if got := store.SweepRuns(obs, time.Hour, escalatepkg.SeverityPage); len(got) != 0 {
		t.Fatalf("duplicate generation events = %+v, want none", got)
	}
	obs[0].Gen = 8
	if got := store.SweepRuns(obs, time.Hour, escalatepkg.SeverityPage); len(got) != 1 {
		t.Fatalf("next generation events = %d, want 1: %+v", len(got), got)
	}
}

func TestSweepRunsSkipsFreshAndNonHumanWaits(t *testing.T) {
	store := Load("")
	now := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	store.SetClock(func() time.Time { return now })
	obs := []RunObservation{
		{Key: "fresh", Gen: 1, WaitingOn: "human", WaitingSince: now.Add(-30 * time.Minute)},
		{Key: "agent", Gen: 1, WaitingOn: "agent", WaitingSince: now.Add(-2 * time.Hour)},
	}
	if got := store.SweepRuns(obs, time.Hour, escalatepkg.SeverityDecision); len(got) != 0 {
		t.Fatalf("fresh/non-human events = %+v, want none", got)
	}
}

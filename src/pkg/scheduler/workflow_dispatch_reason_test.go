package scheduler

import (
	"strings"
	"testing"
	"time"
)

func TestDeclineIsStampedWithItsSession(t *testing.T) {
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	now := at.Add(10 * time.Minute)
	e := WorkflowDispatchEntry{Key: "k", Ref: "o/r#7", Agent: "quality", At: at, Status: WorkflowHandoffPending}
	got, changed := ReviewWorkflowHandoff(e, WorkflowHandoffEvidence{Now: now, Declined: "stand down: no push rights"})
	if !changed || got.Status != WorkflowHandoffNotStarted {
		t.Fatalf("not decided: %+v", got)
	}
	if got.ReasonAt == nil || !got.ReasonAt.Equal(now) || got.ReasonSession != "quality@2026-11-17T01:00:00Z" {
		t.Fatalf("reason provenance not recorded: %+v", got)
	}
	again := NextWorkflowDispatchEntry(got, &got)
	if again.ReasonAt != nil || again.ReasonSession != "" || again.Reason != "" {
		t.Fatalf("a new hand-off kept the old reason's provenance: %+v", again)
	}
}

func TestReasonOriginAgeAndUnknownSession(t *testing.T) {
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	legacy := WorkflowDispatchEntry{Key: "k", Ref: "o/r#7", At: at, Status: WorkflowHandoffNotStarted, Reason: "x"}
	o := ReplayedReasonOrigin(legacy, at.Add(3*time.Hour+5*time.Minute), false)
	if o.Session != ReasonSessionUnknown || o.Age != "3h 5m" || o.Stale {
		t.Fatalf("legacy origin: %+v", o)
	}
	if strings.Contains(o.Label(), "x") && strings.Contains(o.Label(), legacy.Reason+" ") {
		t.Fatalf("label repeated the reason: %q", o.Label())
	}
}

// A stale decline is never the current state: once a newer hand-off of the
// same item exists, the older record is marked stale; a hand-off of another
// item, or an older one, does not.
func TestStaleOnlyWhenANewerHandoffOfTheSameItemExists(t *testing.T) {
	at := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	log, _ := OpenWorkflowDispatchLog("")
	decided := at.Add(time.Hour)
	old := WorkflowDispatchEntry{Key: "a", Ref: "o/r#7", Agent: "q", At: at, Status: WorkflowHandoffNotStarted, Reason: "no", ReasonAt: &decided}
	other := WorkflowDispatchEntry{Key: "b", Ref: "o/r#8", Agent: "q", At: at.Add(2 * time.Hour), Status: WorkflowHandoffPending}
	_ = log.RecordAll([]WorkflowDispatchEntry{old, other})
	if log.Superseded(old) {
		t.Fatal("another item's hand-off made the decline stale")
	}
	earlier := WorkflowDispatchEntry{Key: "c", Ref: "o/r#7", Agent: "q", At: at.Add(-time.Hour), Status: WorkflowHandoffStarted}
	_ = log.Record(earlier)
	if log.Superseded(old) {
		t.Fatal("an older hand-off made the decline stale")
	}
	newer := WorkflowDispatchEntry{Key: "d", Ref: "o/r#7", Agent: "w", At: at.Add(3 * time.Hour), Status: WorkflowHandoffPending}
	_ = log.Record(newer)
	if !log.Superseded(old) {
		t.Fatal("a newer hand-off of the same item did not make the decline stale")
	}
	var nilLog *WorkflowDispatchLog
	if nilLog.Superseded(old) {
		t.Fatal("nil log reported stale")
	}
}

func TestHandoffAge(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Second: "0s", 40 * time.Second: "40s", 5*time.Minute + 2*time.Second: "5m 2s", 26 * time.Hour: "1d 2h"} {
		if got := HandoffAge(d); got != want {
			t.Errorf("%v: %q want %q", d, got, want)
		}
	}
}

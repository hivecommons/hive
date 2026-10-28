package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Close-first supersession (hivecommons/hive#15135): the closed original
// never reaches the tracker; a reopened one reads "copy failed", parked for
// a person, and is not "recovering".

func TestStageTrackerCloseFirstOriginalNotRendered(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.AdoptedByHive, "contributor-lane:adopted", []string{"stage/074-adopted-by-hive"})
	store.MarkAdopted(key, "bc3a6ea", 15123, now)
	store.MarkSupersededClosedFirst(key, 15123, false, now)
	// Even if the status still lists the original (the close has not
	// propagated), the superseded record drops the row.
	for _, it := range s.buildStageTracker(status, store, now).Items {
		if it.Kind == "pr" && it.Number == 14947 {
			t.Fatalf("superseded original rendered: %+v", it)
		}
	}
}

func TestStageTrackerCloseFirstCopyFailedSaysWhy(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionCopyFailed, []string{"stage/085-needs-decision"})
	store.MarkAdopted(key, "bc3a6ea", 15123, now.Add(-time.Hour))
	store.MarkSupersededClosedFirst(key, 15123, false, now.Add(-time.Hour))
	store.MarkAdoptionReopened(key, 15123, now)
	store.MarkAdoptionEscalated(key, "bc3a6ea", stagelabels.LaneReasonAdoptionCopyFailed)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 15123 || it.AdoptedByState != "copy-failed" {
		t.Fatalf("adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "copy #15123 failed") || !strings.Contains(it.Caption, "reopened") || !strings.Contains(it.Caption, "needs a person") || strings.Contains(it.Caption, "fresh copy") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.DecisionNeeded == nil || !strings.Contains(it.DecisionNeeded.Question, "#15123") || !strings.Contains(it.DecisionNeeded.Question, "reopened") {
		t.Fatalf("decision_needed = %+v", it.DecisionNeeded)
	}
	if got := humanizeStageReason(stagelabels.LaneReasonAdoptionCopyFailed); !strings.Contains(got, "copy of this PR failed") {
		t.Fatalf("humanizeStageReason = %q", got)
	}
	raw, _ := json.Marshal(it)
	if !strings.Contains(string(raw), `"adopted_by_state":"copy-failed"`) {
		t.Fatalf("payload lacks adopted_by_state: %s", raw)
	}
	// An operator re-adopts: the fresh copy clears the state.
	store.MarkAdopted(key, "bc3a6ea", 15200, now.Add(time.Minute))
	store.Observe(key, stagelabels.AdoptedByHive, "contributor-lane:adopted")
	it = trackerItem(t, s.buildStageTracker(status, store, now.Add(time.Minute)), "pr", 14947)
	if it.AdoptedBy != 15200 || it.AdoptedByState != "" {
		t.Fatalf("after re-adopt adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
}

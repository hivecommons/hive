package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1091: an adopted PR whose hive push undid its change
// reads as a person's decision with that reason, not as the hive fixing it.
func TestStageTrackerAdoptionFixerRevertsSaysWhy(t *testing.T) {
	now := time.Date(2026, 11, 3, 21, 15, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionFixerReverts, []string{"stage/085-needs-decision"})
	store.MarkAdoptedInPlace(key, "bc3a6ea", "contributor-offline", now.Add(-time.Hour))
	store.MarkAdoptionEscalated(key, "bc3a6ea", stagelabels.LaneReasonAdoptionFixerReverts)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if !strings.Contains(it.Caption, "fixing CI would undo this PR's change") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.DecisionNeeded == nil || !strings.Contains(it.DecisionNeeded.Question, "undoes most of the PR's own change") {
		t.Fatalf("decision_needed = %+v", it.DecisionNeeded)
	}
	if got := humanizeStageReason(stagelabels.LaneReasonAdoptionFixerReverts); !strings.Contains(got, "undo this PR's change") {
		t.Fatalf("humanizeStageReason = %q", got)
	}
}

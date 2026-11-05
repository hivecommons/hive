package dashboard

import (
	"strings"
	"testing"
)

func TestStageTrackerApplyTrainQueuedCaption(t *testing.T) {
	const front = "queued for base refresh behind PR #16718"
	stock := "Review swarm requested; waiting for a verdict on the current head — " + front
	mk := func(state string, done, total int) *StageTrackerItem {
		return &StageTrackerItem{Kind: "pr", Number: 16691, Head: "abc1234", Stage: trackerNumberReviewPending,
			Reason: "base-refresh:queued-behind#16718", Caption: stock, Actor: "review swarm",
			StageActor:     &StageTrackerActor{Kind: StageActorHive, Why: "review swarm — waiting for a verdict on the current head"},
			ReviewActivity: &StageTrackerActivity{Head: "abc1234", State: state, PerspectivesDone: done, PerspectivesTotal: total}}
	}
	cases := []struct {
		name    string
		it      *StageTrackerItem
		caption string
		why     string
	}{
		{"approved behind front", mk("approved", 5, 5), "Approved 5/5 · " + front, "approved — " + front},
		{"approved without a count", mk("approved", 0, 0), "Approved · " + front, "approved — " + front},
		{"still reviewing", mk("reviewing", 3, 5), stock, "review swarm — waiting for a verdict on the current head"},
		{"changes requested", mk("changes_requested", 5, 5), stock, "review swarm — waiting for a verdict on the current head"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageTrackerApplyTrainQueuedCaption(tc.it)
			if tc.it.Caption != tc.caption {
				t.Fatalf("caption = %q, want %q", tc.it.Caption, tc.caption)
			}
			if tc.it.StageActor.Why != tc.why {
				t.Fatalf("why = %q, want %q", tc.it.StageActor.Why, tc.why)
			}
			if tc.it.ReviewActivity.State == "approved" && strings.Contains(tc.it.Caption, "waiting for a verdict") {
				t.Fatalf("approved caption still waits for a verdict: %q", tc.it.Caption)
			}
		})
	}
}

func TestStageTrackerApplyTrainQueuedCaptionLeavesOtherRows(t *testing.T) {
	approved := &StageTrackerActivity{Head: "abc1234", State: "approved", PerspectivesDone: 5, PerspectivesTotal: 5}
	cases := []struct {
		name string
		it   *StageTrackerItem
	}{
		{"stale head approval", &StageTrackerItem{Kind: "pr", Head: "ffff999", Stage: trackerNumberReviewPending, Reason: "base-refresh:queued-behind#16718", Caption: "stock", ReviewActivity: approved}},
		{"060 keeps merge-admission words", &StageTrackerItem{Kind: "pr", Head: "abc1234", Stage: trackerNumberReviewed, Reason: "base-refresh:queued-behind#16718", Caption: "Review passed; waiting for merge admission — queued for base refresh behind PR #16718", ReviewActivity: approved}},
		{"050 without a train reason", &StageTrackerItem{Kind: "pr", Head: "abc1234", Stage: trackerNumberReviewPending, Reason: "no review verdict yet", Caption: "stock", ReviewActivity: approved}},
		{"preparing front", &StageTrackerItem{Kind: "pr", Head: "abc1234", Stage: trackerNumberReviewPending, Reason: "base-refresh:preparing", Caption: "stock", ReviewActivity: approved}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.it.Caption
			stageTrackerApplyTrainQueuedCaption(tc.it)
			if tc.it.Caption != before {
				t.Fatalf("caption changed to %q", tc.it.Caption)
			}
		})
	}
}

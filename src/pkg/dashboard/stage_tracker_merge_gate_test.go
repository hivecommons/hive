package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

func TestStageTrackerMergeGateStatesAndNestedSoak(t *testing.T) {
	now := time.Date(2026, 10, 28, 19, 0, 0, 0, time.UTC)
	for stage, state := range map[int]string{50: "gate-pending", 60: "gate-pending", 80: "ready", 95: "soaking", 100: "merging", 110: "merged"} {
		got := stageTrackerMergeGate(StageTrackerItem{Stage: stage, Reason: "check-pending", SoakRemainingSeconds: 420}, github.MergeSweepTimes{}, now)
		if got == nil || got.State != state || got.Reason != "check-pending" {
			t.Fatalf("stage %d: %+v", stage, got)
		}
		if stage == 95 && (got.SoakUntil == nil || !got.SoakUntil.Equal(now.Add(7*time.Minute))) {
			t.Fatalf("soak deadline: %+v", got)
		}
	}
	pr := StageTrackerItem{Repo: "org/repo", Kind: "pr", Number: 2, Stage: 95, SoakRemainingSeconds: 420, Reason: "soaking(remaining=7m)"}
	pr.MergeGate = stageTrackerMergeGate(pr, github.MergeSweepTimes{SweepRunning: true, NextSweepAt: &now}, now)
	items := []StageTrackerItem{{Repo: "org/repo", Kind: "issue", Number: 1, LinkedPR: 2}, pr}
	stageTrackerResolveLinkedPRs(items)
	nested := items[0].LinkedPRs[0]
	if nested.MergeGate != pr.MergeGate || nested.SoakRemainingSeconds != 420 || nested.Reason != pr.Reason {
		t.Fatalf("nested PR lost gate context: %+v", nested)
	}
	data, err := json.Marshal(nested)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	gate := wire["merge_gate"].(map[string]any)
	for _, key := range []string{"state", "reason", "sweep_running", "last_sweep_at", "next_sweep_at", "soak_until", "soak_remaining_seconds"} {
		if _, ok := gate[key]; !ok {
			t.Fatalf("missing wire field %s: %s", key, data)
		}
	}
}

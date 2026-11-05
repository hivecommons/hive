package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1097: a hive hold from the human-merge-path refusal
// must not offer "release it and the lane merges it"; no lane merges it.
func TestStageTrackerDecisionHumanMergePathHold(t *testing.T) {
	since := time.Date(2026, 11, 3, 15, 42, 20, 0, time.UTC)
	it := StageTrackerItem{Repo: "projectbluefin/utah", Number: 1097, Kind: "pr", Stage: stagelabels.NeedsDecision.Number(), StageSince: &since, Reason: "label:hold", Modifiers: []string{modHold},
		Hold: &StageTrackerHold{By: "hive", Source: stagelabels.HoldSourceHumanMergePath, Reason: "it changes " + stagelabels.HumanMergePathReason, Since: &since}}
	d := stageTrackerDecisionNeeded(it, nil, "")
	if d == nil {
		t.Fatal("a hive-held 085 row must carry a decision")
	}
	for _, want := range []string{"because it changes a human-merge path", "the hive never merges it", "merge it by hand"} {
		if !strings.Contains(d.Question, want) {
			t.Errorf("question lacks %q: %q", want, d.Question)
		}
	}
	if strings.Contains(d.Question, "the lane then merges it") {
		t.Fatalf("question promises a lane merge: %q", d.Question)
	}
}

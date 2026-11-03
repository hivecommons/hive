package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A ⚠️-titled PR the lanes parked with the human-merge rule
// (hivecommons/hive#15442) renders at 090 as "Ready for human merge" with
// the "human merge" chip and the owner-merges reason; the lead decision is
// the operator's merge-as-maintainer click and queueing for the hive is
// offered greyed, since no lane merges it however it is queued.
func TestStageTrackerHumanMergeRequiredRow(t *testing.T) {
	now := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 15442), stagelabels.ReadyForHumanMerge, "human-merge-required")
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 15442, Title: "\u26a0\ufe0f drop the v4 API", Author: "pat", Labels: []string{"stage/090-ready-for-human-merge", "lgtm"}}},
		// Not yet staged: the 000-new narration names the human-merge gate.
		FrontendPR{PullRequest: github.PullRequest{Number: 15443, Title: "\u2728 add a thing", Author: "pat", Labels: []string{"needs-owner-merge"}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	it := trackerItem(t, resp, "pr", 15442)
	if it.Stage != 90 || !it.ManualMerge || it.HumanMergeReason != "breaking-change" {
		t.Fatalf("row = stage %d manual %v reason %q", it.Stage, it.ManualMerge, it.HumanMergeReason)
	}
	if it.Caption != "Ready for human merge (breaking change — owner merges)" || it.Actor != "maintainer" {
		t.Fatalf("caption %q actor %q", it.Caption, it.Actor)
	}
	if !strings.HasPrefix(it.HumanMergeReasonText, "Breaking change — owner merges") {
		t.Fatalf("reason text %q", it.HumanMergeReasonText)
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"manual_merge":true,"human_merge_reason":"breaking-change"`) {
		t.Fatalf("payload lacks the chip fields: %s", raw)
	}
	if it.DecisionNeeded == nil || len(it.DecisionNeeded.Options) < 2 {
		t.Fatalf("decision = %+v", it.DecisionNeeded)
	}
	mergeNow, queue := it.DecisionNeeded.Options[0], it.DecisionNeeded.Options[1]
	if mergeNow.Action != decisionActionMergeNow || !mergeNow.Enabled {
		t.Fatalf("lead option = %+v, want an enabled merge-now", mergeNow)
	}
	if queue.Action != decisionActionQueueMerge || queue.Enabled || !strings.Contains(queue.ReasonIfDisabled, "never merges a breaking change") {
		t.Fatalf("queue option = %+v, want it greyed with the rule", queue)
	}
	if !strings.Contains(strings.ToLower(it.DecisionNeeded.Question), "breaking change") {
		t.Fatalf("question %q must name the rule", it.DecisionNeeded.Question)
	}

	fresh := trackerItem(t, resp, "pr", 15443)
	if fresh.New == nil {
		t.Fatalf("PR 15443 should be 000-new: %+v", fresh)
	}
	var gate *StageTrackerGate
	for i := range fresh.New.Gates {
		if fresh.New.Gates[i].Name == StageTrackerGateHumanMerge {
			gate = &fresh.New.Gates[i]
		}
	}
	if gate == nil || gate.Text != "breaking change — owner merges" {
		t.Fatalf("new.gates = %+v, want a human-merge gate", fresh.New.Gates)
	}
	if plain := trackerItem(t, resp, "pr", 103); plain.New != nil {
		for _, g := range plain.New.Gates {
			if g.Name == StageTrackerGateHumanMerge {
				t.Fatalf("PR 103 must not carry the human-merge gate: %+v", plain.New.Gates)
			}
		}
	}
}

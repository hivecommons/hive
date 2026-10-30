package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerItemColumnRatchet: the chevron column follows the
// CURRENT state, never the highest stage a PR once reached. Every PR
// stage × gate state × train state × reason → one column, and the same
// predicate (stageTrackerTrainUnderRepair) that seats or drops a train
// member decides it, so membership and column can never disagree.
// Hive's own #15574 sat at 070 "Fixing · conflicting" under Merge queue
// because its stale merge-failed gate still promoted the column.
func TestStageTrackerItemColumnRatchet(t *testing.T) {
	gates := []string{"", MergeGateStatePending, MergeGateStateReady, MergeGateStateSoaking, MergeGateStateMerging, MergeGateStateMergeFailed}
	trains := []string{"", MergeGateStateQueuedBehind, MergeGateStateRebasing, MergeGateStateRebaseExhausted, MergeGateStateTrainFront}
	reasons := []string{"", "conflicting", "required-check-failing:build-and-test", "ci-red", "required-check-pending:build-and-test"}
	cases := 0
	for _, d := range stagelabels.Table {
		if !d.PR {
			continue
		}
		for _, gate := range gates {
			for _, train := range trains {
				for _, reason := range reasons {
					it := StageTrackerItem{Repo: "hivecommons/hive", Number: 1, Kind: "pr", Stage: d.Number, Reason: reason}
					if gate != "" {
						it.MergeGate = &StageTrackerMergeGate{State: gate}
					}
					if train != "" {
						it.MergeTrain = &StageTrackerMergeTrain{ID: "v5#2", State: train, Front: 2, Position: 2, Size: 2}
					}
					want := stageTrackerColumn(d.Stage)
					repairing := stageTrackerTrainUnderRepair(d.Number, reason) != ""
					switch {
					case d.Number <= trackerColumnPR:
						// A stale gate never jumps a PR back in progress forward.
					case repairing && d.Stage != stagelabels.NeedsDecision:
						want = trackerColumnReview
					case gate == MergeGateStateMerging || gate == MergeGateStateMergeFailed || train != "":
						want = trackerColumnMergeGate
					}
					got := stageTrackerItemColumn(&it)
					label := d.Stage.String() + " gate=" + gate + " train=" + train + " reason=" + reason
					if got != want {
						t.Errorf("%s: column %d, want %d", label, got, want)
					}
					if repairing && d.Number > trackerColumnPR && d.Stage != stagelabels.NeedsDecision && got > trackerColumnReview {
						t.Errorf("%s: under repair yet promoted to column %d", label, got)
					}
					cases++
				}
			}
		}
	}
	if cases < 11*len(gates)*len(trains)*len(reasons) {
		t.Fatalf("ratchet covered only %d cases", cases)
	}
	merged := StageTrackerItem{Kind: "pr", Stage: stageTrackerMergedStage, MergeGate: &StageTrackerMergeGate{State: MergeGateStateMergeFailed}}
	if got := stageTrackerItemColumn(&merged); got != trackerColumnMerged {
		t.Fatalf("merged row column %d, want %d", got, trackerColumnMerged)
	}
	if got := stageTrackerItemColumn(nil); got != trackerColumnOpen {
		t.Fatalf("nil column %d", got)
	}
	// hivecommons/docs#512 keeps its column: a merge outcome at 050/060
	// with nothing to repair is a Merge queue matter.
	docs512 := StageTrackerItem{Kind: "pr", Stage: 60, Reason: "check-pending", MergeGate: &StageTrackerMergeGate{State: MergeGateStateMergeFailed}}
	if got := stageTrackerItemColumn(&docs512); got != trackerColumnMergeGate {
		t.Fatalf("docs#512 column %d, want %d", got, trackerColumnMergeGate)
	}
}

// TestStageTrackerFixingAfterMergeFailedColumn replays hive's own
// #15574 through the payload: an adopted copy that led a train, whose
// merge attempt GitHub refused (conflict), now recorded at 070 fixing
// while a sibling's record still names it as the front. The refusal
// stays on the gate as history, but its column is CI / Review, it rides
// no train (the sub-row under its issue says so too), and the survivor's
// train shrinks to none.
func TestStageTrackerFixingAfterMergeFailedColumn(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	queued := prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15574, Position: 2, Size: 2, OverlapFiles: []string{"docs/a.md"}}.Reason()
	store.Observe(stagelabels.Key("acme/console", 15560), stagelabels.AwaitingMergeAdmission, queued)
	store.Observe(stagelabels.Key("acme/console", 15574), stagelabels.AwaitingMergeAdmission, "")
	store.Observe(stagelabels.Key("acme/console", 15574), stagelabels.Fixing, "conflicting")
	head := "abc123"
	attempts := []github.MergeAttempt{{Repo: "acme/console", Number: 15574, Path: github.PRAuditPathSweep, Head: head,
		StartedAt: now.Add(-10 * time.Minute), EndedAt: now.Add(-10*time.Minute + time.Second), Error: "PUT /merge: 405 Head branch is out of date"}}
	s.mergeAttempts = func() []github.MergeAttempt { return attempts }
	s.statusMu.Lock()
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues,
		github.Issue{Number: 15511, Title: "Tracker column", Author: "pat", AuthorIsHuman: true, Labels: []string{"stage/040-covered-by-pr"}})
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 15560, Title: "Sibling", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: []string{stagelabels.AwaitingMergeAdmission.Label(), "author/hive"}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 15574, Title: "Adopted copy", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", HeadSHA: head,
			Labels: []string{stagelabels.Fixing.Label(), "author/hive"}, LinkedIssues: []github.PRLinkedIssue{{Number: 15511}}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	pr := trackerItem(t, resp, "pr", 15574)
	if pr.Stage != stagelabels.Fixing.Number() {
		t.Fatalf("stage %d, want 070", pr.Stage)
	}
	if pr.MergeGate == nil || pr.MergeGate.State != MergeGateStateMergeFailed {
		t.Fatalf("the refusal stays on the gate as history: %+v", pr.MergeGate)
	}
	if pr.MergeTrain != nil || pr.LeftTrain == nil || pr.LeftTrain.Why != stagelabels.Fixing.Slug() {
		t.Fatalf("a PR being fixed rides no train: train %+v left %+v", pr.MergeTrain, pr.LeftTrain)
	}
	if got := stageTrackerItemColumn(&pr); got != trackerColumnReview {
		t.Fatalf("070 fixing after a refused merge lights column %d, want CI / Review %d", got, trackerColumnReview)
	}
	alone := trackerItem(t, resp, "pr", 15560)
	if alone.MergeTrain != nil || stageTrackerItemColumn(&alone) != trackerColumnMergeGate {
		t.Fatalf("survivor: train %+v column %d", alone.MergeTrain, stageTrackerItemColumn(&alone))
	}
	issue := trackerItem(t, resp, "issue", 15511)
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Number != 15574 {
		t.Fatalf("issue linked_prs = %+v", issue.LinkedPRs)
	}
	lp := issue.LinkedPRs[0]
	if lp.MergeTrain != nil || lp.LeftTrain == nil || lp.LeftTrain.Why != stagelabels.Fixing.Slug() {
		t.Fatalf("the sub-row mirrors the row: train %+v left %+v", lp.MergeTrain, lp.LeftTrain)
	}
	raw, err := json.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"left_train":{"why":"fixing"`) {
		t.Fatalf("linked_prs payload lacks left_train:\n%s", raw)
	}
}

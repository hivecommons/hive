package automerge

import (
	"context"
	"strings"
	"testing"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// newBudgetSweepClient is newAutoMergeSweepClient with the stage reconciler
// on, so the sweep's per-PR reasons land in tracker.
func newBudgetSweepClient(apiURL string, tracker *StageTracker) *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	return New(client, Options{
		HeadFirstSeen: NewHeadFirstSeenTracker(),
		MergerAuthorizer: func(login string) bool {
			return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
		},
		StageLabelPolicy: func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} },
		StageTracker:     tracker,
	})
}

// hivecommons/hive#12607: with more behind self-authored PRs than the
// per-tick update budget, the PRs past the budget keep an explicit
// behind/update-branch cause instead of the generic check-pending, and the
// stage engine reads that as a branch update the hive owes (merge queue,
// waiting on the hive), never contributor or fix work.
func TestSweepSelfAuthoredBehindPastBudgetKeepsUpdateBranchCause(t *testing.T) {
	var merged []int
	updateCounts := make([]int, selfAuthoredSweepMaxBranchUpdates+3)
	prs := make([]selfAuthoredPR, 0, len(updateCounts))
	for i := range updateCounts {
		prs = append(prs, selfAuthoredPR{
			number:         60 + i,
			author:         testHiveAppBotLogin,
			mergeableState: "behind",
			updateCount:    &updateCounts[i],
		})
	}
	api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
	defer api.Close()

	tracker := NewStageTracker()
	c := newBudgetSweepClient(api.URL, tracker)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("SweepSelfAuthoredAutoMerges returned error: %v", err)
	}
	if result.UpdatedBranches != selfAuthoredSweepMaxBranchUpdates {
		t.Fatalf("UpdatedBranches = %d, want %d", result.UpdatedBranches, selfAuthoredSweepMaxBranchUpdates)
	}

	reasons := map[int]stageObservation{}
	for _, list := range tracker.drain() {
		for _, o := range list {
			if o.Lane == stageLaneSelf {
				reasons[o.Number] = o
			}
		}
	}
	exhausted := 0
	for i := range updateCounts {
		number := 60 + i
		o, ok := reasons[number]
		if !ok {
			t.Fatalf("PR %d: no self-authored stage observation", number)
		}
		if i < selfAuthoredSweepMaxBranchUpdates {
			if o.Reason == autoMergeReasonBehindBudgetExhausted {
				t.Fatalf("PR %d was updated within the budget but recorded %q", number, o.Reason)
			}
			continue
		}
		exhausted++
		if o.Reason != autoMergeReasonBehindBudgetExhausted {
			t.Fatalf("PR %d past the budget recorded reason %q, want %q", number, o.Reason, autoMergeReasonBehindBudgetExhausted)
		}
		v := classifyStage(o)
		if v.Stage != stagelabels.MergeQueue || v.Rank != stageRankOwned {
			t.Fatalf("PR %d past the budget classified %+v, want owned merge queue", number, v)
		}
		if a := stageActivity(o, v.Stage); !a.WaitingOnHive {
			t.Fatalf("PR %d past the budget does not read as waiting on the hive", number)
		}
	}
	if exhausted != 3 {
		t.Fatalf("budget-exhausted PRs = %d, want 3", exhausted)
	}
}

// The budget-exhausted path is distinct from a true conflict, which stays
// author-facing.
func TestTrySweepSelfAuthoredBehindWithoutBudgetVsConflicting(t *testing.T) {
	var merged []int
	var behindUpdates, dirtyUpdates int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{
		{number: 81, author: testHiveAppBotLogin, mergeableState: "behind", updateCount: &behindUpdates},
		{number: 82, author: testHiveAppBotLogin, mergeableState: "dirty", updateCount: &dirtyUpdates},
	}, &merged)
	defer api.Close()

	c := newAutoMergeSweepClient(api.URL)
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 81, false)
	if err != nil || reason != autoMergeReasonBehindBudgetExhausted || behindUpdates != 0 {
		t.Fatalf("behind without budget: reason=%q err=%v updates=%d, want %q and no update", reason, err, behindUpdates, autoMergeReasonBehindBudgetExhausted)
	}
	_, reason, err = c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 82, false)
	if err != nil || reason != "conflicting" || dirtyUpdates != 0 {
		t.Fatalf("dirty: reason=%q err=%v updates=%d, want conflicting", reason, err, dirtyUpdates)
	}
	if v := classifyStage(stageObservation{Lane: stageLaneTrusted, Reason: "conflicting"}); v.Stage != stagelabels.NeedsAuthor {
		t.Fatalf("conflicting classified %+v, want 072", v)
	}
	if v := classifyStage(stageObservation{Lane: stageLaneTrusted, Reason: autoMergeReasonBehindBudgetExhausted}); v.Stage != stagelabels.MergeQueue {
		t.Fatalf("budget-exhausted classified %+v, want merge queue", v)
	}
}

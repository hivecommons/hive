package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

// A base branch whose ruleset requires GitHub's merge queue refuses the
// sweep's REST merge, and the sweep skips the PR with merge-queue-required on
// every pass. The classifier used to call the same PR "eligible: the sweep
// would merge this now" (projectbluefin/bluefin-bling#93 and #113,
// 2026-10-25), so the dashboard listed it as Merge-eligible while only a
// person could land it.

func TestClassifyMergeEligibility_MergeQueueRequired(t *testing.T) {
	green := github.PullRequest{Number: 1, BaseRef: "main", Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"}
	red := github.PullRequest{Number: 2, BaseRef: "main", Mergeable: github.MergeableYes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}}

	cases := []struct {
		name       string
		pr         github.PullRequest
		held       bool
		gates      mergeGates
		wantBucket mergeBucket
		wantState  github.MergeVerdictState
		wantHand   bool
		wantReason string
	}{
		{"queue-required base is a hand merge", green, false, mergeGates{mergeQueueRequired: true}, mergeBucketSkip, github.MergeVerdictOutstanding, true, "merge-queue-required: main only accepts merges through GitHub's merge queue"},
		{"same PR without a queue is eligible", green, false, mergeGates{}, mergeBucketEligible, github.MergeVerdictEligible, false, "the sweep would merge this now"},
		{"held PR stays held", green, true, mergeGates{mergeQueueRequired: true}, mergeBucketSkip, github.MergeVerdictOutstanding, false, "label:hold"},
		{"red PR is still CI work", red, false, mergeGates{mergeQueueRequired: true}, mergeBucketFailing, github.MergeVerdictOutstanding, false, "CI failing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict := mergeVerdictOf(t, tc.pr, tc.held, tc.gates)
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			if verdict.State != tc.wantState || verdict.HandMerge != tc.wantHand {
				t.Errorf("verdict = %+v, want state %q HandMerge=%v", verdict, tc.wantState, tc.wantHand)
			}
			if !strings.Contains(verdict.Reason, tc.wantReason) {
				t.Errorf("reason %q does not contain %q", verdict.Reason, tc.wantReason)
			}
		})
	}

	if got := mergeQueueRequiredReason(""); !strings.Contains(got, "the base branch only accepts") {
		t.Errorf("reason without a base ref = %q, want it to name the base branch generically", got)
	}
}

func TestWriteMergeEligible_MergeQueueRequiredBase(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail, origBases := mergeEligiblePath, ciFailingPath, hiveMergeQueueBases
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	hiveMergeQueueBases = automerge.NewMergeQueueBases()
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
		hiveMergeQueueBases = origBases
	})

	// What the self-authored sweep records on the 405.
	hiveMergeQueueBases.Note("org/bling", "main", time.Now())

	l6 := config.SelfMergeMinACMMLevel
	cfg := &config.Config{ACMMLevel: &l6, Project: config.ProjectConfig{Org: "org"}}
	pr := func(repo, base string, n int) github.PullRequest {
		return github.PullRequest{Repo: repo, BaseRef: base, Number: n, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", HeadSHA: "abc"}
	}
	queued := pr("bling", "main", 113)
	otherBase := pr("bling", "testing", 114)
	otherRepo := pr("demo", "main", 7)
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{queued, otherBase, otherRepo}}}
	got := writeMergeEligible(actionable, github.HoldResult{}, "org", nil, false, nil, false, nil,
		nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if v := got[github.MergeVerdictKey(queued)]; !v.HandMerge || v.State != github.MergeVerdictOutstanding || !strings.Contains(v.Reason, "merge-queue-required") {
		t.Errorf("queue base verdict = %+v, want an outstanding hand merge naming merge-queue-required", v)
	}
	if mergeTargetEligible("org/bling", 113, "abc") {
		t.Error("queue base PR is in merge-eligible.json; the relay merges over REST and cannot land it either")
	}
	for _, p := range []github.PullRequest{otherBase, otherRepo} {
		if v := got[github.MergeVerdictKey(p)]; v.HandMerge || v.State != github.MergeVerdictEligible {
			t.Errorf("%s@%s verdict = %+v, want eligible", p.Repo, p.BaseRef, v)
		}
		if !mergeTargetEligible("org/"+p.Repo, p.Number, "abc") {
			t.Errorf("%s#%d missing from merge-eligible.json", p.Repo, p.Number)
		}
	}
}

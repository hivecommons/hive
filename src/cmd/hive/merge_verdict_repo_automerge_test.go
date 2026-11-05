package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Every merge sweep skips a repo whose auto-merge switch is off
// (config.RepoAutoMergeEnabled; "repo-auto-merge-disabled" in the sweep log),
// and below ACMM L6 that is every repo. The verdict must say so instead of
// "the sweep would merge this now": green there told the operator a PR merges
// on its own when only a person could land it (#7478's contract).

func TestClassifyMergeEligibility_RepoAutoMergeOffIsHandMerge(t *testing.T) {
	green := github.PullRequest{Number: 1, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"}

	bucket, verdict := mergeVerdictOf(t, green, false, mergeGates{repoAutoMergeOff: true})
	if bucket != mergeBucketEligible {
		t.Errorf("bucket = %v, want eligible: merge-eligible.json is unchanged, its readers apply the repo switch themselves", bucket)
	}
	if verdict.State != github.MergeVerdictOutstanding || !verdict.HandMerge {
		t.Errorf("verdict = %+v, want outstanding with HandMerge", verdict)
	}
	if !strings.Contains(verdict.Reason, "auto-merge is off") {
		t.Errorf("reason %q does not say auto-merge is off", verdict.Reason)
	}

	bucket, verdict = mergeVerdictOf(t, green, false, mergeGates{})
	if bucket != mergeBucketEligible || verdict.State != github.MergeVerdictEligible || verdict.HandMerge {
		t.Errorf("repo on: bucket %v verdict %+v, want eligible without HandMerge", bucket, verdict)
	}

	// An earlier gate still wins in the classifier: a red PR in an off repo
	// keeps its CI verdict here. writeMergeEligible then marks an outstanding
	// verdict as a hand merge too (handMergeVerdict), so the your-turn inbox
	// still sees it.
	red := github.PullRequest{Number: 2, Mergeable: github.MergeableYes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}}
	bucket, verdict = mergeVerdictOf(t, red, false, mergeGates{repoAutoMergeOff: true})
	if bucket != mergeBucketFailing || verdict.HandMerge {
		t.Errorf("red PR in an off repo: bucket %v verdict %+v, want failing without HandMerge", bucket, verdict)
	}
}

func TestWriteMergeEligible_RepoAutoMergeSwitchDrivesVerdict(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	off := false
	l5, l6 := 5, config.SelfMergeMinACMMLevel
	pr := func(repo string, n int) github.PullRequest {
		return github.PullRequest{Repo: repo, Number: n, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", HeadSHA: "abc"}
	}
	cases := []struct {
		name     string
		cfg      *config.Config
		wantHand bool
	}{
		{"L5 hive: every repo is off", &config.Config{ACMMLevel: &l5, Project: config.ProjectConfig{Org: "org"}}, true},
		{"L6 hive, repo switched off", &config.Config{ACMMLevel: &l6, Project: config.ProjectConfig{Org: "org", RepoPolicies: []config.RepoPolicy{{Repo: "repo", AutoMerge: &off}}}}, true},
		{"L6 hive, repo on by default", &config.Config{ACMMLevel: &l6, Project: config.ProjectConfig{Org: "org"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pr("repo", 7)
			actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{p}}}
			got := writeMergeEligible(actionable, github.HoldResult{}, "org", nil, false, nil, false, nil,
				nil, tc.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			v := got[github.MergeVerdictKey(p)]
			if v.HandMerge != tc.wantHand {
				t.Errorf("verdict = %+v, want HandMerge=%v", v, tc.wantHand)
			}
			wantState := github.MergeVerdictEligible
			if tc.wantHand {
				wantState = github.MergeVerdictOutstanding
			}
			if v.State != wantState {
				t.Errorf("state = %q, want %q", v.State, wantState)
			}
			// merge-eligible.json keeps the PR either way: the merge-request
			// relay (pkg/agent AuthorizeMerge) and the lanes apply the repo
			// switch themselves.
			if !mergeTargetEligible("org/repo", 7, "abc") {
				t.Error("PR dropped from merge-eligible.json; its readers rely on it being listed")
			}
		})
	}
}

// projectbluefin/documentation#1668 and projectbluefin/actions#697: in a
// repo whose auto-merge is off, a PR held by a Hive-side gate (a
// contributor-lane refusal, a red check nobody declared required) is still a
// hand merge, and its reason says both. A PR GitHub itself blocks stays the
// author's CI work.
func TestHandMergeVerdict(t *testing.T) {
	refusal := "contributor-lane policy refuses the merge relay for @github-actions[bot]'s PR: contributor-lane:no-linked-issue"
	cases := []struct {
		name     string
		in       github.MergeVerdict
		off      bool
		wantHand bool
		wantText []string
	}{
		{"lane refusal in an off repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: refusal}, true, true, []string{refusal, repoAutoMergeOffReason}},
		{"red optional check in an off repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "CI failing: Check consumer validation evidence"}, true, true, []string{"CI failing", repoAutoMergeOffReason}},
		{"blocked by GitHub in an off repo", github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "blocked — CI failing: zizmor"}, true, false, []string{"zizmor"}},
		{"unknown in an off repo", github.MergeVerdict{State: github.MergeVerdictUnknown}, true, false, nil},
		{"eligible verdict untouched", github.MergeVerdict{State: github.MergeVerdictEligible, Reason: "the sweep would merge this now"}, true, false, nil},
		{"outstanding in an on repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: refusal}, false, false, []string{refusal}},
		{"already a hand merge: reason not doubled", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: repoAutoMergeOffReason, HandMerge: true}, true, true, []string{repoAutoMergeOffReason}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := handMergeVerdict(tc.in, tc.off)
			if got.HandMerge != tc.wantHand || got.State != tc.in.State {
				t.Fatalf("got %+v, want HandMerge=%v state %q", got, tc.wantHand, tc.in.State)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(got.Reason, w) {
					t.Errorf("reason %q lacks %q", got.Reason, w)
				}
			}
			if strings.Count(got.Reason, repoAutoMergeOffReason) > 1 {
				t.Errorf("reason repeats the switch: %q", got.Reason)
			}
			if !tc.wantHand && got.Reason != tc.in.Reason {
				t.Errorf("reason changed without HandMerge: %q", got.Reason)
			}
		})
	}
}

// The lane refusal replaces the classifier's hand-merge verdict after
// classification; the written verdict must still carry HandMerge.
func TestWriteMergeEligible_LaneRefusalKeepsHandMerge(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})
	prevGate := mergeEligibleLaneGate
	setMergeEligibleLaneGate(func(context.Context, string, int, string) (string, error) {
		return "contributor-lane policy refuses the merge relay: contributor-lane:no-linked-issue", nil
	})
	t.Cleanup(func() { setMergeEligibleLaneGate(prevGate) })
	l5 := 5
	cfg := &config.Config{ACMMLevel: &l5, Project: config.ProjectConfig{Org: "org"}}
	p := github.PullRequest{Repo: "repo", Number: 1668, Author: "github-actions[bot]", Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", HeadSHA: "abc"}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{p}}}
	got := writeMergeEligible(actionable, github.HoldResult{}, "org", nil, false, nil, false, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	v := got[github.MergeVerdictKey(p)]
	if !v.HandMerge || v.State != github.MergeVerdictOutstanding || !strings.Contains(v.Reason, "no-linked-issue") || !strings.Contains(v.Reason, "auto-merge is off") {
		t.Fatalf("verdict = %+v, want an outstanding hand merge naming the refusal and the switch", v)
	}
}

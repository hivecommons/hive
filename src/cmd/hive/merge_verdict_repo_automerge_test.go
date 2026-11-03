package main

import (
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

	// An earlier gate still wins: a red PR in an off repo is CI work, not a
	// hand merge.
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

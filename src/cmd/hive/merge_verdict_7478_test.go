package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
)

// The PR pill's green must mean what the sweep means by eligible
// (hivecommons/hive#7478). classifyMergeEligibility is the one rule behind
// merge-eligible.json; these tests pin the MergeVerdict it hands the
// dashboard for each branch of that rule, so a pill can never be painted
// green for a PR the sweep would refuse — and, the other way, so a PR in the
// eligible bucket is never shown as anything but green.

func mergeVerdictOf(t *testing.T, pr github.PullRequest, held bool, g mergeGates) (mergeBucket, github.MergeVerdict) {
	t.Helper()
	bucket, verdict, _ := classifyMergeEligibility(pr, held, "org/repo", g)
	return bucket, verdict
}

func TestClassifyMergeEligibility_VerdictTracksBucket(t *testing.T) {
	yes := github.MergeableYes
	cases := []struct {
		name       string
		pr         github.PullRequest
		held       bool
		gates      mergeGates
		wantBucket mergeBucket
		wantState  github.MergeVerdictState
		wantReason []string // substrings
	}{
		{
			// The two PRs the issue was filed on: GitHub "unstable" (no
			// conflicts, nothing REQUIRED red), one failing check, no
			// required-check set declared. Before #7478 the pill was green;
			// the sweep would never merge them.
			name:       "unstable with a red check and no required set is outstanding, not eligible",
			pr:         github.PullRequest{Number: 1253, Mergeable: yes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}},
			wantBucket: mergeBucketFailing,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"CI failing", "build", "required_checks"},
		},
		{
			name:       "clean and green is eligible",
			pr:         github.PullRequest{Number: 1242, Mergeable: yes, MergeableState: "clean", CIStatus: "success"},
			wantBucket: mergeBucketEligible,
			wantState:  github.MergeVerdictEligible,
			wantReason: []string{"would merge"},
		},
		{
			// Red only on a non-required check WITH a required set: the
			// sweep merges it, so it is green — and the tooltip names the
			// red optional check so green does not read as all-green.
			name:       "only optional checks red with a required set is eligible and says which are red",
			pr:         github.PullRequest{Number: 2, Mergeable: yes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"playwright"}},
			gates:      mergeGates{requiredChecks: map[string]bool{"build": true}},
			wantBucket: mergeBucketEligible,
			wantState:  github.MergeVerdictEligible,
			wantReason: []string{"non-required", "playwright"},
		},
		{
			name:       "a required check red with a required set is outstanding",
			pr:         github.PullRequest{Number: 3, Mergeable: yes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}},
			gates:      mergeGates{requiredChecks: map[string]bool{"build": true}},
			wantBucket: mergeBucketFailing,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"CI failing", "build"},
		},
		{
			name:       "held and green is outstanding, naming the hold",
			pr:         github.PullRequest{Number: 4, Mergeable: yes, MergeableState: "clean", CIStatus: "success"},
			held:       true,
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"held"},
		},
		{
			name:       "pending with non-required checks outstanding is eligible (the 2026-08-04 rule)",
			pr:         github.PullRequest{Number: 5, Mergeable: yes, MergeableState: "unstable", CIStatus: "pending"},
			wantBucket: mergeBucketEligible,
			wantState:  github.MergeVerdictEligible,
			wantReason: []string{"pending", "unstable"},
		},
		{
			name:       "pending with mergeability unknown is unknown, not amber, and keeps the sweep reason",
			pr:         github.PullRequest{Number: 6, CIStatus: "pending"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictUnknown,
			wantReason: []string{"not yet computed by GitHub", "re-checked next tick", "CI pending"},
		},
		{
			// Conflicts read as what to do, name the base branch, and keep
			// the sweep's own reason — it still stands after the rebase.
			name:       "dirty is blocked, names the base branch and the fix, and keeps the sweep reason",
			pr:         github.PullRequest{Number: 1259, Mergeable: github.MergeableNo, MergeableState: "dirty", BaseRef: "v4", CIStatus: "failure", FailingChecks: []string{"build"}},
			wantBucket: mergeBucketFailing,
			wantState:  github.MergeVerdictBlocked,
			wantReason: []string{"merge conflicts with v4", "needs a rebase", "CI failing: build"},
		},
		{
			// GitHub "blocked" with every sweep gate green: the sweep cannot
			// name the rule yet, but must say that, not just "blocked".
			name:       "blocked with green CI is blocked and says a branch-protection rule is unsatisfied",
			pr:         github.PullRequest{Number: 603, Mergeable: github.MergeableNo, MergeableState: "blocked", CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: []string{"blocked — all sweep gates pass", "branch-protection rule"},
		},
		{
			name:       "a draft is blocked and says how to enter the sweep",
			pr:         github.PullRequest{Number: 7, Draft: true, Mergeable: yes, CIStatus: "success"},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: []string{"draft — mark ready for review"},
		},
		{
			name:       "review approval required and missing is outstanding",
			pr:         github.PullRequest{Number: 8, Mergeable: yes, MergeableState: "clean", CIStatus: "success"},
			gates:      mergeGates{requireReviewApproval: true, reviewLoaded: true},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"review approval"},
		},
		{
			name:       "review artifact unavailable fails closed as outstanding and says so",
			pr:         github.PullRequest{Number: 9, Mergeable: yes, MergeableState: "clean", CIStatus: "success"},
			gates:      mergeGates{requireReviewApproval: true, reviewLoaded: false},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"review-verdicts.json"},
		},
		{
			name: "intent verification refusing an agent PR is outstanding with the intent reason",
			pr:   github.PullRequest{Number: 10, Mergeable: yes, MergeableState: "clean", CIStatus: "success"},
			gates: mergeGates{enforceIntent: true, intentVerdicts: map[string]intent.Verdict{
				"org/repo/10": {AgentPR: true, Authorized: false, Reason: "no authorizing issue"},
			}},
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: []string{"intent", "no authorizing issue"},
		},
		{
			// A conflicting PR the sweep also refuses for another reason is
			// still BLOCKED: the conflict is what the operator resolves first
			// — and the hold is still named, since it outlives the rebase.
			name:       "held AND dirty is blocked, not amber, and still names the hold",
			pr:         github.PullRequest{Number: 11, Mergeable: github.MergeableNo, MergeableState: "dirty", CIStatus: "success"},
			held:       true,
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictBlocked,
			wantReason: []string{"merge conflicts with the base branch", "held"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict := mergeVerdictOf(t, tc.pr, tc.held, tc.gates)
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %v, want %v", bucket, tc.wantBucket)
			}
			if verdict.State != tc.wantState {
				t.Errorf("verdict state = %q, want %q (reason %q)", verdict.State, tc.wantState, verdict.Reason)
			}
			for _, want := range tc.wantReason {
				if !strings.Contains(verdict.Reason, want) {
					t.Errorf("verdict reason %q missing %q", verdict.Reason, want)
				}
			}
			// The invariant the pill relies on, both ways: green iff the
			// eligible bucket.
			if (bucket == mergeBucketEligible) != (verdict.State == github.MergeVerdictEligible) {
				t.Errorf("bucket %v and verdict %q disagree about eligibility", bucket, verdict.State)
			}
		})
	}
}

// writeMergeEligible returns the verdicts keyed the way the dashboard looks
// them up, for every PR it saw — Items and Held alike — so no pill is left
// to the GitHub-flag fallback.
func TestWriteMergeEligible_ReturnsVerdictsForEveryPR(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	green := github.PullRequest{Repo: "repo", Number: 1, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"}
	red := github.PullRequest{Repo: "repo", Number: 2, Mergeable: github.MergeableYes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}}
	heldGreen := github.PullRequest{Repo: "other", Number: 3, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success"}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{green, red}, Held: []github.PullRequest{heldGreen}}}

	got := writeMergeEligible(actionable, github.HoldResult{}, "org", nil, false, nil, false, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	want := map[string]github.MergeVerdictState{
		github.MergeVerdictKey(green):     github.MergeVerdictEligible,
		github.MergeVerdictKey(red):       github.MergeVerdictOutstanding,
		github.MergeVerdictKey(heldGreen): github.MergeVerdictOutstanding,
	}
	if len(got) != len(want) {
		t.Fatalf("verdicts = %+v, want one per PR: %v", got, want)
	}
	for key, state := range want {
		v, ok := got[key]
		if !ok {
			t.Errorf("no verdict under %q; keys: %v", key, keysOf(got))
			continue
		}
		if v.State != state {
			t.Errorf("%s: state = %q, want %q (reason %q)", key, v.State, state, v.Reason)
		}
	}
	if !strings.Contains(got[github.MergeVerdictKey(heldGreen)].Reason, "held") {
		t.Errorf("held PR's reason does not name the hold: %q", got[github.MergeVerdictKey(heldGreen)].Reason)
	}
}

func keysOf(m map[string]github.MergeVerdict) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

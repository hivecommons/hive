package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
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
		{"soak refusal in an off repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "contributor-lane policy refuses the merge relay: contributor-lane:soaking(remaining=12m0s)"}, true, true, []string{"contributor-lane:soaking", repoAutoMergeOffReason}},
		{"awaiting review in an off repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: "awaiting review approval"}, true, true, []string{"awaiting review approval; " + repoAutoMergeOffReason}},
		{"review returned the head for changes in an off repo", github.MergeVerdict{State: github.MergeVerdictOutstanding, Reason: reviewChangesRequestedReason, ReviewChangesRequested: true}, true, false, []string{"changes requested"}},
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

// The review gate fails both for a head the hive's review has not judged yet
// and for one it returned for changes. Only the first is "awaiting review
// approval" and, in a repo whose auto-merge is off, a hand merge; the second
// says the review asked for changes and is never offered to a person as a
// merge. A requires_human or reject head keeps the awaiting wording and the
// hand merge: the review's own hold reaches a person first, and after a
// release the hand merge is the ask. Buckets do not change.
func TestReviewGateSplitsNotYetReviewedFromChangesRequested(t *testing.T) {
	const head = "c0ffee0123456789abcdef0123456789abcdef01"
	recorded := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	agg := func(v review.Verdict, sha string, eligible bool) review.Aggregate {
		return review.Aggregate{Repo: "org/repo", Number: 7, HeadSHA: sha, Verdict: v, MergeEligible: eligible, RecordedAt: recorded}
	}
	green := github.PullRequest{Number: 7, Mergeable: github.MergeableYes, MergeableState: "clean", CIStatus: "success", HeadSHA: head}
	cases := []struct {
		name       string
		items      []review.Aggregate
		wantBucket mergeBucket
		wantFlag   bool
		// wantOff and wantOn are the exact reasons written for a repo whose
		// auto-merge is off and on.
		wantOff, wantOn string
		wantOffHand     bool
	}{
		{"no verdict for any head", nil, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"changes requested on an earlier head only", []review.Aggregate{agg(review.VerdictChangesRequested, "0123456789abcdef", false)}, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"incomplete verdict on this head", []review.Aggregate{agg(review.VerdictIncomplete, head, false)}, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"changes requested on this head", []review.Aggregate{agg(review.VerdictChangesRequested, head, false)}, mergeBucketSkip, true,
			"the hive's review did not approve this head (changes requested)", "the hive's review did not approve this head (changes requested)", false},
		{"changes requested on this head, held for a person", []review.Aggregate{func() review.Aggregate {
			a := agg(review.VerdictChangesRequested, head, false)
			a.RequiresHuman = true
			return a
		}()}, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"requires_human on this head", []review.Aggregate{agg(review.VerdictRequiresHuman, head, false)}, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"reject on this head", []review.Aggregate{agg(review.VerdictReject, head, false)}, mergeBucketSkip, false,
			"awaiting review approval; " + repoAutoMergeOffReason, "awaiting review approval", true},
		{"approved on this head, every other gate passing", []review.Aggregate{agg(review.VerdictApprove, head, true)}, mergeBucketEligible, false,
			repoAutoMergeOffReason, "the sweep would merge this now", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, off := range []bool{true, false} {
				g := mergeGates{requireReviewApproval: true, reviewLoaded: true, reviewArtifact: review.Artifact{Items: tc.items}, repoAutoMergeOff: off}
				bucket, verdict := mergeVerdictOf(t, green, false, g)
				// writeMergeEligible applies handMergeVerdict to every verdict.
				verdict = handMergeVerdict(verdict, off)
				if bucket != tc.wantBucket {
					t.Errorf("off=%v: bucket = %v, want %v", off, bucket, tc.wantBucket)
				}
				if verdict.ReviewChangesRequested != tc.wantFlag {
					t.Errorf("off=%v: ReviewChangesRequested = %v, want %v (%+v)", off, verdict.ReviewChangesRequested, tc.wantFlag, verdict)
				}
				wantReason, wantHand := tc.wantOn, false
				if off {
					wantReason, wantHand = tc.wantOff, tc.wantOffHand
				}
				if verdict.HandMerge != wantHand {
					t.Errorf("off=%v: HandMerge = %v, want %v (%+v)", off, verdict.HandMerge, wantHand, verdict)
				}
				if verdict.Reason != wantReason {
					t.Errorf("off=%v: reason = %q, want %q", off, verdict.Reason, wantReason)
				}
				wantState := github.MergeVerdictOutstanding
				if !off && tc.wantBucket == mergeBucketEligible {
					wantState = github.MergeVerdictEligible
				}
				if verdict.State != wantState {
					t.Errorf("off=%v: state = %q, want %q", off, verdict.State, wantState)
				}
			}
		})
	}

	// GitHub's own refusal still leads the wording of a judged head.
	blocked := green
	blocked.Mergeable, blocked.MergeableState = github.MergeableNo, "blocked"
	g := mergeGates{requireReviewApproval: true, reviewLoaded: true, reviewArtifact: review.Artifact{Items: []review.Aggregate{agg(review.VerdictChangesRequested, head, false)}}, repoAutoMergeOff: true}
	bucket, verdict := mergeVerdictOf(t, blocked, false, g)
	verdict = handMergeVerdict(verdict, true)
	if bucket != mergeBucketSkip || verdict.State != github.MergeVerdictBlocked || verdict.HandMerge || !verdict.ReviewChangesRequested ||
		!strings.Contains(verdict.Reason, "the hive's review did not approve this head (changes requested)") {
		t.Errorf("blocked judged head: bucket %v verdict %+v, want a blocked, flagged, non-hand-merge verdict naming the review", bucket, verdict)
	}
}

// A returned head is flagged whichever gate stops it first. A red check
// nobody declared required stops a mergeable PR before the review gate; in a
// repo whose auto-merge is off that verdict used to become a hand merge even
// though the review had returned the head for changes. The earlier gate keeps
// its bucket and its reason.
func TestReviewChangesRequestedFlagIgnoresGateOrder(t *testing.T) {
	const head = "c0ffee0123456789abcdef0123456789abcdef01"
	red := github.PullRequest{Number: 7, Mergeable: github.MergeableYes, MergeableState: "unstable", CIStatus: "failure", FailingChecks: []string{"build"}, HeadSHA: head}
	returned := review.Artifact{Items: []review.Aggregate{{Repo: "org/repo", Number: 7, HeadSHA: head, Verdict: review.VerdictChangesRequested, RecordedAt: time.Now()}}}

	g := mergeGates{requireReviewApproval: true, reviewLoaded: true, reviewArtifact: returned, repoAutoMergeOff: true}
	bucket, verdict := mergeVerdictOf(t, red, false, g)
	written := handMergeVerdict(verdict, true)
	if bucket != mergeBucketFailing {
		t.Errorf("bucket = %v, want failing: the red check still leads", bucket)
	}
	if !written.ReviewChangesRequested || written.HandMerge {
		t.Errorf("verdict = %+v, want ReviewChangesRequested and no HandMerge", written)
	}
	if written.Reason != verdict.Reason || strings.Contains(written.Reason, "did not approve") || !strings.Contains(written.Reason, "build") {
		t.Errorf("reason = %q, want the red check's own reason, unchanged", written.Reason)
	}

	// The same red PR with no verdict on its head is still a hand merge.
	g.reviewArtifact = review.Artifact{}
	_, verdict = mergeVerdictOf(t, red, false, g)
	if written := handMergeVerdict(verdict, true); written.ReviewChangesRequested || !written.HandMerge {
		t.Errorf("unreviewed red head: verdict %+v, want a hand merge", written)
	}

	// review.require_approval off: the review gate never reads the artifact,
	// and neither does the flag.
	g = mergeGates{reviewArtifact: returned, repoAutoMergeOff: true}
	_, verdict = mergeVerdictOf(t, red, false, g)
	if verdict.ReviewChangesRequested {
		t.Errorf("require_approval off: verdict %+v, want no flag", verdict)
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

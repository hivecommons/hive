package main

// The merge-request relay lands a PR only when the governor's classifier
// filed it in merge-eligible.json (bindMergeAuthz) AND the lane-policy gate
// (relayLanePolicyGate → automerge.Engine.RelayMergeRefusal) lets it
// through. These tests pin the classifier half for the control labels the
// relay must honour — sentinel-alert, hold and the needs-decision parking
// stages — end to end through writeMergeEligible + bindMergeAuthz, and the
// cmd-side wiring of the lane gate.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
)

func TestClassifyMergeEligibility_RelayControlLabels(t *testing.T) {
	green := func(labels ...string) github.PullRequest {
		return github.PullRequest{
			Number:    401,
			Repo:      "docs",
			Author:    "Karibhou",
			CIStatus:  "success",
			Mergeable: github.MergeableYes,
			HeadSHA:   "cafe",
			Labels:    labels,
		}
	}
	cases := []struct {
		name       string
		pr         github.PullRequest
		wantBucket mergeBucket
		wantState  github.MergeVerdictState
		wantReason string
	}{
		{
			name:       "contributor PR at stage/060 with no control label is eligible (the lane gate decides tier and soak)",
			pr:         green("stage/060-reviewed", "contributor-lane:claim-tier-untrusted"),
			wantBucket: mergeBucketEligible,
			wantState:  github.MergeVerdictEligible,
		},
		{
			name:       "sentinel-alert blocks a contributor PR",
			pr:         green("sentinel-alert"),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: "sentinel",
		},
		{
			name:       "hold blocks",
			pr:         green("hold"),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: "label:hold",
		},
		{
			name:       "stage/045-needs-decision parks the PR on a person",
			pr:         green(github.NeedsDecisionStageLabel),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: "label:stage/045-needs-decision — parked on a human decision",
		},
		{
			name:       "stage/085-needs-decision parks the PR on a person",
			pr:         green(github.PRNeedsDecisionStageLabel),
			wantBucket: mergeBucketSkip,
			wantState:  github.MergeVerdictOutstanding,
			wantReason: "label:stage/085-needs-decision — parked on a human decision",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, verdict, _ := classifyMergeEligibility(tc.pr, false, "hivecommons/docs", mergeGates{})
			if bucket != tc.wantBucket {
				t.Fatalf("bucket = %d, want %d (verdict %+v)", bucket, tc.wantBucket, verdict)
			}
			if verdict.State != tc.wantState {
				t.Fatalf("state = %q, want %q (reason %q)", verdict.State, tc.wantState, verdict.Reason)
			}
			if tc.wantReason != "" && verdict.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", verdict.Reason, tc.wantReason)
			}
		})
	}
}

// A parked RED PR is still its author's to repair: the park sits after the
// red classification exactly like the hold does (hivecommons/hive#7438).
func TestClassifyMergeEligibility_ParkedRedPRStaysFailing(t *testing.T) {
	pr := github.PullRequest{
		Number: 7, Repo: "docs", Author: "Karibhou", CIStatus: "failure", FailingChecks: []string{"build"},
		Mergeable: github.MergeableNo, MergeableState: "blocked",
		Labels: []string{github.PRNeedsDecisionStageLabel},
	}
	bucket, verdict, _ := classifyMergeEligibility(pr, false, "hivecommons/docs", mergeGates{})
	if bucket != mergeBucketFailing {
		t.Fatalf("bucket = %d, want failing (verdict %+v)", bucket, verdict)
	}
}

// A parked CONFLICTING PR is still its fixer's to rebase: the conflict
// classification sits before the park (hivecommons/hive#14822 sat 14h at
// 085 behind v5 with no lane owning the rebase).
func TestClassifyMergeEligibility_ParkedConflictingPRStaysConflict(t *testing.T) {
	pr := github.PullRequest{
		Number: 14822, Repo: "hive", Author: "thatguywob", CIStatus: "success",
		Mergeable: github.MergeableNo, MergeableState: "dirty",
		Labels: []string{github.PRNeedsDecisionStageLabel, "needs-rebase"},
	}
	bucket, verdict, _ := classifyMergeEligibility(pr, false, "hivecommons/hive", mergeGates{})
	if bucket != mergeBucketConflict {
		t.Fatalf("bucket = %d, want conflict (verdict %+v)", bucket, verdict)
	}
	if verdict.State != github.MergeVerdictBlocked {
		t.Fatalf("state = %q, want blocked", verdict.State)
	}
}

func TestNeedsDecisionParkLabel(t *testing.T) {
	if got := needsDecisionParkLabel([]string{"stage/060-reviewed", " Stage/085-Needs-Decision "}); got != "stage/085-needs-decision" {
		t.Fatalf("got %q", got)
	}
	if got := needsDecisionParkLabel([]string{"stage/045-needs-decision"}); got != "stage/045-needs-decision" {
		t.Fatalf("got %q", got)
	}
	if got := needsDecisionParkLabel([]string{"needs-decision", "stage/090-ready-for-human-merge"}); got != "" {
		t.Fatalf("got %q, want no park", got)
	}
}

// End to end through the governor's artifact: a PR carrying a control label
// never reaches merge-eligible.json, so bindMergeAuthz refuses the relay
// request for it, while the same PR without the label is accepted.
func TestBindMergeAuthz_RefusesParkedHeldAndSentinelTargets(t *testing.T) {
	const sha = "cccccccccccccccccccccccccccccccccccccccc"
	mk := func(number int, labels ...string) github.PullRequest {
		return github.PullRequest{Number: number, Repo: "docs", Author: "Karibhou", CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: sha, Labels: labels}
	}
	prs := []github.PullRequest{
		mk(1, "stage/060-reviewed"),
		mk(2, "sentinel-alert"),
		mk(3, "hold"),
		mk(4, github.NeedsDecisionStageLabel),
		mk(5, github.PRNeedsDecisionStageLabel),
	}
	eligible, _ := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "hivecommons"})
	if len(eligible) != 1 || eligible[0].Number != 1 {
		t.Fatalf("eligible = %+v, want only #1", eligible)
	}
	allow := func(agent string, fileUID int, repo string) error { return nil }
	authz := bindMergeAuthz(allow)
	if err := authz("scanner", 1001, "hivecommons/docs", 1, sha); err != nil {
		t.Fatalf("#1 should pass the target binding: %v", err)
	}
	for _, number := range []int{2, 3, 4, 5} {
		err := authz("scanner", 1001, "hivecommons/docs", number, sha)
		if err == nil || !strings.Contains(err.Error(), "not in the current merge-eligible list") {
			t.Fatalf("#%d: err = %v, want merge-eligible refusal", number, err)
		}
	}
}

func TestRelayLanePolicyGateWiring(t *testing.T) {
	build := relayLanePolicyGate(automerge.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if gate := build(nil); gate != nil {
		t.Fatal("nil client must install no gate")
	}
	client := github.NewClientForTest("http://127.0.0.1:0", "hivecommons", []string{"docs"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gate := build(client)
	if gate == nil {
		t.Fatal("a live client must get the engine's RelayMergeRefusal")
	}
	// The gate on an unreachable forge fails closed with an error, never a
	// silent allow.
	if refusal, err := gate(context.Background(), "hivecommons/docs", 401, "cafe"); err == nil || refusal != "" {
		t.Fatalf("refusal = %q, err = %v; want fail-closed error", refusal, err)
	}
}

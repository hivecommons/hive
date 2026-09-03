package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// writeMergeEligible decides which PRs the sweep may merge and which go to
// the fix loop. Every exclusion branch in it is annotated with a dated
// production incident (three green PRs frozen for hours on 2026-08-04, 16
// dependabot PRs accumulating for 11 days on 2026-08-28, DIRTY bumps pinning
// the eligible count on 2026-08-31), and it has been patched by a dozen fix
// PRs — yet the only test exercised the intent-verdict branch. These tests pin
// each bucket decision so the next patch to one branch cannot silently move a
// PR between buckets on another.

type eligibleEntry struct {
	Number    int      `json:"number"`
	Repo      string   `json:"repo"`
	Labels    []string `json:"labels"`
	Mergeable string   `json:"mergeable"`
	DCO       string   `json:"dco"`
	HeadSHA   string   `json:"head_sha"`
}

type failingEntry struct {
	Number        int      `json:"number"`
	Repo          string   `json:"repo"`
	HeadSHA       string   `json:"head_sha"`
	FailingChecks []string `json:"failing_checks"`
	Excerpt       string   `json:"excerpt"`
	Escalated     bool     `json:"escalated"`
}

type mergeEligibleInputs struct {
	hold           github.HoldResult
	org            string
	escalated      map[string]bool
	requireReview  bool
	requiredChecks map[string]bool
}

// runWriteMergeEligible points the two output seams at a TempDir, runs
// writeMergeEligible with intent enforcement OFF (that branch has its own test
// in intent_alignment_gate_test.go), and returns both decoded buckets.
func runWriteMergeEligible(t *testing.T, prs []github.PullRequest, in mergeEligibleInputs) ([]eligibleEntry, []failingEntry) {
	t.Helper()
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: prs}}
	writeMergeEligible(actionable, in.hold, in.org, in.escalated, false, nil, in.requireReview, in.requiredChecks,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	var mergePayload struct {
		GeneratedAt   string          `json:"generated_at"`
		MergeEligible []eligibleEntry `json:"merge_eligible"`
	}
	readJSON(t, mergeEligiblePath, &mergePayload)
	if _, err := time.Parse(time.RFC3339, mergePayload.GeneratedAt); err != nil {
		t.Errorf("merge-eligible generated_at %q is not RFC3339: %v", mergePayload.GeneratedAt, err)
	}

	var failPayload struct {
		GeneratedAt string         `json:"generated_at"`
		CIFailing   []failingEntry `json:"ci_failing"`
	}
	readJSON(t, ciFailingPath, &failPayload)
	if _, err := time.Parse(time.RFC3339, failPayload.GeneratedAt); err != nil {
		t.Errorf("ci-failing generated_at %q is not RFC3339: %v", failPayload.GeneratedAt, err)
	}
	return mergePayload.MergeEligible, failPayload.CIFailing
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, raw)
	}
}

type bucket int

const (
	bucketNeither bucket = iota
	bucketEligible
	bucketFailing
)

func (b bucket) String() string {
	switch b {
	case bucketEligible:
		return "eligible"
	case bucketFailing:
		return "ci_failing"
	}
	return "neither"
}

// TestWriteMergeEligible_BucketDecisions is the truth table: one PR per case,
// which bucket it lands in, and which incident the row guards.
func TestWriteMergeEligible_BucketDecisions(t *testing.T) {
	required := map[string]bool{"build": true, "test": true}
	cases := []struct {
		name     string
		pr       github.PullRequest
		in       mergeEligibleInputs
		want     bucket
		guarding string
	}{
		{
			name: "green and mergeable is eligible",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 1, CIStatus: "success", Mergeable: github.MergeableYes},
			want: bucketEligible,
		},
		{
			name: "draft is never listed anywhere",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 2, Draft: true, CIStatus: "success", Mergeable: github.MergeableYes},
			want: bucketNeither,
		},
		{
			name: "held PR is never listed anywhere",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 3, CIStatus: "success", Mergeable: github.MergeableYes},
			in:   mergeEligibleInputs{hold: github.HoldResult{Items: []github.HoldItem{{Repo: "kubestellar/hive", Number: 3}}}},
			want: bucketNeither,
		},
		{
			name:     "red with no required-check set fails closed into ci_failing",
			pr:       github.PullRequest{Repo: "kubestellar/hive", Number: 4, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright"}},
			want:     bucketFailing,
			guarding: "no required set → cannot tell optional from required → old fail-closed behavior",
		},
		{
			name:     "red only on optional checks and mergeable is eligible",
			pr:       github.PullRequest{Repo: "kubestellar/hive", Number: 5, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright", "coverage"}},
			in:       mergeEligibleInputs{requiredChecks: required},
			want:     bucketEligible,
			guarding: "2026-08-28: 16 dependabot PRs parked in ci-failing.json for 11 days behind perma-red optional shards",
		},
		{
			name: "red on a required check is ci_failing even with the set configured",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 6, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright", "test"}},
			in:   mergeEligibleInputs{requiredChecks: required},
			want: bucketFailing,
		},
		{
			name:     "red only on optional checks but mergeability unknown stays ci_failing",
			pr:       github.PullRequest{Repo: "kubestellar/hive", Number: 7, CIStatus: "failure", Mergeable: github.MergeableUnknown, FailingChecks: []string{"playwright"}},
			in:       mergeEligibleInputs{requiredChecks: required},
			want:     bucketFailing,
			guarding: "the optional-red carve-out needs GitHub's own mergeable verdict, not just the check names",
		},
		{
			name:     "pending but mergeable is eligible",
			pr:       github.PullRequest{Repo: "kubestellar/hive", Number: 8, CIStatus: "pending", Mergeable: github.MergeableYes},
			want:     bucketEligible,
			guarding: "2026-08-04: three green console PRs frozen for hours waiting on tide/coverage that never complete",
		},
		{
			name: "pending with unknown mergeability is neither",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 9, CIStatus: "pending", Mergeable: github.MergeableUnknown},
			want: bucketNeither,
		},
		{
			name: "pending and conflicting is neither",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 10, CIStatus: "pending", Mergeable: github.MergeableNo},
			want: bucketNeither,
		},
		{
			name:     "green but conflicting is neither",
			pr:       github.PullRequest{Repo: "kubestellar/hive", Number: 11, CIStatus: "success", Mergeable: github.MergeableNo},
			want:     bucketNeither,
			guarding: "2026-08-31: DIRTY go.mod bumps pinned the eligible count at N while nothing could merge",
		},
		{
			name: "green with unknown mergeability is eligible (list endpoint never populates it)",
			pr:   github.PullRequest{Repo: "kubestellar/hive", Number: 12, CIStatus: "success", Mergeable: github.MergeableUnknown},
			want: bucketEligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eligible, failing := runWriteMergeEligible(t, []github.PullRequest{tc.pr}, tc.in)
			got := bucketNeither
			switch {
			case len(eligible) == 1 && len(failing) == 0:
				got = bucketEligible
			case len(failing) == 1 && len(eligible) == 0:
				got = bucketFailing
			case len(eligible) == 0 && len(failing) == 0:
				got = bucketNeither
			default:
				t.Fatalf("PR landed in both buckets: eligible=%+v failing=%+v", eligible, failing)
			}
			if got != tc.want {
				t.Fatalf("PR #%d landed in %s, want %s (%s)", tc.pr.Number, got, tc.want, tc.guarding)
			}
		})
	}
}

// The failing bucket has to carry the CI evidence to the fix agent: the check
// names, the excerpt, and the escalation flag — otherwise the kick says "red"
// and nothing else.
func TestWriteMergeEligible_FailingEntryCarriesEvidenceAndEscalation(t *testing.T) {
	pr := github.PullRequest{
		Repo: "hive", Number: 42, CIStatus: "failure", Mergeable: github.MergeableYes,
		HeadSHA: "deadbeef", FailingChecks: []string{"build-and-test"}, CIFailureExcerpt: "main.go:12: undefined: foo",
	}
	escalated := map[string]bool{escalation.Key("kubestellar/hive", 42): true}
	eligible, failing := runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "kubestellar", escalated: escalated})
	if len(eligible) != 0 || len(failing) != 1 {
		t.Fatalf("eligible=%+v failing=%+v, want exactly one failing entry", eligible, failing)
	}
	got := failing[0]
	if got.Repo != "kubestellar/hive" {
		t.Errorf("bare repo %q was not org-qualified: got %q", pr.Repo, got.Repo)
	}
	if got.HeadSHA != "deadbeef" {
		t.Errorf("head_sha = %q, want deadbeef", got.HeadSHA)
	}
	if len(got.FailingChecks) != 1 || got.FailingChecks[0] != "build-and-test" {
		t.Errorf("failing_checks = %v, want [build-and-test]", got.FailingChecks)
	}
	if got.Excerpt != pr.CIFailureExcerpt {
		t.Errorf("excerpt = %q, want %q", got.Excerpt, pr.CIFailureExcerpt)
	}
	if !got.Escalated {
		t.Error("escalated = false; the flag keyed by escalation.Key(org/repo, number) was not applied")
	}

	// Positive control for the escalation lookup: the same PR without an
	// escalation record must NOT be flagged, or the assertion above would pass
	// for a function that flags everything.
	_, failing = runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "kubestellar"})
	if len(failing) != 1 || failing[0].Escalated {
		t.Fatalf("un-escalated PR reported escalated: %+v", failing)
	}
}

// Eligible entries carry the fields the merge step and relay compare against:
// the org-qualified repo, the head SHA (CWE-367 guard), the tri-state
// mergeability as a string (a bool defaulted every PR to false, #M4), and the
// DCO verdict decoded from the dco-signoff labels.
func TestWriteMergeEligible_EligibleEntryFields(t *testing.T) {
	prs := []github.PullRequest{
		{Repo: "hive", Number: 1, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "abc123", Labels: []string{"dco-signoff: yes", "kind/bug"}},
		{Repo: "kubestellar/console", Number: 2, CIStatus: "success", Mergeable: github.MergeableYes, Labels: []string{"dco-signoff: no"}},
		{Repo: "hive", Number: 3, CIStatus: "success", Mergeable: github.MergeableUnknown},
	}
	eligible, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "kubestellar"})
	if len(failing) != 0 {
		t.Fatalf("unexpected ci_failing entries: %+v", failing)
	}
	if len(eligible) != 3 {
		t.Fatalf("eligible = %+v, want 3 entries", eligible)
	}
	byNumber := map[int]eligibleEntry{}
	for _, e := range eligible {
		byNumber[e.Number] = e
	}
	if e := byNumber[1]; e.Repo != "kubestellar/hive" || e.HeadSHA != "abc123" || e.Mergeable != string(github.MergeableYes) || e.DCO != "yes" || len(e.Labels) != 2 {
		t.Errorf("PR #1 entry = %+v", e)
	}
	if e := byNumber[2]; e.Repo != "kubestellar/console" || e.DCO != "no" {
		t.Errorf("PR #2 entry = %+v (already-qualified repo must not be double-prefixed)", e)
	}
	if e := byNumber[3]; e.Mergeable != mergeableJSONUnknown || e.DCO != "unknown" {
		t.Errorf("PR #3 entry = %+v (unknown mergeability must be spelled out, not the empty string)", e)
	}
}

// requireReviewApproval fails CLOSED: with no verdict artifact nothing is
// eligible, and with one, only the PR whose approval is recorded AT its
// current head SHA is.
func TestWriteMergeEligible_ReviewApprovalFailsClosed(t *testing.T) {
	origPath := review.ReviewVerdictsPath
	t.Cleanup(func() { review.ReviewVerdictsPath = origPath })
	dir := t.TempDir()
	review.ReviewVerdictsPath = filepath.Join(dir, "review-verdicts.json")

	prs := []github.PullRequest{
		{Repo: "kubestellar/hive", Number: 1, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "sha-1"},
		{Repo: "kubestellar/hive", Number: 2, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "sha-2-moved"},
	}

	// No artifact on disk: every green PR is withheld.
	eligible, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{requireReview: true})
	if len(eligible) != 0 || len(failing) != 0 {
		t.Fatalf("with no review artifact: eligible=%+v failing=%+v, want both empty", eligible, failing)
	}

	// Positive control: the same PRs without the requirement are all eligible,
	// so the empty result above is the gate and not a broken fixture.
	eligible, _ = runWriteMergeEligible(t, prs, mergeEligibleInputs{})
	if len(eligible) != 2 {
		t.Fatalf("without the requirement eligible = %+v, want both PRs", eligible)
	}

	// Approval for #1 at its head, and for #2 at a head that has since moved.
	artifact := review.Artifact{GeneratedAt: time.Now(), Items: []review.Aggregate{
		{Repo: "kubestellar/hive", Number: 1, HeadSHA: "sha-1", Verdict: review.VerdictApprove, MergeEligible: true},
		{Repo: "kubestellar/hive", Number: 2, HeadSHA: "sha-2-reviewed", Verdict: review.VerdictApprove, MergeEligible: true},
	}}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(review.ReviewVerdictsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	eligible, _ = runWriteMergeEligible(t, prs, mergeEligibleInputs{requireReview: true})
	if len(eligible) != 1 || eligible[0].Number != 1 {
		t.Fatalf("with approvals on disk eligible = %+v, want only PR #1 (PR #2's head moved after review)", eligible)
	}
}

// Both files are rewritten every cycle, including when there is nothing to
// report — a consumer must never read a stale bucket from a previous pass.
func TestWriteMergeEligible_EmptyInputStillRewritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})
	for _, p := range []string{mergeEligiblePath, ciFailingPath} {
		if err := os.WriteFile(p, []byte(`{"stale":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeMergeEligible(&github.ActionableResult{}, github.HoldResult{}, "", nil, false, nil, false, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, p := range []string{mergeEligiblePath, ciFailingPath} {
		var got map[string]any
		readJSON(t, p, &got)
		if _, stale := got["stale"]; stale {
			t.Errorf("%s was not rewritten on an empty cycle", filepath.Base(p))
		}
		if _, ok := got["generated_at"]; !ok {
			t.Errorf("%s has no generated_at: %v", filepath.Base(p), got)
		}
	}
}

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
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
	Number           int      `json:"number"`
	Repo             string   `json:"repo"`
	Agent            string   `json:"agent"`
	HeadSHA          string   `json:"head_sha"`
	FailingChecks    []string `json:"failing_checks"`
	Excerpt          string   `json:"excerpt"`
	Escalated        bool     `json:"escalated"`
	HeadRef          string   `json:"head_ref"`
	HeadRepo         string   `json:"head_repo"`
	FromFork         bool     `json:"from_fork"`
	ReachableAction  string   `json:"reachable_action"`
	Held             bool     `json:"held"`
	MergeableState   string   `json:"mergeable_state"`
	Conflict         bool     `json:"conflict"`
	ReroutedFrom     string   `json:"rerouted_from"`
	DeferredIncident int      `json:"deferred_incident"`
}

type mergeEligibleInputs struct {
	// heldPRs is the PRs.Held population: PRs the hold gate removed from
	// Items, which must still be classified as red work (hivecommons/hive#7438).
	heldPRs        []github.PullRequest
	hold           github.HoldResult
	org            string
	escalated      map[string]bool
	requireReview  bool
	requiredChecks map[string]bool
	cfg            *config.Config
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

	actionable := &github.ActionableResult{PRs: github.PRResult{Items: prs, Held: in.heldPRs}}
	writeMergeEligible(actionable, in.hold, in.org, in.escalated, false, nil, in.requireReview, in.requiredChecks,
		nil, in.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 1, CIStatus: "success", Mergeable: github.MergeableYes},
			want: bucketEligible,
		},
		{
			name:     "L6 green agent PR with hive provenance label is eligible",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 8851, Labels: []string{"agent/scanner", "hive/h1"}, CIStatus: "success", Mergeable: github.MergeableYes},
			want:     bucketEligible,
			guarding: "hivecommons/hive#8851: hive/<id> provenance must not be a hold that hides L6 agent PRs from merge-eligible.json",
		},
		{
			name: "draft is never listed anywhere",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 2, Draft: true, CIStatus: "success", Mergeable: github.MergeableYes},
			want: bucketNeither,
		},
		{
			name: "held green PR is never listed anywhere",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 3, CIStatus: "success", Mergeable: github.MergeableYes},
			in:   mergeEligibleInputs{hold: github.HoldResult{Items: []github.HoldItem{{Repo: "hivecommons/hive", Number: 3}}}},
			want: bucketNeither,
		},
		{
			name:     "held RED PR is ci_failing — the hold is a merge checkpoint, not a repair checkpoint",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 33, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"Python Unit Tests"}},
			in:       mergeEligibleInputs{hold: github.HoldResult{Items: []github.HoldItem{{Repo: "hivecommons/hive", Number: 33}}}},
			want:     bucketFailing,
			guarding: "hivecommons/hive#7438: sec-check's level-held #188 sat red and held, invisible to its author, until a human repaired it",
		},
		{
			name:     "held PR red only on optional checks is still never eligible",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 34, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright"}},
			in:       mergeEligibleInputs{requiredChecks: required, hold: github.HoldResult{Items: []github.HoldItem{{Repo: "hivecommons/hive", Number: 34}}}},
			want:     bucketNeither,
			guarding: "the moved hold check must still sit in front of the merge-eligible path",
		},
		{
			name:     "red with no required-check set fails closed into ci_failing",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 4, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright"}},
			want:     bucketFailing,
			guarding: "no required set → cannot tell optional from required → old fail-closed behavior",
		},
		{
			name:     "red only on optional checks and mergeable is eligible",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 5, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright", "coverage"}},
			in:       mergeEligibleInputs{requiredChecks: required},
			want:     bucketEligible,
			guarding: "2026-08-28: 16 dependabot PRs parked in ci-failing.json for 11 days behind perma-red optional shards",
		},
		{
			name: "red on a required check is ci_failing even with the set configured",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 6, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"playwright", "test"}},
			in:   mergeEligibleInputs{requiredChecks: required},
			want: bucketFailing,
		},
		{
			name:     "red only on optional checks but mergeability unknown stays ci_failing",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 7, CIStatus: "failure", Mergeable: github.MergeableUnknown, FailingChecks: []string{"playwright"}},
			in:       mergeEligibleInputs{requiredChecks: required},
			want:     bucketFailing,
			guarding: "the optional-red carve-out needs GitHub's own mergeable verdict, not just the check names",
		},
		{
			name:     "pending but mergeable is eligible",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 8, CIStatus: "pending", Mergeable: github.MergeableYes},
			want:     bucketEligible,
			guarding: "2026-08-04: three green console PRs frozen for hours waiting on tide/coverage that never complete",
		},
		{
			name: "pending with unknown mergeability is neither",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 9, CIStatus: "pending", Mergeable: github.MergeableUnknown},
			want: bucketNeither,
		},
		{
			name: "pending and conflicting is neither",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 10, CIStatus: "pending", Mergeable: github.MergeableNo},
			want: bucketNeither,
		},
		{
			name:     "green but conflicting is neither",
			pr:       github.PullRequest{Repo: "hivecommons/hive", Number: 11, CIStatus: "success", Mergeable: github.MergeableNo},
			want:     bucketNeither,
			guarding: "2026-08-31: DIRTY go.mod bumps pinned the eligible count at N while nothing could merge",
		},
		{
			name: "green with unknown mergeability is eligible (list endpoint never populates it)",
			pr:   github.PullRequest{Repo: "hivecommons/hive", Number: 12, CIStatus: "success", Mergeable: github.MergeableUnknown},
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

// hivecommons/hive#7438 acceptance: a non-draft red PR carrying a hold lands
// in ci_failing with held:true and never in eligible, so its author's
// fix-before-new block can list it with the do-not-remove-the-hold note.
func TestWriteMergeEligible_HeldRedPRIsFailingWithHeldFlag(t *testing.T) {
	held := github.PullRequest{Repo: "hive", Number: 188, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"Python Unit Tests"}}
	free := github.PullRequest{Repo: "hive", Number: 190, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"lint"}}
	in := mergeEligibleInputs{org: "hivecommons", hold: github.HoldResult{Items: []github.HoldItem{{Repo: "hive", Number: 188}}}}
	eligible, failing := runWriteMergeEligible(t, []github.PullRequest{held, free}, in)
	if len(eligible) != 0 {
		t.Fatalf("a held PR became merge-eligible: %+v", eligible)
	}
	if len(failing) != 2 {
		t.Fatalf("ci_failing = %+v, want both red PRs (held #188 and unheld #190)", failing)
	}
	byNumber := map[int]failingEntry{}
	for _, f := range failing {
		byNumber[f.Number] = f
	}
	if !byNumber[188].Held {
		t.Errorf("#188 is in ci_failing without held:true: %+v", byNumber[188])
	}
	if byNumber[190].Held {
		t.Errorf("#190 carries held:true but has no hold: %+v", byNumber[190])
	}
	if byNumber[188].FailingChecks == nil || byNumber[188].FailingChecks[0] != "Python Unit Tests" {
		t.Errorf("held entry lost its CI evidence: %+v", byNumber[188])
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
	escalated := map[string]bool{escalation.Key("hivecommons/hive", 42): true}
	eligible, failing := runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons", escalated: escalated})
	if len(eligible) != 0 || len(failing) != 1 {
		t.Fatalf("eligible=%+v failing=%+v, want exactly one failing entry", eligible, failing)
	}
	got := failing[0]
	if got.Repo != "hivecommons/hive" {
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
	_, failing = runWriteMergeEligible(t, []github.PullRequest{pr}, mergeEligibleInputs{org: "hivecommons"})
	if len(failing) != 1 || failing[0].Escalated {
		t.Fatalf("un-escalated PR reported escalated: %+v", failing)
	}
}

// A red PR the enrichment pass found deferred to a still-open shared-CI
// incident reaches ci-failing.json with deferred_incident set — including a
// red PR that is also conflicted — so the kick builders can keep it out of
// their repair lists (hivecommons/hive#10528). A red PR without one does not.
func TestWriteMergeEligible_FailingEntryCarriesDeferredIncident(t *testing.T) {
	prs := []github.PullRequest{
		{Repo: "hive", Number: 10511, CIStatus: "failure", HeadSHA: "a", FailingChecks: []string{"build"}, SharedCIIncident: 10512},
		{Repo: "hive", Number: 10514, CIStatus: "failure", HeadSHA: "b", FailingChecks: []string{"build"}, SharedCIIncident: 10512,
			Mergeable: github.MergeableNo, MergeableState: "dirty", AppAuthored: true},
		{Repo: "hive", Number: 10475, CIStatus: "failure", HeadSHA: "c", FailingChecks: []string{"build"}},
	}
	_, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "hivecommons"})
	want := map[int]int{10511: 10512, 10514: 10512, 10475: 0}
	if len(failing) != len(want) {
		t.Fatalf("failing=%+v, want %d rows", failing, len(want))
	}
	for _, f := range failing {
		if f.DeferredIncident != want[f.Number] {
			t.Errorf("#%d deferred_incident = %d, want %d", f.Number, f.DeferredIncident, want[f.Number])
		}
	}
}

// A hive-created PR with mergeable_state=dirty needs a base merge/rebase even
// when CI is green, so it rides the same fix-lane artifact as red CI with a
// conflict marker and an owning lane. If that lane is unavailable in config,
// the row is assigned to scanner, the fallback fixer.
func TestWriteMergeEligible_ConflictedHivePRRoutesToOwnerOrFallback(t *testing.T) {
	dirty := github.PullRequest{
		Repo: "hive", Number: 9510, Title: "dirty hive PR", CIStatus: "success",
		Mergeable: github.MergeableNo, MergeableState: "dirty", AppAuthored: true,
		Labels: []string{"agent/strategist"}, HeadRef: "strategist/v6-readiness",
	}
	eligible, failing := runWriteMergeEligible(t, []github.PullRequest{dirty}, mergeEligibleInputs{org: "hivecommons"})
	if len(eligible) != 0 || len(failing) != 1 {
		t.Fatalf("eligible=%+v failing=%+v, want one conflict row", eligible, failing)
	}
	if f := failing[0]; !f.Conflict || f.MergeableState != "dirty" || f.Agent != "strategist" || len(f.FailingChecks) != 0 {
		t.Fatalf("conflict row = %+v, want strategist-owned dirty row without CI failures", f)
	}

	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"scanner":    {Enabled: true},
			"strategist": {Enabled: true},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"idle":  {Cadences: map[string]config.Cadence{"strategist": config.NewIntervalCadence("paused")}},
			"surge": {Cadences: map[string]config.Cadence{"strategist": config.NewIntervalCadence("pause")}},
		}},
	}
	_, failing = runWriteMergeEligible(t, []github.PullRequest{dirty}, mergeEligibleInputs{org: "hivecommons", cfg: cfg})
	if len(failing) != 1 {
		t.Fatalf("failing=%+v, want one rerouted conflict row", failing)
	}
	if f := failing[0]; f.Agent != "scanner" || f.ReroutedFrom != "strategist" || !f.Conflict {
		t.Fatalf("rerouted conflict row = %+v, want scanner rerouted_from=strategist", f)
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
	eligible, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "hivecommons"})
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
	if e := byNumber[1]; e.Repo != "hivecommons/hive" || e.HeadSHA != "abc123" || e.Mergeable != string(github.MergeableYes) || e.DCO != "yes" || len(e.Labels) != 2 {
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
		{Repo: "hivecommons/hive", Number: 1, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "sha-1"},
		{Repo: "hivecommons/hive", Number: 2, CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: "sha-2-moved"},
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
		{Repo: "hivecommons/hive", Number: 1, HeadSHA: "sha-1", Verdict: review.VerdictApprove, MergeEligible: true},
		{Repo: "hivecommons/hive", Number: 2, HeadSHA: "sha-2-reviewed", Verdict: review.VerdictApprove, MergeEligible: true},
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
		nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// The failing bucket says where each red branch lives and what the agent can
// do about it (hivecommons/hive#7386): a fork PR is comment-only, a same-repo
// PR is push-repairable, and the fields are on the wire so kick builders and
// agents never have to discover it with a failed push.
func TestWriteMergeEligible_FailingEntryCarriesForkOrigin(t *testing.T) {
	prs := []github.PullRequest{
		{Repo: "testsuite", Number: 839, CIStatus: "failure", Mergeable: github.MergeableYes, HeadSHA: "f1",
			FailingChecks: []string{"build"}, HeadRef: "sec-check-dashboard", HeadRepo: "alice/testsuite", FromFork: true},
		{Repo: "testsuite", Number: 840, CIStatus: "failure", Mergeable: github.MergeableYes, HeadSHA: "f2",
			FailingChecks: []string{"build"}, HeadRef: "hive/fix-840", HeadRepo: "projectbluefin/testsuite"},
	}
	_, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "projectbluefin"})
	if len(failing) != 2 {
		t.Fatalf("failing = %+v, want both PRs", failing)
	}
	byNum := map[int]failingEntry{}
	for _, f := range failing {
		byNum[f.Number] = f
	}
	if f := byNum[839]; !f.FromFork || f.HeadRepo != "alice/testsuite" || f.HeadRef != "sec-check-dashboard" || f.ReachableAction != github.ReachableActionCommentOnly {
		t.Errorf("fork PR entry = %+v, want from_fork with head origin and reachable_action=comment-only", f)
	}
	if f := byNum[840]; f.FromFork || f.HeadRepo != "projectbluefin/testsuite" || f.ReachableAction != github.ReachableActionPush {
		t.Errorf("same-repo PR entry = %+v, want reachable_action=push", f)
	}
}

// An escalated PR that is conflicted (CI never ran, so nothing is red) or
// green must still reach the reviewer lane (hivecommons/hive#9477). It is not
// fix-lane work, so it stays out of ci_failing, and goes to the separate
// "escalated" list with GitHub's mergeability and CI state instead.
func TestWriteMergeEligible_EscalatedNonFailingPRsListedForReviewerLane(t *testing.T) {
	prs := []github.PullRequest{
		{Repo: "hive", Number: 9258, CIStatus: "success", Mergeable: github.MergeableNo, MergeableState: "dirty", HeadSHA: "3e20e38dd", Labels: []string{"needs-human"}},
		{Repo: "hive", Number: 7, CIStatus: "failure", Mergeable: github.MergeableYes, FailingChecks: []string{"build"}},
		{Repo: "hive", Number: 8, CIStatus: "success", Mergeable: github.MergeableNo},
	}
	escalated := map[string]bool{
		escalation.Key("hivecommons/hive", 9258): true,
		escalation.Key("hivecommons/hive", 7):    true,
	}
	_, failing := runWriteMergeEligible(t, prs, mergeEligibleInputs{org: "hivecommons", escalated: escalated})
	if len(failing) != 1 || failing[0].Number != 7 {
		t.Fatalf("ci_failing = %+v, want only the red PR #7", failing)
	}

	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		t.Fatalf("read ci-failing.json: %v", err)
	}
	var payload struct {
		Escalated []struct {
			Number    int      `json:"number"`
			Repo      string   `json:"repo"`
			Escalated bool     `json:"escalated"`
			Labels    []string `json:"labels"`
			Mergeable string   `json:"mergeable"`
			CIStatus  string   `json:"ci_status"`
		} `json:"escalated"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode ci-failing.json: %v", err)
	}
	if len(payload.Escalated) != 1 {
		t.Fatalf("escalated = %+v, want only the conflicted escalated PR #9258 (the red one is already in ci_failing; #8 is not escalated)", payload.Escalated)
	}
	got := payload.Escalated[0]
	if got.Number != 9258 || got.Repo != "hivecommons/hive" || !got.Escalated {
		t.Errorf("escalated row = %+v", got)
	}
	if got.Mergeable != string(github.MergeableNo) || got.CIStatus != "success" {
		t.Errorf("escalated row mergeable=%q ci_status=%q, want no/success", got.Mergeable, got.CIStatus)
	}
}

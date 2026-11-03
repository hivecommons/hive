package review

import (
	"testing"
	"time"
)

func TestSummarizeReviewerAccuracyFamilyBucketsAndFallbackModel(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	ledger := &OutcomeLedger{Items: map[string]*PROutcome{
		"acme/a#1": {Repo: "acme/a", Number: 1, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#2": {Repo: "acme/a", Number: 2, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#3": {Repo: "acme/a", Number: 3, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#4": {Repo: "acme/a", Number: 4, FirstSeenAt: now.Add(-2 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
	}}
	approve := map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}
	artifact := Artifact{Items: []Aggregate{
		// Same family, wrongly approved.
		{Repo: "acme/a", Number: 1, HeadSHA: "a", AuthorModel: "claude-fable-5.1", ReviewModel: "claude-opus-4-6", Verdict: VerdictApprove, Perspectives: approve, RecordedAt: now.Add(-time.Hour)},
		// Cross family, correctly approved.
		{Repo: "acme/a", Number: 2, HeadSHA: "b", AuthorModel: "claude-fable-5.1", ReviewModel: "gpt-6-astra", Verdict: VerdictApprove, Perspectives: approve, RecordedAt: now.Add(-time.Hour)},
		// No review_model recorded: falls back to the reviewer launch model
		// (claude), so it is same-family against a claude author.
		{Repo: "acme/a", Number: 3, HeadSHA: "c", AuthorModel: "claude-fable-5.1", Verdict: VerdictApprove, Perspectives: approve, RecordedAt: now.Add(-time.Hour)},
		// Human-authored PR (hive_authored known false, no author model).
		{Repo: "acme/a", Number: 4, HeadSHA: "d", HiveAuthoredKnown: true, HiveAuthored: false, ReviewModel: "gemini-4-argon", Verdict: VerdictApprove, Perspectives: approve, RecordedAt: now.Add(-time.Hour)},
	}}
	evidence := map[string]ReviewerAccuracyEvidence{"acme/a#1": {ReworkedAfterApproval: true}}

	sum := SummarizeReviewerAccuracyWith(now, 30*24*time.Hour, artifact, ledger, evidence, ReviewerAccuracyOptions{FallbackReviewModel: "claude-fable-5.1"})

	if g := findAccuracyGroup(t, sum.ReviewerModels, "claude-fable-5.1"); g.Samples != 1 {
		t.Fatalf("fallback model group = %+v", g)
	}
	for _, g := range sum.ReviewerModels {
		if g.Name == "unknown" {
			t.Fatalf("fallback left an unknown reviewer model group: %+v", sum.ReviewerModels)
		}
	}
	claude := findAccuracyGroup(t, sum.AuthorFamilies, "claude")
	if claude.Samples != 3 || claude.FalseApprovals != 1 {
		t.Fatalf("claude author family = %+v", claude)
	}
	if human := findAccuracyGroup(t, sum.AuthorFamilies, "human"); human.Samples != 1 {
		t.Fatalf("human author family = %+v", human)
	}
	same := findAccuracyGroup(t, sum.FamilyMatch, FamilyMatchSame)
	if same.Samples != 2 || same.FalseApprovals != 1 || same.FalseApproveRate != 0.5 {
		t.Fatalf("same-family group = %+v", same)
	}
	cross := findAccuracyGroup(t, sum.FamilyMatch, FamilyMatchCross)
	if cross.Samples != 1 || cross.FalseApprovals != 0 {
		t.Fatalf("cross-family group = %+v", cross)
	}
	if unknown := findAccuracyGroup(t, sum.FamilyMatch, FamilyMatchUnknown); unknown.Samples != 1 {
		t.Fatalf("human PR should be family-match unknown: %+v", unknown)
	}

	// Without the fallback the unstamped verdict stays unknown on both axes.
	plain := SummarizeReviewerAccuracy(now, 30*24*time.Hour, artifact, ledger, evidence)
	if g := findAccuracyGroup(t, plain.ReviewerModels, "unknown"); g.Samples != 1 {
		t.Fatalf("no fallback should leave one unknown reviewer model: %+v", plain.ReviewerModels)
	}
	if g := findAccuracyGroup(t, plain.FamilyMatch, FamilyMatchUnknown); g.Samples != 2 {
		t.Fatalf("no fallback should leave two family-match unknowns: %+v", plain.FamilyMatch)
	}
}

func TestAuthorFamilyBucket(t *testing.T) {
	cases := []struct {
		model       string
		known, hive bool
		want        string
	}{
		{"claude-fable-5.1", true, true, "claude"},
		{"GPT-6-astra", false, false, "gpt"},
		{"gemini-4-argon", true, true, "gemini"},
		{"grok-4.7", true, true, "other"},
		{"", true, false, "human"},
		{"", true, true, "unknown"},
		{"", false, false, "unknown"},
	}
	for _, c := range cases {
		if got := AuthorFamilyBucket(c.model, c.known, c.hive); got != c.want {
			t.Errorf("AuthorFamilyBucket(%q,%v,%v)=%q want %q", c.model, c.known, c.hive, got, c.want)
		}
	}
}

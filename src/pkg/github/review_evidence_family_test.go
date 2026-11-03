package github

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/review"
)

func TestEvidenceVerdictCarriesAuthorModelAndFamilyStamp(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	report := review.PerspectiveReport{Perspective: review.PerspectiveSecurity, Verdict: review.VerdictApprove, ReviewModel: "gpt-6-astra", AuthorModel: " claude-fable-5.1 "}
	v := evidenceVerdict(report, InvocationMeta{Backend: "copilot", Model: "claude-fable-5.1"}, now)
	if v.AuthorModel != "claude-fable-5.1" || v.Model != "gpt-6-astra" || v.SameFamily != nil {
		t.Fatalf("verdict = %+v", v)
	}
	stamped := stampEvidenceVerdictFamily(v, evidence.Author{Kind: evidence.AuthorAgent})
	if stamped.SameFamily == nil || *stamped.SameFamily {
		t.Fatalf("gpt reviewing claude must be cross-family: %+v", stamped)
	}

	// No author model on the verdict: the bundle author's model is used.
	plain := evidenceVerdict(review.PerspectiveReport{Perspective: review.PerspectiveSecurity, Verdict: review.VerdictApprove}, InvocationMeta{Model: "claude-opus-4-6"}, now)
	if plain.Model != "claude-opus-4-6" {
		t.Fatalf("review model should fall back to the launch model: %+v", plain)
	}
	same := stampEvidenceVerdictFamily(plain, evidence.Author{Kind: evidence.AuthorAgent, Model: "claude-fable-5.1"})
	if same.SameFamily == nil || !*same.SameFamily {
		t.Fatalf("claude reviewing claude must be same-family: %+v", same)
	}
	// Human author with no model: family unknown, no stamp.
	if human := stampEvidenceVerdictFamily(plain, evidence.Author{Kind: evidence.AuthorHuman}); human.SameFamily != nil {
		t.Fatalf("human author should leave same_family unset: %+v", human)
	}
}

func TestContainsEvidenceVerdictIgnoresDerivedFamilyFields(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	old := evidence.Verdict{Perspective: "security", Model: "claude-opus-4-6", Verdict: "approve", Confidence: 0.8, RecordedAt: now.Add(-time.Hour)}
	cross := false
	relayed := old
	relayed.RecordedAt = now
	relayed.AuthorModel = "claude-fable-5.1"
	relayed.SameFamily = &cross
	if !containsEvidenceVerdict([]evidence.Verdict{old}, relayed) {
		t.Fatal("re-relayed verdict with new family stamps must dedupe against the old row")
	}
	relayed.Verdict = "changes_requested"
	if containsEvidenceVerdict([]evidence.Verdict{old}, relayed) {
		t.Fatal("a different verdict must not dedupe")
	}
}

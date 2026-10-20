package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/review"
)

func TestBuildEvidenceSummaryAuthorFamilyBuckets(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	same := true
	cross := false

	// Claude-authored PR, reviewed by claude (stored same_family) and gpt.
	claudePR := evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "claude-opus-4-6", Verdict: "approve", Confidence: 0.9, SameFamily: &same},
		{Perspective: "quality", Model: "gpt-6-astra", Verdict: "approve", Confidence: 0.9, SameFamily: &cross},
	}, 0, true)
	claudePR.Author.Model = "claude-fable-5.1"
	// Gemini-authored PR, closed; old bundle without same_family, derived.
	geminiPR := evSummaryBundle("a/b", 2, evidence.AuthorAgent, recent, false, []evidence.Verdict{
		{Perspective: "security", Model: "gemini-4-argon", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	geminiPR.Author.Model = "gemini-4-argon"
	geminiPR.Outcome = &evidence.Outcome{Source: evidence.OutcomeSourceLedger, State: evidence.BackfillStateClosed, At: now, ClosedAt: recent.Add(time.Hour)}
	// Human PR, approved by gemini: family match unknown.
	humanPR := evSummaryBundle("a/b", 3, evidence.AuthorHuman, recent, true, []evidence.Verdict{
		{Perspective: "security", Model: "gemini-4-argon", Verdict: "approve", Confidence: 0.9},
	}, 0, true)
	// Agent PR with no model anywhere: unknown family.
	unknownPR := evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent, true, nil, 0, true)

	win := evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}
	got := buildEvidenceSummaryWindow([]*evidence.Bundle{claudePR, geminiPR, humanPR, unknownPR}, win, "", "")

	if o := got.OutcomesByAuthorFamily["claude"]; o.N != 1 || o.Merged != 1 {
		t.Errorf("claude outcomes = %+v", o)
	}
	if o := got.OutcomesByAuthorFamily["gemini"]; o.N != 1 || o.Closed != 1 {
		t.Errorf("gemini outcomes = %+v", o)
	}
	if o := got.OutcomesByAuthorFamily["human"]; o.N != 1 || o.Merged != 1 {
		t.Errorf("human outcomes = %+v", o)
	}
	if o := got.OutcomesByAuthorFamily["unknown"]; o.N != 1 {
		t.Errorf("unknown outcomes = %+v", o)
	}
	// Author-kind buckets are untouched.
	if o := got.Outcomes["agent"]; o.N != 3 {
		t.Errorf("agent outcomes = %+v", o)
	}

	models := map[string]EvidenceModelAccuracy{}
	for _, m := range got.ReviewerModels {
		models[m.Model] = m
	}
	if m := models["claude-opus-4-6"]; m.ByAuthorFamily["claude"].Approvals != 1 || m.ByFamilyMatch[review.FamilyMatchSame].Approvals != 1 {
		t.Errorf("claude reviewer = %+v", m)
	}
	if m := models["gpt-6-astra"]; m.ByFamilyMatch[review.FamilyMatchCross].Approvals != 1 {
		t.Errorf("gpt reviewer = %+v", m)
	}
	gem := models["gemini-4-argon"]
	if gem.ByAuthorFamily["gemini"].ApprovedNotMerged != 1 || gem.ByFamilyMatch[review.FamilyMatchSame].ApprovedNotMerged != 1 {
		t.Errorf("derived same-family for gemini/gemini = %+v", gem)
	}
	if gem.ByAuthorFamily["human"].Approvals != 1 || gem.ByFamilyMatch[review.FamilyMatchUnknown].Approvals != 1 {
		t.Errorf("human-authored verdict buckets = %+v", gem)
	}
}

func TestEvidenceAuthorFamilyAndVerdictFamilyMatch(t *testing.T) {
	if got := evidenceAuthorFamily(evidence.Author{Kind: evidence.AuthorHuman}); got != "human" {
		t.Errorf("human author family = %q", got)
	}
	if got := evidenceAuthorFamily(evidence.Author{Kind: evidence.AuthorBot, Model: "gpt-6-astra"}); got != "gpt" {
		t.Errorf("bot with model = %q", got)
	}
	if got := evidenceAuthorFamily(evidence.Author{}); got != "unknown" {
		t.Errorf("empty author = %q", got)
	}
	if got := evidenceVerdictFamilyMatch(evidence.Verdict{Model: "gpt-6-astra", AuthorModel: "claude-fable-5.1"}, evidence.Author{Model: "gpt-6-astra"}); got != review.FamilyMatchCross {
		t.Errorf("verdict author_model should win over bundle author: %q", got)
	}
	if got := evidenceVerdictFamilyMatch(evidence.Verdict{Model: "gpt-6-astra"}, evidence.Author{}); got != review.FamilyMatchUnknown {
		t.Errorf("no author model = %q", got)
	}
}

func TestEvidenceAccuracyCardFamilyBreakdown(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	resp := EvidenceSummary{
		ReviewerAccuracy: &review.ReviewerAccuracySummary{
			Samples:        40,
			ReviewerModels: []review.ReviewerAccuracyGroup{{Name: "m1", Approvals: 30, FalseApprovals: 1, Blocks: 10}},
			FamilyMatch: []review.ReviewerAccuracyGroup{
				{Name: review.FamilyMatchSame, Samples: 25, FalseApproveRate: 0.1},
				{Name: review.FamilyMatchCross, Samples: 15, FalseApproveRate: 0.02},
			},
			AuthorFamilies: []review.ReviewerAccuracyGroup{{Name: "claude", Samples: 30}, {Name: "human", Samples: 10}},
		},
	}
	cards := evidenceCards(&resp, evidenceWindow{Hours: 720, From: now.Add(-720 * time.Hour), To: now}, now)
	acc, ok := cards[evidenceCardAccuracy]
	if !ok {
		t.Fatal("no accuracy card")
	}
	if !strings.Contains(acc.PlainSummary, "(same-family 10%, cross-family 2%)") {
		t.Errorf("plain summary = %q", acc.PlainSummary)
	}
	if acc.Grade.Letter != "A" {
		t.Errorf("family breakdown must not change the grade: %+v", acc.Grade)
	}
}

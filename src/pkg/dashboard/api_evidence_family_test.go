package dashboard

import (
	"encoding/json"
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

// TestBuildEvidenceSummaryAuthorModelBuckets pins outcomes_by_author_model
// (hivecommons/hive#18720): change failures per canonical author model,
// with raw spellings folded and "human" / "unknown" bucketed as the family
// map does, so the per-model and per-family maps sum to the same totals.
func TestBuildEvidenceSummaryAuthorModelBuckets(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	mergedAt := now.Add(-time.Hour)
	agentPR := func(n int, model string, merged bool) *evidence.Bundle {
		b := evSummaryBundle("a/b", n, evidence.AuthorAgent, recent, merged, nil, 0, true)
		b.Author.Model = model
		if merged {
			b.Merge.At = mergedAt
		}
		return b
	}
	// Two models of the same family, each spelled more than one way.
	opusReverted := agentPR(1, "claude-opus-5-5", true)
	opusReverted.RevertedBy = []string{"a/b#90@merged"}
	opusClean := agentPR(2, "anthropic/claude-opus-5-5", true)
	opusOpen := agentPR(3, "claude-opus-5.5", false)
	sonnetReopened := agentPR(4, "claude-sonnet-5.5", true)
	sonnetReopened.IssueReopens = []evidence.IssueReopen{{Issue: 7, At: mergedAt.Add(time.Hour), Source: evidence.IssueReopenSourceClaimLedger}}
	gpt := agentPR(5, "openai-codex/gpt-6.1-terra:high", true)
	human := evSummaryBundle("a/b", 6, evidence.AuthorHuman, recent, true, nil, 0, true)
	human.Merge.At = mergedAt
	human.RevertedBy = []string{"a/b#91@merged"}
	noModel := agentPR(7, "", true)

	win := evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}
	got := buildEvidenceSummaryWindow([]*evidence.Bundle{opusReverted, opusClean, opusOpen, sonnetReopened, gpt, human, noModel}, win, "", "")

	type want struct{ n, merged, reverted, reopened int }
	cases := map[string]want{
		"claude-opus-5.5":   {n: 3, merged: 2, reverted: 1},
		"claude-sonnet-5.5": {n: 1, merged: 1, reopened: 1},
		"gpt-6.1-terra":     {n: 1, merged: 1},
		"human":             {n: 1, merged: 1, reverted: 1},
		"unknown":           {n: 1, merged: 1},
	}
	if len(got.OutcomesByAuthorModel) != len(cases) {
		t.Errorf("model keys = %v, want %d keys", got.OutcomesByAuthorModel, len(cases))
	}
	for key, w := range cases {
		o, ok := got.OutcomesByAuthorModel[key]
		if !ok {
			t.Errorf("missing model key %q in %v", key, got.OutcomesByAuthorModel)
			continue
		}
		if o.RevertedWithin14d == nil {
			t.Errorf("%s: reverted_14d is nil after finishing", key)
			continue
		}
		if o.N != w.n || o.Merged != w.merged || *o.RevertedWithin14d != w.reverted || o.IssueReopenedWithin14d != w.reopened || o.ChangeFailures != w.reverted+w.reopened {
			t.Errorf("%s = %+v (reverted %d), want %+v", key, o, *o.RevertedWithin14d, w)
		}
	}
	if r := got.OutcomesByAuthorModel["claude-opus-5.5"].ChangeFailureRate; r == nil || *r != 0.5 {
		t.Errorf("claude-opus-5.5 change_failure_rate = %v, want 0.5", r)
	}
	if r := got.OutcomesByAuthorModel["gpt-6.1-terra"].ChangeFailureRate; r == nil || *r != 0 {
		t.Errorf("gpt-6.1-terra change_failure_rate = %v, want 0", r)
	}

	// Summing over model keys equals summing over family keys, field by field.
	sum := func(m map[string]EvidenceOutcomes) EvidenceOutcomes {
		var tot EvidenceOutcomes
		reverted := 0
		for _, o := range m {
			tot.N += o.N
			tot.Merged += o.Merged
			tot.MergedElsewhere += o.MergedElsewhere
			tot.Closed += o.Closed
			tot.Open += o.Open
			tot.Unknown += o.Unknown
			tot.IssueReopenedWithin14d += o.IssueReopenedWithin14d
			tot.ChangeFailures += o.ChangeFailures
			if o.RevertedWithin14d != nil {
				reverted += *o.RevertedWithin14d
			}
		}
		tot.RevertedWithin14d = &reverted
		return tot
	}
	byModel, byFamily := sum(got.OutcomesByAuthorModel), sum(got.OutcomesByAuthorFamily)
	if *byModel.RevertedWithin14d != *byFamily.RevertedWithin14d {
		t.Errorf("reverted_14d: models %d, families %d", *byModel.RevertedWithin14d, *byFamily.RevertedWithin14d)
	}
	byModel.RevertedWithin14d, byFamily.RevertedWithin14d = nil, nil
	if byModel != byFamily {
		t.Errorf("model sums %+v != family sums %+v", byModel, byFamily)
	}
	if byModel.N != got.N {
		t.Errorf("model sums n = %d, want %d", byModel.N, got.N)
	}

	// Existing maps are unchanged by the new grouping. modelfamily.Family
	// derives the family from the provider-stripped id (#18803), so
	// "anthropic/claude-opus-5-5" joins the other Claude PRs and
	// "openai-codex/gpt-6.1-terra:high" is "gpt"; nothing lands in "other".
	if o := got.OutcomesByAuthorFamily["claude"]; o.N != 4 || o.Merged != 3 || o.Unknown != 1 || o.ChangeFailures != 2 {
		t.Errorf("claude family = %+v", o)
	}
	if o := got.OutcomesByAuthorFamily["gpt"]; o.N != 1 || o.Merged != 1 || o.ChangeFailures != 0 {
		t.Errorf("gpt family = %+v", o)
	}
	if o, ok := got.OutcomesByAuthorFamily["other"]; ok && o.N != 0 {
		t.Errorf("other family = %+v, want empty", o)
	}
	if o := got.Outcomes["agent"]; o.N != 6 || o.Merged != 5 || o.ChangeFailures != 2 {
		t.Errorf("agent kind = %+v", o)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"outcomes_by_author_model":{`) {
		t.Errorf("JSON lacks outcomes_by_author_model: %s", raw)
	}
	// An empty window still serialises an empty object, never null.
	empty := buildEvidenceSummaryWindow(nil, win, "", "")
	if empty.OutcomesByAuthorModel == nil || len(empty.OutcomesByAuthorModel) != 0 {
		t.Errorf("empty window OutcomesByAuthorModel = %v", empty.OutcomesByAuthorModel)
	}
}

func TestEvidenceAuthorModel(t *testing.T) {
	for _, tc := range []struct {
		author evidence.Author
		want   string
	}{
		{evidence.Author{Kind: evidence.AuthorAgent, Model: "claude-opus-5-5"}, "claude-opus-5.5"},
		{evidence.Author{Kind: evidence.AuthorAgent, Model: "anthropic/claude-opus-5-5"}, "claude-opus-5.5"},
		{evidence.Author{Kind: evidence.AuthorAgent, Model: " Claude-Opus-5.5 "}, "claude-opus-5.5"},
		{evidence.Author{Kind: evidence.AuthorAgent, Model: "gpt-6-astra"}, "gpt-6-astra"},
		{evidence.Author{Kind: evidence.AuthorAgent}, "unknown"},
		{evidence.Author{Kind: evidence.AuthorBot}, "unknown"},
		{evidence.Author{Kind: evidence.AuthorHuman}, "human"},
		{evidence.Author{}, "unknown"},
	} {
		if got := evidenceAuthorModel(tc.author); got != tc.want {
			t.Errorf("evidenceAuthorModel(%+v) = %q, want %q", tc.author, got, tc.want)
		}
		// Every author lands in one model key and one family key; a
		// model-less author shares the family's human/unknown key.
		if strings.TrimSpace(tc.author.Model) == "" && evidenceAuthorFamily(tc.author) != tc.want {
			t.Errorf("model-less %+v: model key %q, family key %q", tc.author, tc.want, evidenceAuthorFamily(tc.author))
		}
	}
}

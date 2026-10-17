package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func baseReport(p Perspective, v Verdict, findings ...outputschema.Finding) PerspectiveReport {
	if findings == nil {
		findings = []outputschema.Finding{}
	}
	return PerspectiveReport{
		AgentReport: outputschema.AgentReport{
			Lane:       "review-swarm",
			Kind:       outputschema.KindReview,
			Findings:   findings,
			PRsOpened:  []outputschema.PROpened{},
			BeadsFiled: []outputschema.BeadFiled{},
			Summary:    string(p) + " summary",
		},
		Perspective: p,
		Verdict:     v,
		Repo:        "hivecommons/hive",
		Number:      2807,
		HeadSHA:     testSHA,
	}
}

func allApprove() []PerspectiveReport {
	out := make([]PerspectiveReport, 0, len(DefaultPerspectives))
	for _, p := range DefaultPerspectives {
		out = append(out, baseReport(p, VerdictApprove))
	}
	return out
}

func TestAggregateReportsMatrix(t *testing.T) {
	highFinding := outputschema.Finding{Title: "risky", Severity: outputschema.SeverityHigh, Summary: "needs a human"}
	tests := []struct {
		name  string
		reps  []PerspectiveReport
		opts  AggregateOptions
		want  Verdict
		merge bool
		fix   bool
		human bool
		close bool
	}{
		{name: "unanimous approve is merge eligible", reps: allApprove(), want: VerdictApprove, merge: true},
		{name: "changes requested enters fix cycle", reps: append(allApprove()[:1], baseReport(PerspectiveSecurity, VerdictChangesRequested)), want: VerdictChangesRequested, fix: true},
		{name: "severity threshold requires human", reps: []PerspectiveReport{baseReport(PerspectiveSecurity, VerdictApprove, highFinding)}, want: VerdictRequiresHuman, human: true},
		{name: "explicit requires human wins", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictApprove), baseReport(PerspectiveSecurity, VerdictRequiresHuman)}, want: VerdictRequiresHuman, human: true},
		{name: "decision request marker wins", reps: []PerspectiveReport{func() PerspectiveReport {
			r := baseReport(PerspectiveCorrectness, VerdictApprove)
			r.DecisionRequested = true
			r.DecisionQuestion = "@clubanderson should decide whether this groundwork lands before its consumer."
			return r
		}()}, want: VerdictRequiresHuman, human: true},
		{name: "reject recommends close", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictReject)}, want: VerdictReject, close: true},
		{name: "fix cap requires human", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictChangesRequested)}, opts: AggregateOptions{FixAttempts: DefaultFixCycleCap}, want: VerdictRequiresHuman, human: true},
		{name: "missing perspectives is incomplete, not a human hold", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictApprove)}, want: VerdictIncomplete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AggregateReports(tt.reps, tt.opts)
			if got.Verdict != tt.want || got.MergeEligible != tt.merge || got.FixCycle != tt.fix || got.RequiresHuman != tt.human || got.CloseRecommended != tt.close {
				t.Fatalf("aggregate = verdict:%s merge:%v fix:%v human:%v close:%v", got.Verdict, got.MergeEligible, got.FixCycle, got.RequiresHuman, got.CloseRecommended)
			}
		})
	}
}

func TestAggregateReportsCarriesDecisionQuestion(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictApprove)
	r.DecisionRequested = true
	r.DecisionQuestion = "@clubanderson should decide whether this groundwork lands before its consumer."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want requires_human decision request", got)
	}
	if got.DecisionQuestion != r.DecisionQuestion {
		t.Fatalf("DecisionQuestion = %q, want %q", got.DecisionQuestion, r.DecisionQuestion)
	}
	if len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], r.DecisionQuestion) {
		t.Fatalf("Reasons = %v, want question quoted", got.Reasons)
	}
}

func TestAggregateReportsCarriesRequiresHumanSummary(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.Summary = "Trust-surface file by location; a relay owner should confirm before merge."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want requires_human", got)
	}
	if len(got.HumanSummaries) != 1 || !strings.Contains(got.HumanSummaries[0], "relay owner should confirm") {
		t.Fatalf("HumanSummaries = %v, want reviewer summary", got.HumanSummaries)
	}
	if len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "security requires human") {
		t.Fatalf("Reasons = %v, want requires_human summary", got.Reasons)
	}
}

func TestOutOfScopeFindingsDoNotDriveVerdictOrConfidence(t *testing.T) {
	outOfScope := outputschema.Finding{
		Title:       "pre-existing config drift",
		Severity:    outputschema.SeverityHigh,
		Summary:     "real but unrelated to this issue",
		File:        "pkg/config/config.go",
		Line:        12,
		ReviewScope: "out-of-scope",
	}
	reports := allApprove()
	reports[0] = baseReport(PerspectiveCorrectness, VerdictChangesRequested, outOfScope)

	got := AggregateReports(reports, AggregateOptions{})
	if got.Verdict != VerdictApprove || !got.MergeEligible {
		t.Fatalf("aggregate with only out-of-scope findings = %s merge=%v, want approve/merge", got.Verdict, got.MergeEligible)
	}
	if got.Confidence.Score != ConfidenceMax {
		t.Fatalf("confidence = %+v, want clean max score", got.Confidence)
	}
	if len(got.Findings) != 1 || got.Findings[0].Finding.ReviewScope != "out-of-scope" {
		t.Fatalf("aggregate should retain classified finding for reporting: %+v", got.Findings)
	}
}

func TestPromptBuilders(t *testing.T) {
	pr := PullRequest{Repo: "hivecommons/hive", Number: 2807, Title: "review swarm", Author: "bot", HeadSHA: testSHA, URL: "https://example.invalid/pr/2807", ScopeContract: "linked issue hivecommons/hive#8662 from the PR closing keyword"}
	prompt := BuildPerspectivePrompt(PerspectiveSecurity, pr)
	for _, want := range []string{"[review-perspective:security]", "hivecommons/hive#2807", "kind to \"review\"", "Allowed verdicts", testSHA, "SCOPE CONTRACT", "hivecommons/hive#8662", "review_scope"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestReviewPromptsCarryWritingGuide(t *testing.T) {
	const guide = "WRITING GUIDE (set by this hive's owner in project.writing_guide).\n\nKeep review comments concise.\n"
	pr := PullRequest{Repo: "hivecommons/hive", Number: 2807, HeadSHA: testSHA}
	opts := PromptOptions{WritingGuideSection: guide, PostComments: true}

	for name, prompt := range map[string]string{
		"single":   BuildPerspectivePromptWith(PerspectiveSecurity, pr, opts),
		"combined": BuildCombinedPrompt(pr, []Perspective{PerspectiveSecurity, PerspectiveStyle}, opts),
	} {
		if !strings.Contains(prompt, guide) {
			t.Fatalf("%s prompt does not carry writing guide:\n%s", name, prompt)
		}
		if strings.Index(prompt, guide) > strings.Index(prompt, "Return exactly one") {
			t.Fatalf("%s prompt places writing guide after the answer schema:\n%s", name, prompt)
		}
	}
}

func TestValidateRoundTripAndCollect(t *testing.T) {
	dir := t.TempDir()
	reports := allApprove()
	for _, r := range reports {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ValidateReport(raw)
		if err != nil {
			t.Fatalf("ValidateReport(%s): %v", r.Perspective, err)
		}
		if parsed.Perspective != r.Perspective || parsed.Verdict != VerdictApprove {
			t.Fatalf("round-trip mismatch: %+v", parsed)
		}
		path := filepath.Join(dir, ReviewReportFilePrefix+string(r.Perspective)+ReviewReportFileSuffix)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifact, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Items) != 1 || !artifact.HasAggregateApproval("hivecommons/hive", 2807, testSHA) {
		t.Fatalf("unexpected artifact: %+v", artifact)
	}
	out := filepath.Join(dir, ReviewVerdictsFile)
	if err := WriteArtifact(out, artifact); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadArtifact(out)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.HasAggregateApproval("hivecommons/hive", 2807, testSHA) {
		t.Fatalf("loaded artifact missing approval: %+v", loaded)
	}
	if loaded.HasAggregateApproval("other/hive", 2807, testSHA) {
		t.Fatal("approval must not cross repository owners")
	}
}

func TestCollectKeepsBodyDigestsSeparate(t *testing.T) {
	dir := t.TempDir()
	oldBody := PullRequestBodyDigest("fix links", "Closes #12397")
	newBody := PullRequestBodyDigest("fix links", "Refs #12397")
	reports := []PerspectiveReport{
		func() PerspectiveReport {
			r := baseReport(PerspectiveIntentAlignment, VerdictChangesRequested)
			r.BodyDigest = oldBody
			r.Summary = "closing keyword contradicts refs"
			return r
		}(),
		func() PerspectiveReport {
			r := baseReport(PerspectiveSecurity, VerdictApprove)
			r.BodyDigest = newBody
			return r
		}(),
	}
	for _, r := range reports {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, ReviewReportFilePrefix+string(r.Perspective)+ReviewReportFileSuffix)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	artifact, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Items) != 2 {
		t.Fatalf("items = %+v, want separate aggregates per body digest", artifact.Items)
	}
	if agg, ok := artifact.AggregateForBody("hivecommons/hive", 2807, testSHA, newBody); !ok || agg.BodyDigest != newBody {
		t.Fatalf("new-body aggregate = %+v, %v", agg, ok)
	}
}

// Perspective reports that omit body_digest must join the digest-keyed
// aggregate for the same head; apart, a full approval reads as two
// "incomplete" items (hivecommons/hive#12332 @19b51e2).
func TestCollectFoldsDigestlessReportsIntoHeadGroup(t *testing.T) {
	dir := t.TempDir()
	body := PullRequestBodyDigest("fix links", "Refs #12397")
	for _, p := range []Perspective{PerspectiveCorrectness, PerspectiveSecurity, PerspectiveIntentAlignment, PerspectiveStyle, PerspectiveDocsCurrency} {
		r := baseReport(p, VerdictApprove)
		if p != PerspectiveStyle && p != PerspectiveCorrectness {
			r.BodyDigest = body
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+string(p)+ReviewReportFileSuffix), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifact, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("items = %+v, want one aggregate for the head", artifact.Items)
	}
	agg := artifact.Items[0]
	if agg.BodyDigest != body || len(agg.Perspectives) != 5 || agg.Verdict != VerdictApprove || !agg.MergeEligible {
		t.Fatalf("aggregate = %+v, want 5/5 approve with digest %s", agg, body)
	}
}

func TestHasAggregateApprovalUsesNewestHeadAggregate(t *testing.T) {
	old := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	artifact := Artifact{Items: []Aggregate{
		{Repo: "hivecommons/hive", Number: 2807, HeadSHA: testSHA, BodyDigest: "old", Verdict: VerdictApprove, MergeEligible: true, RecordedAt: old},
		{Repo: "hivecommons/hive", Number: 2807, HeadSHA: testSHA, BodyDigest: "new", Verdict: VerdictChangesRequested, RecordedAt: old.Add(time.Hour)},
	}}

	if artifact.HasAggregateApproval("hivecommons/hive", 2807, testSHA) {
		t.Fatal("stale approval for an older body digest must not approve the current head")
	}
}

func TestValidateRejectsBadExtension(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictApprove)
	raw, _ := json.Marshal(r)
	raw = []byte(strings.Replace(string(raw), `"perspective":"security"`, `"perspective":"unknown"`, 1))
	if _, err := ValidateReport(raw); err == nil {
		t.Fatal("expected invalid perspective to fail")
	}

	r = baseReport(PerspectiveSecurity, VerdictApprove)
	r.Repo = "hive"
	raw, _ = json.Marshal(r)
	if _, err := ValidateReport(raw); err == nil {
		t.Fatal("expected bare repo to fail")
	}
}

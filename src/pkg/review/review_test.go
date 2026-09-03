package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{name: "reject recommends close", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictReject)}, want: VerdictReject, close: true},
		{name: "fix cap requires human", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictChangesRequested)}, opts: AggregateOptions{FixAttempts: DefaultFixCycleCap}, want: VerdictRequiresHuman, human: true},
		{name: "missing perspectives is disagreement", reps: []PerspectiveReport{baseReport(PerspectiveCorrectness, VerdictApprove)}, want: VerdictRequiresHuman, human: true},
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

func TestPromptBuilders(t *testing.T) {
	pr := PullRequest{Repo: "hivecommons/hive", Number: 2807, Title: "review swarm", Author: "bot", HeadSHA: testSHA, URL: "https://example.invalid/pr/2807"}
	prompt := BuildPerspectivePrompt(PerspectiveSecurity, pr)
	for _, want := range []string{"[review-perspective:security]", "hivecommons/hive#2807", "kind to \"review\"", "Allowed verdicts", testSHA} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	prompts := BuildPerspectivePrompts(pr, nil)
	if len(prompts) != len(DefaultPerspectives) {
		t.Fatalf("default prompts = %d, want %d", len(prompts), len(DefaultPerspectives))
	}
	seq := BuildSequentialPrompt(pr, []Perspective{PerspectiveDocsCurrency, PerspectiveCorrectness})
	if !strings.Contains(seq, "phase-1 sequential fallback") || !strings.Contains(seq, "review-perspective:docs-currency") {
		t.Fatalf("sequential prompt missing expected text: %s", seq)
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

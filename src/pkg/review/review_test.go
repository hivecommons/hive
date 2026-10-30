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

func TestAggregateReportsResolvesNonRiskHiveAuthoredDecisionQuestions(t *testing.T) {
	cases := []string{
		// Exact live questions from #12808 and #12790: Hive-authored
		// sequencing/config-shape decisions resolved by the reviewer.
		"@hanthor should decide whether the contribution-preset loader should land before any hivectl command consumes it",
		"accept `self_experiments.scheduler` as a new top-level config namespace vs nesting, and a fixed ledger path?",
		// #13020: the linked issue (#12964) specified the default; the old
		// allowlist still parked the PR because "default" was not on it.
		"should the composite action's default `mention_login` be hard-coded to `hivecommons-hive` or stay org-neutral?",
		"rename flag --foo to --bar?",
		// Wording the old allowlist never anticipated is still the hive's call.
		"keep the helper in the same file or split it out?",
	}
	for _, question := range cases {
		t.Run(question, func(t *testing.T) {
			r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
			r.HiveAuthored = true
			r.DecisionRequested = true
			r.DecisionQuestion = question

			got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

			if got.Verdict != VerdictApprove || !got.MergeEligible || got.RequiresHuman || got.DecisionRequested {
				t.Fatalf("aggregate = %+v, want resolved approve", got)
			}
			if len(got.Reasons) != 1 || !strings.Contains(got.Reasons[0], "decision resolved by policy:") || !strings.Contains(got.Reasons[0], "not a risk question") || !strings.Contains(got.Reasons[0], question) {
				t.Fatalf("Reasons = %v, want policy-resolution audit line with question", got.Reasons)
			}
		})
	}
}

func TestDecisionQuestionRiskClassDenylist(t *testing.T) {
	cases := []struct {
		question string
		class    string
	}{
		{"should the composite action's default `mention_login` be hard-coded to `hivecommons-hive` or stay org-neutral?", ""},
		{"rename flag --foo to --bar?", ""},
		{"should this workflow be granted contents: write?", "permissions-credentials"},
		{"drop the users table column?", "data-loss-irreversible"},
		{"rotate the deploy token in this PR?", "permissions-credentials"},
		{"is this change to OWNERS acceptable?", "sensitive-paths"},
		{"edit .github/workflows/ci.yml to skip the lint job?", "sensitive-paths"},
		{"should L6 auto-merge apply to this repo?", "merge-trust-policy"},
		{"should the author of this PR be notified?", ""},
		{"publish the release to ghcr now?", "external-side-effects"},
		{"is the added dependency license acceptable?", "licensing-legal"},
		{"the requirement is ambiguous; which reading applies?", "unverifiable"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			class, risk := DecisionQuestionRiskClass(tc.question)
			if risk != (tc.class != "") || class != tc.class {
				t.Fatalf("DecisionQuestionRiskClass(%q) = %q,%v; want %q", tc.question, class, risk, tc.class)
			}
		})
	}
}

func TestDecisionRiskClassesAreNamedAndNonEmpty(t *testing.T) {
	seen := map[string]bool{}
	for _, class := range DecisionRiskClasses {
		if class.Name == "" || seen[class.Name] {
			t.Fatalf("risk class %+v: name must be unique and non-empty", class)
		}
		seen[class.Name] = true
		if len(class.Terms)+len(class.Patterns) == 0 {
			t.Fatalf("risk class %q has no terms", class.Name)
		}
	}
	// The sensitive-path class is derived from the sentinel list and must
	// carry its headline entries.
	for _, want := range []string{"owners", ".github/workflows", "hive.yaml", "dockerfile"} {
		if _, risk := DecisionQuestionRiskClass("touches " + want); !risk {
			t.Fatalf("sentinel path %q not in the decision risk denylist", want)
		}
	}
}

func TestAggregateReportsEscalatesHighRiskMarkerOnBenignQuestion(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.DecisionRequested = true
	r.DecisionQuestion = "rename flag --foo to --bar?"
	r.DecisionRiskHigh = true

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want hive-decision-risk: high to escalate", got)
	}
}

func TestAggregateReportsDecisionQuestionDoesNotMaskRiskHumanHold(t *testing.T) {
	decision := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	decision.HiveAuthored = true
	decision.DecisionRequested = true
	decision.DecisionQuestion = "Should this groundwork land before its consumer?"
	risk := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	risk.HiveAuthored = true
	risk.Summary = "Could not read the diff for an authorization change."

	got := AggregateReports([]PerspectiveReport{decision, risk}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want risk human hold", got)
	}
}

func TestAggregateReportsEscalatesRiskDecisionQuestions(t *testing.T) {
	for _, question := range []string{
		"Should we accept this public-facing auth behavior change?",
		"Can this data deletion migration run without data loss?",
		"Is the added dependency license acceptable?",
		"should this workflow be granted contents: write?",
		"drop the users table column?",
	} {
		t.Run(question, func(t *testing.T) {
			r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
			r.HiveAuthored = true
			r.DecisionRequested = true
			r.DecisionQuestion = question

			got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

			if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
				t.Fatalf("aggregate = %+v, want risk escalation", got)
			}
		})
	}
}

func TestAggregateReportsRoutesSelfAuthoredConcreteRequiresHumanToFixer(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.Summary = "LaneQueuedWork is plumbed but never populated; add the producer in AgentActivityFor or reword the changelog."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want changes_requested fixer route", got)
	}
	if got.Perspectives[PerspectiveCorrectness] != VerdictChangesRequested {
		t.Fatalf("perspectives = %+v, want normalized changes_requested", got.Perspectives)
	}
}

func TestNormalizeLegacyConcreteRequiresHumanAggregate(t *testing.T) {
	legacy := Aggregate{
		Repo:           "hivecommons/hive",
		Number:         12875,
		HeadSHA:        testSHA,
		Verdict:        VerdictRequiresHuman,
		RequiresHuman:  true,
		HumanSummaries: []string{"correctness requires human: LaneQueuedWork is plumbed but never populated; add the producer in AgentActivityFor or reword the changelog."},
		Perspectives: map[Perspective]Verdict{
			PerspectiveCorrectness:     VerdictRequiresHuman,
			PerspectiveIntentAlignment: VerdictRequiresHuman,
		},
	}

	got := legacy.Normalize()

	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.RequiresHuman {
		t.Fatalf("normalized = %+v, want changes_requested fixer route", got)
	}
	if got.Perspectives[PerspectiveCorrectness] != VerdictChangesRequested || got.Perspectives[PerspectiveIntentAlignment] != VerdictChangesRequested {
		t.Fatalf("perspectives = %+v, want requires_human perspectives normalized", got.Perspectives)
	}
}

func TestNormalizeRepairsKnownHiveConcreteRequiresHumanAggregate(t *testing.T) {
	knownHive := Aggregate{
		Repo:              "hivecommons/hive",
		Number:            12875,
		HeadSHA:           testSHA,
		HiveAuthoredKnown: true,
		HiveAuthored:      true,
		Verdict:           VerdictRequiresHuman,
		RequiresHuman:     true,
		HumanSummaries:    []string{"correctness requires human: field is plumbed but never populated; add the producer."},
		Perspectives:      map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman},
	}

	got := knownHive.Normalize()

	if got.Verdict != VerdictChangesRequested || got.RequiresHuman || !got.FixCycle {
		t.Fatalf("normalized = %+v, want known hive aggregate repaired to changes_requested", got)
	}
}

func TestNormalizeLiveKnownHiveAggregateWithBlockingPerspectives(t *testing.T) {
	live := Aggregate{
		Repo:              "hivecommons/hive",
		Number:            12875,
		HeadSHA:           "bbb5676",
		HiveAuthoredKnown: true,
		HiveAuthored:      true,
		Verdict:           VerdictRequiresHuman,
		RequiresHuman:     true,
		Reasons:           []string{`correctness finding "LaneQueuedWork is never populated" is high (threshold high)`},
		Findings: []PerspectiveFinding{{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{
			Title:       "LaneQueuedWork is never populated",
			Severity:    outputschema.SeverityHigh,
			Summary:     "Add the producer in AgentActivityFor or trim the changelog claim.",
			ReviewScope: "in-scope",
		}}},
		Perspectives: map[Perspective]Verdict{
			PerspectiveCorrectness:     VerdictChangesRequested,
			PerspectiveIntentAlignment: VerdictChangesRequested,
		},
	}

	got := live.Normalize()

	if got.Verdict != VerdictChangesRequested || got.RequiresHuman || !got.FixCycle {
		t.Fatalf("normalized = %+v, want live aggregate repaired to changes_requested", got)
	}
}

func TestAggregateReportsRoutesSelfAuthoredDuplicateChangelogDecisionToFixer(t *testing.T) {
	r := baseReport(PerspectiveIntentAlignment, VerdictRequiresHuman, outputschema.Finding{
		Title:       "Duplicate changelog claim",
		Severity:    outputschema.SeverityHigh,
		Summary:     "Summary and changelog fragment duplicate work that already landed; trim the summary and fragment.",
		ReviewScope: "in-scope",
	})
	r.HiveAuthored = true
	r.DecisionRequested = true
	r.DecisionQuestion = "Should the maintainer trim the summary/fragment or accept the duplicate changelog entry?"
	r.Summary = "Trim the summary/changelog fragment that duplicates a prior fix."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictChangesRequested || got.RequiresHuman || !got.FixCycle || got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want duplicate changelog routed to fixer", got)
	}
}

func TestNormalizeKeepsKnownNonHiveRequiresHumanAggregate(t *testing.T) {
	knownHuman := Aggregate{
		Repo:              "hivecommons/hive",
		Number:            12875,
		HeadSHA:           testSHA,
		HiveAuthoredKnown: true,
		HiveAuthored:      false,
		Verdict:           VerdictRequiresHuman,
		RequiresHuman:     true,
		HumanSummaries:    []string{"correctness requires human: field is plumbed but never populated; add the producer."},
		Perspectives:      map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman},
	}

	got := knownHuman.Normalize()

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
		t.Fatalf("normalized = %+v, want known non-hive human hold unchanged", got)
	}
}

func TestNormalizeKeepsFixCycleCapRequiresHumanAggregate(t *testing.T) {
	capped := Aggregate{
		Repo:           "hivecommons/hive",
		Number:         12875,
		HeadSHA:        testSHA,
		Verdict:        VerdictRequiresHuman,
		RequiresHuman:  true,
		FixAttempts:    DefaultFixCycleCap,
		MaxFixAttempts: DefaultFixCycleCap,
		Reasons:        []string{"fix cycle cap reached (3/3)"},
		HumanSummaries: []string{"correctness requires human: field is plumbed but never populated; add the producer."},
		Perspectives:   map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman},
	}

	got := capped.Normalize()

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
		t.Fatalf("normalized = %+v, want capped human hold unchanged", got)
	}
}

func TestNormalizeKeepsRiskRequiresHumanAggregate(t *testing.T) {
	legacy := Aggregate{
		Repo:           "hivecommons/hive",
		Number:         12875,
		HeadSHA:        testSHA,
		Verdict:        VerdictRequiresHuman,
		RequiresHuman:  true,
		HumanSummaries: []string{"security requires human: Could not read the diff for a public-facing authorization change."},
		Perspectives:   map[Perspective]Verdict{PerspectiveSecurity: VerdictRequiresHuman},
	}

	got := legacy.Normalize()

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || got.FixCycle {
		t.Fatalf("normalized = %+v, want risk hold unchanged", got)
	}
}

func TestLoadArtifactNormalizesLegacyConcreteRequiresHumanAggregate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	legacy := Artifact{Items: []Aggregate{{
		Repo:           "hivecommons/hive",
		Number:         12875,
		HeadSHA:        testSHA,
		Verdict:        VerdictRequiresHuman,
		RequiresHuman:  true,
		HumanSummaries: []string{"correctness requires human: LaneQueuedWork is plumbed but never populated; add the producer in AgentActivityFor."},
		Perspectives:   map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman},
	}}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	loaded, err := LoadArtifact(path)
	if err != nil {
		t.Fatalf("LoadArtifact: %v", err)
	}
	got := loaded.Items[0]
	if got.Verdict != VerdictChangesRequested || !got.FixCycle || got.Perspectives[PerspectiveCorrectness] != VerdictChangesRequested {
		t.Fatalf("loaded = %+v, want normalized changes_requested", got)
	}
}

func TestAggregateReportsAdvisesSelfAuthoredNonRiskHumanWithoutRemediation(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.Summary = "This sequencing question is low stakes but worth noting for maintainers."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictApprove || !got.MergeEligible || got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want advisory approval", got)
	}
}

func TestAggregateReportsKeepsSelfAuthoredRiskRequiresHuman(t *testing.T) {
	r := baseReport(PerspectiveSecurity, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.Summary = "Could not read the diff, so a human must check this public-facing authorization change."

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman {
		t.Fatalf("aggregate = %+v, want risk escalation", got)
	}
}

func TestAggregateReportsEscalatePolicyKeepsSelfAuthoredDecisionHold(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.DecisionRequested = true
	r.DecisionQuestion = "Should this groundwork land before its consumer?"

	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{SelfAuthoredDecisionPolicy: "escalate"})

	if got.Verdict != VerdictRequiresHuman || !got.RequiresHuman || !got.DecisionRequested {
		t.Fatalf("aggregate = %+v, want configured escalation", got)
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

// hivecommons/hive#15499: a hive-authored PR whose reviewer found its
// change already on the base (hive-redundant-of) is not a question the
// hive answers by approving; the aggregate keeps the decision and names
// the superseding PR for the closer.
func TestAggregateReportsKeepsRedundantOfDecisionOnHiveAuthoredPR(t *testing.T) {
	r := baseReport(PerspectiveCorrectness, VerdictRequiresHuman)
	r.HiveAuthored = true
	r.DecisionRequested = true
	r.DecisionQuestion = "close this as a no-op?"
	r.RedundantOf = 15427
	got := AggregateReports([]PerspectiveReport{r}, AggregateOptions{})
	if got.Verdict != VerdictRequiresHuman || !got.DecisionRequested || got.RedundantOf != 15427 || got.MergeEligible {
		t.Fatalf("aggregate = %+v, want requires_human with redundant_of 15427", got)
	}
}

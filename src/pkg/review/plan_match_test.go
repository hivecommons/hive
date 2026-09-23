package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

const planMatchWave = "Plan: widget subsystem (epic-1, approved)\n- [T1] Design the data model (open)\n- [T3] Implement persistence (open)"

func planMatchPR(runKey, planRef, wave string) PullRequest {
	return PullRequest{
		Repo:    "acme/hive",
		Number:  8317,
		Title:   "runs: implement persistence",
		Author:  "hive-app[bot]",
		HeadSHA: "sha1",
		RunKey:  runKey,
		PlanRef: planRef,
		// PlanWave is planner output, never the PR author's rationale.
		PlanWave: wave,
	}
}

func planMatchReport(v Verdict, findings ...outputschema.Finding) PerspectiveReport {
	r := baseReport(PerspectivePlanMatch, v, findings...)
	r.Repo, r.Number, r.HeadSHA = "acme/hive", 8317, "sha1"
	return r
}

// plan_match is built in, so it validates without focus text and carries a
// default focus line -- but it is NOT in the default set, because it only
// earns its cost on a hive with run trailers and plans to compare against.
func TestPlanMatchIsBuiltinButOffByDefault(t *testing.T) {
	if (PerspectiveSet{}).Known(PerspectivePlanMatch) {
		t.Fatal("plan_match must not run by default")
	}
	if DefaultFocus(PerspectivePlanMatch) == "" {
		t.Fatal("plan_match has no default focus line")
	}
	set, err := NewPerspectiveSet([]string{"correctness", "plan_match"}, nil)
	if err != nil {
		t.Fatalf("explicit plan_match selection rejected: %v", err)
	}
	if !set.Known(PerspectivePlanMatch) {
		t.Fatal("explicitly selected plan_match not known")
	}
	if !isBuiltinPerspective(PerspectivePlanMatch) {
		t.Fatal("plan_match must be a built-in name")
	}
}

// WithPlanMatch is how the review.plan_match.enabled toggle reaches the set:
// appended once, after the configured perspectives, never past the cap.
func TestWithPlanMatchAppendsOnce(t *testing.T) {
	set := (PerspectiveSet{}).WithPlanMatch()
	list := set.List()
	if list[len(list)-1] != PerspectivePlanMatch || set.Len() != len(DefaultPerspectives)+1 {
		t.Fatalf("WithPlanMatch on defaults = %v", list)
	}
	if again := set.WithPlanMatch(); again.Len() != set.Len() {
		t.Fatalf("WithPlanMatch appended twice: %v", again.List())
	}
	custom, err := NewPerspectiveSet([]string{"security"}, map[string]string{"security": "only secrets"})
	if err != nil {
		t.Fatal(err)
	}
	custom = custom.WithPlanMatch()
	if custom.Focus(PerspectiveSecurity) != "only secrets" {
		t.Fatal("WithPlanMatch dropped the focus overrides")
	}
	if got := custom.List(); len(got) != 2 || got[0] != PerspectiveSecurity || got[1] != PerspectivePlanMatch {
		t.Fatalf("WithPlanMatch on a selection = %v", got)
	}

	names := make([]string, 0, MaxPerspectives)
	focus := map[string]string{}
	for i := 0; i < MaxPerspectives; i++ {
		n := "p" + strings.Repeat("x", i+1)
		names = append(names, n)
		focus[n] = "focus"
	}
	full, err := NewPerspectiveSet(names, focus)
	if err != nil {
		t.Fatal(err)
	}
	if capped := full.WithPlanMatch(); capped.Known(PerspectivePlanMatch) {
		t.Fatal("WithPlanMatch exceeded MaxPerspectives")
	}
}

// The kick carries the plan wave and the two findings this perspective
// reports, in both the single and the combined shape. The wave is planner
// output, so the prompt may quote it; the trailers are identifiers.
func TestPlanMatchPromptCarriesPlanWave(t *testing.T) {
	pr := planMatchPR("acme/hive#42", "epic-1", planMatchWave)
	single := BuildPerspectivePromptWith(PerspectivePlanMatch, pr, PromptOptions{Perspectives: (PerspectiveSet{}).WithPlanMatch()})
	combined := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness, PerspectivePlanMatch}, PromptOptions{Perspectives: (PerspectiveSet{}).WithPlanMatch()})
	for name, got := range map[string]string{"single": single, "combined": combined} {
		for _, want := range []string{"PLAN MATCH", "Hive-Run: acme/hive#42", "Hive-Plan: epic-1", "[T3] Implement persistence", "scope exceeds plan", "planned item missing"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s prompt lacks %q:\n%s", name, want, got)
			}
		}
	}
	// Other perspectives never see the plan section: it is plan_match's job.
	if got := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{}); strings.Contains(got, "PLAN MATCH") {
		t.Fatal("plan section leaked into the correctness kick")
	}
	if got := BuildCombinedPrompt(pr, []Perspective{PerspectiveCorrectness}, PromptOptions{}); strings.Contains(got, "PLAN MATCH") {
		t.Fatal("plan section leaked into a combined kick without plan_match")
	}
}

// A plan longer than MaxPlanWaveLen is cut and the cut is announced, so a
// reviewer does not report the unseen tail as missing work.
func TestPlanMatchPromptTruncatesLongWave(t *testing.T) {
	long := strings.Repeat("- [T9] a very long planned item\n", MaxPlanWaveLen/10)
	got := planMatchSection(planMatchPR("run-1", "", long))
	if !strings.Contains(got, PlanWaveTruncationNote) {
		t.Fatal("long wave was not truncated")
	}
	if strings.Count(got, "[T9]") > MaxPlanWaveLen/10 {
		t.Fatal("truncated wave still carries every item")
	}
}

// Trailer present, plan not found: the reviewer must say so rather than
// invent a plan from the diff.
func TestPlanMatchPromptWithoutLoadedPlanAsksForHuman(t *testing.T) {
	got := planMatchSection(planMatchPR("acme/hive#42", "", ""))
	if !strings.Contains(got, "could not be loaded") || !strings.Contains(got, "requires_human") {
		t.Fatalf("missing-plan instruction absent:\n%s", got)
	}
}

// Acceptance: a fixture PR matching its plan gets a clean report, and that
// report scores as any other clean perspective would.
func TestPlanMatchCleanReportKeepsConfidenceMax(t *testing.T) {
	set := (PerspectiveSet{}).WithPlanMatch()
	reps := append(allApprove(), planMatchReport(VerdictApprove))
	agg := AggregateReports(reps, AggregateOptions{Perspectives: set})
	if agg.Verdict != VerdictApprove || !agg.MergeEligible {
		t.Fatalf("clean plan_match aggregate = %s (%v)", agg.Verdict, agg.Reasons)
	}
	if agg.Confidence.Score != ConfidenceMax || len(agg.Confidence.Reasons) != 0 {
		t.Fatalf("clean plan_match confidence = %+v", agg.Confidence)
	}
	if agg.Perspectives[PerspectivePlanMatch] != VerdictApprove {
		t.Fatalf("plan_match verdict not recorded: %+v", agg.Perspectives)
	}
}

// Acceptance: an unplanned file yields a scope finding, and at high severity
// the finding routes to holdForHuman exactly like any other blocking finding.
func TestPlanMatchScopeFindingHoldsForHuman(t *testing.T) {
	set := (PerspectiveSet{}).WithPlanMatch()
	scope := outputschema.Finding{Title: "scope exceeds plan", Severity: outputschema.SeverityHigh, Summary: "adds pkg/billing/invoice.go, which no planned item covers", File: "pkg/billing/invoice.go", Line: 1}
	reps := append(allApprove(), planMatchReport(VerdictRequiresHuman, scope))
	agg := AggregateReports(reps, AggregateOptions{Perspectives: set})
	if agg.Verdict != VerdictRequiresHuman || !agg.RequiresHuman {
		t.Fatalf("scope finding aggregate = %s", agg.Verdict)
	}
	if len(agg.Reasons) == 0 || !strings.Contains(agg.Reasons[0], "plan_match finding \"scope exceeds plan\" is high") {
		t.Fatalf("scope finding not named in reasons: %v", agg.Reasons)
	}
	if agg.Confidence.Score > ConfidenceNeedsAttentionCap {
		t.Fatalf("scope finding left confidence at %+v", agg.Confidence)
	}
	agg.Repo, agg.Number, agg.HeadSHA = "acme/hive", 8317, "sha1"

	pr := planMatchPR("acme/hive#42", "epic-1", planMatchWave)
	pr.Author = "hive-bot"
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", AIAuthor: "hive-bot", Perspectives: set, Agents: []AgentCapability{{Name: "r1", Enabled: true, UsesKick: true, Role: "review"}}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(plan.State.Human) != 1 || plan.State.Human[0].Number != 8317 {
		t.Fatalf("high scope finding did not hold for human: %+v", plan.State.Human)
	}
	if !strings.Contains(plan.State.Human[0].Reason, "scope exceeds plan") {
		t.Fatalf("hold reason = %q, want the scope finding", plan.State.Human[0].Reason)
	}
	if len(plan.ReviewKicks) != 0 || len(plan.FixKicks) != 0 {
		t.Fatalf("held PR was kicked again: reviews=%d fixes=%d", len(plan.ReviewKicks), len(plan.FixKicks))
	}
}

// Acceptance: a PR without trailers leaves confidence unchanged. The kick
// tells the reviewer to return not-applicable, and that report is dropped
// from both sides of the coverage check.
func TestPlanMatchTrailerlessPRIsNotApplicable(t *testing.T) {
	pr := planMatchPR("", "", "")
	if PlanMatchApplicable(pr) {
		t.Fatal("trailer-less PR reported applicable")
	}
	kick := BuildPerspectivePromptWith(PerspectivePlanMatch, pr, PromptOptions{})
	if !strings.Contains(kick, "not_applicable") || strings.Contains(kick, "approved plan wave this PR claims") {
		t.Fatalf("trailer-less kick should ask for a not-applicable report:\n%s", kick)
	}

	set := (PerspectiveSet{}).WithPlanMatch()
	na := planMatchReport(VerdictApprove)
	na.NotApplicable = true
	na.Summary = "not applicable: no run trailer"
	without := AggregateReports(allApprove(), AggregateOptions{})
	with := AggregateReports(append(allApprove(), na), AggregateOptions{Perspectives: set})
	if with.Confidence.Score != without.Confidence.Score || len(with.Confidence.Reasons) != 0 {
		t.Fatalf("not-applicable report moved confidence: %+v vs %+v", with.Confidence, without.Confidence)
	}
	if with.Verdict != VerdictApprove || !with.MergeEligible {
		t.Fatalf("not-applicable report blocked unanimity: %s (%v)", with.Verdict, with.Reasons)
	}

	// Findings on a not-applicable report carry no weight: there was no plan
	// to find them against.
	stray := planMatchReport(VerdictChangesRequested, finding(outputschema.SeverityHigh))
	stray.NotApplicable = true
	agg := AggregateReports(append(allApprove(), stray), AggregateOptions{Perspectives: set})
	if agg.Verdict != VerdictApprove || agg.Confidence.Score != ConfidenceMax {
		t.Fatalf("not-applicable findings counted: %s %+v", agg.Verdict, agg.Confidence)
	}
}

// The not_applicable flag survives the relay's validation round-trip, and a
// plan_match verdict is refused on a hive that never enabled the perspective.
func TestPlanMatchReportValidation(t *testing.T) {
	raw := []byte(`{"lane":"review-swarm","kind":"review","perspective":"plan_match","verdict":"approve","not_applicable":true,"repo":"acme/hive","number":8317,"summary":"not applicable: no run trailer","findings":[],"prs_opened":[],"beads_filed":[]}`)
	got, err := ValidateReportFor(raw, (PerspectiveSet{}).WithPlanMatch())
	if err != nil {
		t.Fatalf("plan_match verdict rejected on an enabled hive: %v", err)
	}
	if !got.NotApplicable {
		t.Fatal("not_applicable did not round-trip")
	}
	if _, err := ValidateReportFor(raw, PerspectiveSet{}); err == nil {
		t.Fatal("plan_match verdict accepted on a hive that did not enable it")
	}
}

// A plan_match report already on disk must still collect after the
// perspective is toggled off: the hive wrote it, and one stale file must not
// take every other verdict down with it.
func TestCollectAcceptsBuiltinReportOutsideSelectedSet(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(planMatchReport(VerdictApprove))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReviewReportFilePrefix+"plan"+ReviewReportFileSuffix), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := Collect(dir, AggregateOptions{})
	if err != nil {
		t.Fatalf("collect refused a built-in perspective's report: %v", err)
	}
	if len(artifact.Items) != 1 || artifact.Items[0].Perspectives[PerspectivePlanMatch] != VerdictApprove {
		t.Fatalf("plan_match report not collected: %+v", artifact.Items)
	}
}

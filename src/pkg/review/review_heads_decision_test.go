package review

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Part 2 of hivecommons/hive#13542: the decision step, the skip rule,
// per-head retries and head accuracy. Fixtures reuse headsOpts (perspectives
// correctness and security, heads cr-sol and cr-sonnet under base cr).

func headModel(head string) string {
	if head == "cr-sol" {
		return "gpt-6-sol"
	}
	return "claude-sonnet-5-5"
}

func headRep(p Perspective, head string, v Verdict, findings ...outputschema.Finding) PerspectiveReport {
	return PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: p, Verdict: v, Head: head, ReviewModel: headModel(head),
		AgentReport: outputschema.AgentReport{Summary: "summary from " + head, Findings: findings}}
}

func headAggOpts() AggregateOptions {
	set, _ := NewPerspectiveSet([]string{"correctness", "security"}, nil)
	return AggregateOptions{Perspectives: set, ExpectedHeads: []string{"cr-sol", "cr-sonnet"}}
}

var nilDeref = outputschema.Finding{Title: "nil deref", Severity: "medium", Summary: "Load returns nil and the caller dereferences it", File: "a.go", Line: 3, ReviewScope: "in-scope"}

// awaitingAgg: every head answered both perspectives; sol found something.
func awaitingAgg() Aggregate {
	return AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
}

func TestHeadsSettledWithFindingsAwaitDecision(t *testing.T) {
	agg := awaitingAgg()
	if agg.Verdict != VerdictIncomplete || !agg.AwaitingDecision || agg.Decided || agg.DecisionSkipped {
		t.Fatalf("agg = %+v, want incomplete awaiting the decision", agg)
	}
	if len(agg.HeadReports) != 4 || !strings.Contains(strings.Join(agg.Reasons, " "), "decision step") {
		t.Fatalf("head reports = %d reasons = %v", len(agg.HeadReports), agg.Reasons)
	}
}

func TestHeadsNotSettledWaitOnTheMissingHead(t *testing.T) {
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	if agg.Verdict != VerdictIncomplete || agg.AwaitingDecision {
		t.Fatalf("agg = %+v, want incomplete and not yet awaiting a decision", agg)
	}
	if !strings.Contains(strings.Join(agg.Reasons, " "), "security/cr-sol") {
		t.Fatalf("reasons = %v, want the owed pair named", agg.Reasons)
	}
}

func TestHeadsSkipRuleApprovesWithoutADecision(t *testing.T) {
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	if agg.Verdict != VerdictApprove || !agg.MergeEligible || !agg.DecisionSkipped || agg.AwaitingDecision {
		t.Fatalf("agg = %+v, want the skip rule's approve", agg)
	}
}

func TestHeadsMissingHeadCountsAsSettled(t *testing.T) {
	opts := headAggOpts()
	opts.MissingHeads = map[string][]string{reviewKey("acme/hive", 7, "sha1", ""): {"security/cr-sol"}}
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, opts)
	if !agg.AwaitingDecision || len(agg.MissingHeads) != 1 || agg.MissingHeads[0] != "security/cr-sol" {
		t.Fatalf("agg = %+v, want awaiting with the missing head named", agg)
	}
}

func TestDecidedReportsDriveRouting(t *testing.T) {
	decided := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictChangesRequested, Decided: true,
		AgentReport:  outputschema.AgentReport{Summary: "confirmed", Findings: []outputschema.Finding{nilDeref}},
		Dispositions: []FindingDisposition{{Head: "cr-sol", Title: "nil deref", File: "a.go", Line: 3, Disposition: DispositionConfirmed, Reason: "a.go:3 dereferences the nil Load result", Severity: "medium"}}}
	decidedSec := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Verdict: VerdictApprove, Decided: true}
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
		decided, decidedSec,
	}, headAggOpts())
	if !agg.Decided || agg.Verdict != VerdictChangesRequested || !agg.FixCycle {
		t.Fatalf("agg = %+v, want the decided changes_requested routed", agg)
	}
	if len(agg.Findings) != 1 || len(agg.HeadReports) != 4 || len(agg.Dispositions) != 1 || agg.Dispositions[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("findings=%d head reports=%d dispositions=%+v", len(agg.Findings), len(agg.HeadReports), agg.Dispositions)
	}
}

func TestDispositionsAreValidated(t *testing.T) {
	ok := validateDispositions([]FindingDisposition{{Head: "cr-sol", Title: "t", Disposition: DispositionOutOfScope, Reason: "r"}})
	if ok != nil {
		t.Fatalf("valid disposition refused: %v", ok)
	}
	for _, bad := range []FindingDisposition{
		{Head: "cr-sol", Title: "t", Disposition: "maybe", Reason: "r"},
		{Head: "cr-sol", Title: "t", Disposition: DispositionConfirmed},
		{Title: "t", Disposition: DispositionConfirmed, Reason: "r"},
		{Head: "cr-sol", Title: "t", Disposition: DispositionConfirmed, Reason: "r", Severity: "huge"},
	} {
		if err := validateDispositions([]FindingDisposition{bad}); err == nil {
			t.Fatalf("invalid disposition accepted: %+v", bad)
		}
	}
}

func TestDecisionPlannedOnlyOnceHeadsSettle(t *testing.T) {
	opts := headsOpts()
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{awaitingAgg()}}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %+v, want exactly the decision kick", plan.ReviewKicks)
	}
	k := plan.ReviewKicks[0]
	if !k.Decision || k.Agent != "cr" || k.Head != "" || len(k.Perspectives) != 2 {
		t.Fatalf("decision kick = %+v", k)
	}
	decisionPending := 0
	for _, e := range plan.State.Pending {
		if e.Decision && e.Agent == "cr" {
			decisionPending++
		}
	}
	if decisionPending != 2 {
		t.Fatalf("decision pending entries = %d, want one per perspective", decisionPending)
	}
	again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{awaitingAgg()}}, plan.State, opts)
	if len(again.ReviewKicks) != 0 {
		t.Fatalf("decision re-dispatched while one is pending: %+v", again.ReviewKicks)
	}

	unsettled := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	early := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{unsettled}}, DispatchState{}, opts)
	for _, k := range early.ReviewKicks {
		if k.Decision {
			t.Fatalf("decision planned before every head settled: %+v", k)
		}
	}
	if len(early.ReviewKicks) != 1 || early.ReviewKicks[0].Head != "cr-sol" || early.ReviewKicks[0].Perspective != PerspectiveSecurity {
		t.Fatalf("early kicks = %+v, want only the owed head", early.ReviewKicks)
	}
}

func TestDecisionKickCarriesTheFullHeadReports(t *testing.T) {
	opts := headsOpts()
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{awaitingAgg()}}, DispatchState{}, opts)
	msg := plan.ReviewKicks[0].Message
	for _, want := range []string{
		"YOU ARE THE DECISION STEP", "cr-sol", "cr-sonnet", "gpt-6-sol", "claude-sonnet-5-5",
		"summary from cr-sol", "nil deref", "Load returns nil and the caller dereferences it", "a.go:3", "[medium, in-scope]",
		DispositionNotReproducible, "### Head findings", "hive-review 7 --repo acme/hive --comment --body-file",
		"dispositions: one object per head finding", "block_at",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("decision kick missing %q:\n%s", want, msg)
		}
	}
	missing := awaitingAgg()
	missing.MissingHeads = []string{"security/cr-sol"}
	if got := BuildDecisionPrompt(dispatchPR("sha1"), []Perspective{PerspectiveCorrectness, PerspectiveSecurity}, missing, PromptOptions{}); !strings.Contains(got, "HEADS THAT NEVER ANSWERED") || !strings.Contains(got, "security/cr-sol") || !strings.Contains(got, "--record-verdict") {
		t.Fatalf("missing heads or record-only path not in the prompt:\n%s", got)
	}
}

func TestHeadRetriesThenSettleMissingWhenAnotherHeadAnswered(t *testing.T) {
	opts := headsOpts()
	opts.PendingTTL = time.Hour
	opts.HeadMaxRetries = 1
	now := opts.Now
	unsettled := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	artifact := Artifact{Items: []Aggregate{unsettled}}
	state := DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "cr-sol-1", Head: "cr-sol", Dispatched: now.Add(-2 * time.Hour)}}}

	first := PlanDispatch([]PullRequest{dispatchPR("sha1")}, artifact, state, opts)
	if len(first.ReviewKicks) != 1 || first.ReviewKicks[0].Head != "cr-sol" || len(first.State.MissingHeads) != 0 {
		t.Fatalf("first expiry should re-send the head: kicks=%+v missing=%+v", first.ReviewKicks, first.State.MissingHeads)
	}

	opts.Now = now.Add(2 * time.Hour)
	second := PlanDispatch([]PullRequest{dispatchPR("sha1")}, artifact, first.State, opts)
	if len(second.ReviewKicks) != 0 {
		t.Fatalf("head re-sent past head_max_retries: %+v", second.ReviewKicks)
	}
	if len(second.State.MissingHeads) != 1 || second.State.MissingHeads[0].Head != "cr-sol" || second.State.MissingHeads[0].Attempts != 2 {
		t.Fatalf("missing heads = %+v, want cr-sol after 2 attempts", second.State.MissingHeads)
	}

	aggOpts := headAggOpts()
	aggOpts.MissingHeads = MissingHeadsFromState(second.State)
	settled := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, aggOpts)
	third := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{settled}}, second.State, opts)
	if len(third.ReviewKicks) != 1 || !third.ReviewKicks[0].Decision || !strings.Contains(third.ReviewKicks[0].Message, "security/cr-sol") {
		t.Fatalf("decision kick should run without the missing head and name it: %+v", third.ReviewKicks)
	}
}

func TestHeadRetriesWithNoHeadAnsweredKeepTheRedispatchBudget(t *testing.T) {
	opts := headsOpts()
	opts.PendingTTL = time.Hour
	opts.HeadMaxRetries = 1
	opts.MaxRedispatch = 5
	now := opts.Now
	onlyCorrectness := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	old := now.Add(-2 * time.Hour)
	state := DispatchState{
		Pending: []PendingReview{
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "cr-sol-1", Head: "cr-sol", Dispatched: old},
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "cr-sonnet-1", Head: "cr-sonnet", Dispatched: old},
		},
		HeadRetries: []HeadRetryCount{
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Head: "cr-sol", Count: 1},
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Head: "cr-sonnet", Count: 1},
		},
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{onlyCorrectness}}, state, opts)
	if len(plan.State.MissingHeads) != 0 {
		t.Fatalf("a perspective no head answered was settled as missing: %+v", plan.State.MissingHeads)
	}
	if len(plan.State.Redispatches) != 1 || plan.State.Redispatches[0].Count != 1 {
		t.Fatalf("redispatches = %+v, want the PR head's max_redispatch budget charged", plan.State.Redispatches)
	}
}

func TestSkipRuleAcknowledgesOnce(t *testing.T) {
	clean := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	opts := headsOpts()
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{clean}}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 0 || len(plan.Acknowledgements) != 1 {
		t.Fatalf("kicks=%+v acks=%+v, want no session and one acknowledgement", plan.ReviewKicks, plan.Acknowledgements)
	}
	if body := plan.Acknowledgements[0].Body; !strings.HasPrefix(body, "**Reviewed** — no findings from correctness, security") || !strings.Contains(body, "cr-sol, cr-sonnet") {
		t.Fatalf("acknowledgement = %q", body)
	}
	again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{clean}}, plan.State, opts)
	if len(again.Acknowledgements) != 0 {
		t.Fatalf("acknowledged twice: %+v", again.Acknowledgements)
	}
	opts.AcknowledgeNoFindings = false
	quiet := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{clean}}, DispatchState{}, opts)
	if len(quiet.Acknowledgements) != 0 {
		t.Fatalf("acknowledged with acknowledge_no_findings off: %+v", quiet.Acknowledgements)
	}
}

func TestModelStampsKeepEachHeadsModel(t *testing.T) {
	stamps := ModelStampsFromState(DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "cr-sol-1", Head: "cr-sol", ReviewModel: "gpt-6-sol"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "cr-sonnet-1", Head: "cr-sonnet", ReviewModel: "claude-sonnet-5-5"},
	}})
	for head, want := range map[string]string{"cr-sol": "gpt-6-sol", "cr-sonnet": "claude-sonnet-5-5"} {
		r := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Head: head}
		stamps.apply(&r)
		if r.ReviewModel != want {
			t.Fatalf("head %s stamped %q, want %q", head, r.ReviewModel, want)
		}
	}
}

func TestReviewerAccuracyScoresHeadsAgainstTheDecision(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	item := Aggregate{
		Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Decided: true, RecordedAt: now,
		Verdict:      VerdictChangesRequested,
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictChangesRequested, PerspectiveSecurity: VerdictApprove},
		HeadReports: []HeadReport{
			{Perspective: PerspectiveCorrectness, Head: "cr-sol", ReviewModel: "gpt-6-sol", Verdict: VerdictChangesRequested, Findings: []outputschema.Finding{nilDeref, {Title: "typo", Severity: "low", Summary: "s"}}},
			{Perspective: PerspectiveCorrectness, Head: "cr-sonnet", ReviewModel: "claude-sonnet-5-5", Verdict: VerdictApprove},
			{Perspective: PerspectiveSecurity, Head: "cr-sol", ReviewModel: "gpt-6-sol", Verdict: VerdictApprove},
			{Perspective: PerspectiveSecurity, Head: "cr-sonnet", ReviewModel: "claude-sonnet-5-5", Verdict: VerdictChangesRequested, Findings: []outputschema.Finding{{Title: "token logged", Severity: "high", Summary: "s"}}},
		},
		Dispositions: []FindingDisposition{
			{Perspective: PerspectiveCorrectness, Head: "cr-sol", Title: "nil deref", Disposition: DispositionConfirmed, Reason: "r"},
			{Perspective: PerspectiveCorrectness, Head: "cr-sol", Title: "typo", Disposition: DispositionOutOfScope, Reason: "r"},
			{Perspective: PerspectiveSecurity, Head: "cr-sonnet", Title: "token logged", Disposition: DispositionNotReproducible, Reason: "r"},
		},
	}
	sum := SummarizeReviewerAccuracy(now, 0, Artifact{Items: []Aggregate{item}}, nil, nil)
	got := map[string]ReviewerHeadAgreementGroup{}
	for _, g := range sum.ReviewHeads {
		got[g.Name] = g
	}
	sol, sonnet := got["gpt-6-sol"], got["claude-sonnet-5-5"]
	if sol.Samples != 2 || sol.Agreements != 2 || sol.FindingsRaised != 2 || sol.FindingsConfirmed != 1 || sol.FindingsRejected != 1 || sol.ConfirmRate != 0.5 {
		t.Fatalf("sol = %+v", sol)
	}
	if sonnet.Samples != 2 || sonnet.MissedBlocks != 1 || sonnet.OverBlocks != 1 || sonnet.FindingsRejected != 1 || sonnet.AgreementRate != 0 {
		t.Fatalf("sonnet = %+v", sonnet)
	}
	undecided := item
	undecided.Decided = false
	if s := SummarizeReviewerAccuracy(now, 0, Artifact{Items: []Aggregate{undecided}}, nil, nil); len(s.ReviewHeads) != 0 {
		t.Fatalf("an undecided aggregate was scored: %+v", s.ReviewHeads)
	}
}

func TestRemovedPerspectiveDoesNotBlockHeads(t *testing.T) {
	for _, clean := range []bool{false, true} {
		reports := []PerspectiveReport{
			headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
			headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
			headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
			headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
			headRep(Perspective("removed"), "cr-sol", VerdictChangesRequested, nilDeref),
		}
		if !clean {
			reports[0] = headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref)
		}
		agg := AggregateReports(reports, headAggOpts())
		if !agg.AwaitingDecision || agg.DecisionSkipped {
			t.Fatalf("aggregate = %+v, want awaiting decision", agg)
		}
		if len(agg.HeadReports) != 5 || strings.Contains(strings.Join(agg.Reasons, " "), "removed/") {
			t.Fatalf("historical evidence or reasons incorrect: %+v", agg)
		}
		for _, p := range decisionPerspectives(agg, headAggOpts().Perspectives) {
			if p == Perspective("removed") {
				t.Fatalf("decision includes removed perspective: %+v", agg)
			}
		}
	}
}

func TestRemovedPerspectiveDoesNotCountAsCoverage(t *testing.T) {
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(Perspective("removed"), "cr-sol", VerdictApprove),
	}, headAggOpts())
	if agg.Verdict != VerdictIncomplete || agg.DecisionSkipped || agg.AwaitingDecision {
		t.Fatalf("aggregate = %+v, want incomplete configured coverage", agg)
	}
}

func TestDecisionPromptIncludesRemovedPerspectiveEvidence(t *testing.T) {
	agg := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictApprove),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sol", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
		headRep(Perspective("removed"), "cr-sol", VerdictChangesRequested, nilDeref),
	}, headAggOpts())
	prompt := BuildDecisionPrompt(PullRequest{}, decisionPerspectives(agg, headAggOpts().Perspectives), agg, PromptOptions{})
	if !strings.Contains(prompt, "[perspective:removed]") || !strings.Contains(prompt, nilDeref.Title) || !strings.Contains(prompt, "do not return a separate verdict") {
		t.Fatalf("historical finding missing from decision prompt: %s", prompt)
	}
}

func TestRemovedPerspectiveReportedHeadClearsPending(t *testing.T) {
	pr := PullRequest{Repo: "acme/hive", Number: 7, HeadSHA: "sha"}
	pending := []PendingReview{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: Perspective("removed"), Head: "cr-sol"}, {Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: Perspective("removed"), Head: "cr-sonnet"}}
	got := removePendingForPerspectivesAndHeads(pending, pr, nil, map[Perspective][]string{Perspective("removed"): {"cr-sol"}})
	if len(got) != 1 || got[0].Head != "cr-sonnet" {
		t.Fatalf("pending = %+v, want only unanswered head", got)
	}
}

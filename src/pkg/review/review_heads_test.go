package review

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// headReviewer is one replica of a review head derived from base "cr".
func headReviewer(name, head, backend, model string) AgentCapability {
	a := reviewer(name)
	a.ReviewHead, a.ReviewHeadOf, a.Backend, a.Model = head, "cr", backend, model
	return a
}

func headsFleet() []AgentCapability {
	base := reviewer("cr")
	base.HasReviewHeads = true
	return []AgentCapability{
		base,
		headReviewer("cr-sonnet-1", "cr-sonnet", "claude", "claude-sonnet-5-5"),
		headReviewer("cr-sonnet-2", "cr-sonnet", "claude", "claude-sonnet-5-5"),
		headReviewer("cr-sol-1", "cr-sol", "codex", "gpt-6-sol"),
	}
}

func headsOpts() DispatchOptions {
	set, _ := NewPerspectiveSet([]string{"correctness", "security"}, nil)
	return DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 10, Perspectives: set,
		ReviewerAgents: []string{"cr"}, Agents: headsFleet(), AIAuthor: "hive", ProjectOrg: "acme",
		PostComments: true, AcknowledgeNoFindings: true,
		Now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	}
}

func TestReviewCapableAdmitsHeadsThroughTheirBase(t *testing.T) {
	set := ReviewerAgentSet([]string{"cr"})
	if !ReviewCapable(headReviewer("cr-sol-1", "cr-sol", "codex", "m"), set) {
		t.Fatal("head of a listed base was not admitted")
	}
	stranger := headReviewer("x-sol-1", "x-sol", "codex", "m")
	stranger.ReviewHeadOf = "x"
	if ReviewCapable(stranger, set) {
		t.Fatal("head of an unlisted base was admitted")
	}
}

func TestHeadsDispatchEveryPerspectiveToEveryHead(t *testing.T) {
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, headsOpts())
	got := map[string]bool{}
	for _, k := range plan.ReviewKicks {
		if k.Agent == "cr" {
			t.Fatalf("the base (decision agent) was dispatched as a head: %+v", k)
		}
		got[string(k.Perspective)+"/"+k.Head] = true
		if !strings.HasPrefix(k.Agent, k.Head+"-") {
			t.Fatalf("kick for head %s went to %s", k.Head, k.Agent)
		}
	}
	for _, want := range []string{"correctness/cr-sol", "correctness/cr-sonnet", "security/cr-sol", "security/cr-sonnet"} {
		if !got[want] {
			t.Errorf("missing kick %s; got %v", want, got)
		}
	}
	if len(plan.ReviewKicks) != 4 || len(plan.State.Pending) != 4 {
		t.Fatalf("kicks=%d pending=%d, want 4 each", len(plan.ReviewKicks), len(plan.State.Pending))
	}
	for _, p := range plan.State.Pending {
		if p.Head == "" {
			t.Fatalf("pending entry without head: %+v", p)
		}
	}
	// The two sonnet kicks spread across both sonnet replicas (least loaded).
	sonnetAgents := map[string]bool{}
	for _, k := range plan.ReviewKicks {
		if k.Head == "cr-sonnet" {
			sonnetAgents[k.Agent] = true
		}
	}
	if len(sonnetAgents) != 2 {
		t.Fatalf("sonnet kicks did not spread across replicas: %v", sonnetAgents)
	}

	// Next cycle: everything pending, nothing new.
	if again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, plan.State, headsOpts()); len(again.ReviewKicks) != 0 {
		t.Fatalf("re-dispatched pending head reviews: %+v", again.ReviewKicks)
	}
}

func TestHeadKicksAreRecordOnlyAndBlind(t *testing.T) {
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, headsOpts())
	msg := plan.ReviewKicks[0].Message
	for _, want := range []string{
		"INDEPENDENT REVIEW HEAD (" + plan.ReviewKicks[0].Head + ")",
		"--record-verdict --verdict-file <verdict>",
		"Do NOT post anything on the PR",
		"Do NOT read, quote or respond to other reviews",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("head kick missing %q", want)
		}
	}
	if strings.Contains(msg, "--comment --body-file") || strings.Contains(msg, "**Reviewed**") {
		t.Fatalf("head kick tells the reviewer to post a comment:\n%s", msg)
	}
}

func TestHeadsSlotsAndPerPRCap(t *testing.T) {
	opts := headsOpts()
	opts.MaxParallelReviews = 3
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("kicks=%d, want max_parallel_reviews=3", len(plan.ReviewKicks))
	}

	opts = headsOpts()
	opts.MaxPerspectivesPerPR = 1
	plan = PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	perspectives := map[Perspective]int{}
	for _, k := range plan.ReviewKicks {
		perspectives[k.Perspective]++
	}
	if len(perspectives) != 1 || len(plan.ReviewKicks) != 2 {
		t.Fatalf("per-PR cap: kicks by perspective = %v, want one perspective asked of both heads", perspectives)
	}
}

func TestHeadsReaskOnlyTheHeadThatHasNotReported(t *testing.T) {
	opts := headsOpts()
	// correctness: both heads approved; security: only sonnet approved.
	agg := AggregateReports([]PerspectiveReport{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Head: "cr-sonnet"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Head: "cr-sol"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Verdict: VerdictApprove, Head: "cr-sonnet"},
	}, AggregateOptions{Perspectives: opts.Perspectives, ExpectedHeads: []string{"cr-sol", "cr-sonnet"}})
	if agg.Verdict != VerdictIncomplete || !strings.Contains(strings.Join(agg.Reasons, " "), "security/cr-sol") {
		t.Fatalf("aggregate = %s %v, want incomplete waiting on security/cr-sol", agg.Verdict, agg.Reasons)
	}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "cr-sol-1", Head: "cr-sol", Dispatched: opts.Now},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "cr-sonnet-1", Head: "cr-sonnet", Dispatched: opts.Now},
	}}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{agg}}, state, opts)
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("re-asked while sol's security review is still pending: %+v", plan.ReviewKicks)
	}
	// sonnet's security entry was answered and dropped; sol's keeps its TTL.
	if len(plan.State.Pending) != 1 || plan.State.Pending[0].Head != "cr-sol" {
		t.Fatalf("pending = %+v, want only sol's security entry", plan.State.Pending)
	}

	// With sol's entry gone (e.g. expired), only security/cr-sol is asked.
	plan = PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveSecurity || plan.ReviewKicks[0].Head != "cr-sol" {
		t.Fatalf("kicks = %+v, want only security for cr-sol", plan.ReviewKicks)
	}
}

func TestHeadsAggregateStrictestVerdictAndHeads(t *testing.T) {
	agg := AggregateReports([]PerspectiveReport{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictChangesRequested, Head: "cr-sol",
			AgentReport: outputschema.AgentReport{Findings: []outputschema.Finding{{Title: "nil deref", Severity: "medium", File: "a.go", Line: 3}}}},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Head: "cr-sonnet"},
	}, AggregateOptions{ExpectedHeads: []string{"cr-sol", "cr-sonnet"}})
	if agg.Perspectives[PerspectiveCorrectness] != VerdictChangesRequested {
		t.Fatalf("correctness = %s, want the stricter changes_requested", agg.Perspectives[PerspectiveCorrectness])
	}
	if got := agg.Heads[PerspectiveCorrectness]; len(got) != 2 || got[0] != "cr-sol" || got[1] != "cr-sonnet" {
		t.Fatalf("heads = %v", got)
	}
	if agg.Verdict != VerdictChangesRequested || len(agg.Findings) != 1 {
		t.Fatalf("aggregate = %s findings=%d", agg.Verdict, len(agg.Findings))
	}
}

func TestNoHeadsKeepsTodaysDispatch(t *testing.T) {
	opts := headsOpts()
	opts.Agents = []AgentCapability{reviewer("r1"), reviewer("r2")}
	opts.ReviewerAgents = nil
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 2 {
		t.Fatalf("kicks = %d, want one per perspective", len(plan.ReviewKicks))
	}
	for _, k := range plan.ReviewKicks {
		if k.Head != "" || strings.Contains(k.Message, "INDEPENDENT REVIEW HEAD") || !strings.Contains(k.Message, "--comment --body-file") {
			t.Fatalf("plain reviewer got a head kick: %+v", k)
		}
	}
}

func TestCombinedModeIgnoresHeads(t *testing.T) {
	opts := headsOpts()
	opts.CombinedPerspectives = true
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Agent != "cr" || plan.ReviewKicks[0].Head != "" {
		t.Fatalf("combined kicks = %+v, want one combined kick to the base", plan.ReviewKicks)
	}
}

func TestHeadKicksCarryTheHeadModelAndStampPerHead(t *testing.T) {
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, headsOpts())
	for _, k := range plan.ReviewKicks {
		if k.ReviewModel == "" || !strings.Contains(k.Message, "review_model to \""+k.ReviewModel+"\"") {
			t.Fatalf("head kick for %s does not tell the head to record its model %q:\n%s", k.Head, k.ReviewModel, k.Message)
		}
	}
	// Two heads pending on the same perspective: each report is stamped with
	// its own head's model, not the last-recorded one.
	stamps := ModelStampsFromState(plan.State)
	for _, tc := range []struct{ head, want string }{{"cr-sol", "gpt-6-sol"}, {"cr-sonnet", "claude-sonnet-5-5"}} {
		report := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Head: tc.head}
		stamps.apply(&report)
		if report.ReviewModel != tc.want {
			t.Errorf("%s stamped review_model %q, want %q", tc.head, report.ReviewModel, tc.want)
		}
	}
	plain := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness}
	stamps.apply(&plain)
	if plain.ReviewModel != "" {
		t.Fatalf("report without a head took a head's stamp: %q", plain.ReviewModel)
	}
}

func TestHeadsKeepPlainReviewersOnTheOrdinaryPath(t *testing.T) {
	opts := headsOpts()
	opts.Agents = append(headsFleet(), replica("cr-2", "cr"), reviewer("sec"))
	opts.ReviewerAgents = []string{"cr", "sec"}
	heads, others := dispatchReviewers(reviewCapableAgents(opts), false)
	if len(heads) != 3 {
		t.Fatalf("heads = %+v, want the three head replicas", heads)
	}
	if len(others) != 1 || others[0].Name != "sec" {
		t.Fatalf("others = %+v, want only the lane without heads (base and its plain replica excluded)", others)
	}
	// Once the heads have taken every perspective, nothing is left for sec;
	// a slot shortage that leaves a perspective unasked goes to sec next.
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 4 {
		t.Fatalf("kicks = %d, want four head kicks", len(plan.ReviewKicks))
	}
	opts.MaxParallelReviews = 3
	plan = PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("kicks = %d, want max_parallel_reviews", len(plan.ReviewKicks))
	}
	for _, k := range plan.ReviewKicks {
		if k.Agent == "sec" {
			t.Fatalf("sec took a slot ahead of the heads: %+v", plan.ReviewKicks)
		}
	}
}

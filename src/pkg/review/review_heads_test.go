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
	// One review kick in flight per agent: sonnet has two replicas, so both
	// perspectives go out for it at once (one each); sol has one replica, so
	// it takes correctness now and security waits until it answers.
	for _, want := range []string{"correctness/cr-sol", "correctness/cr-sonnet", "security/cr-sonnet"} {
		if !got[want] {
			t.Errorf("missing kick %s; got %v", want, got)
		}
	}
	if got["security/cr-sol"] {
		t.Fatalf("second kick sent to the busy single sol replica: %v", got)
	}
	if len(plan.ReviewKicks) != 3 || len(plan.State.Pending) != 3 {
		t.Fatalf("kicks=%d pending=%d, want 3 each", len(plan.ReviewKicks), len(plan.State.Pending))
	}
	perAgent := map[string]int{}
	for _, k := range plan.ReviewKicks {
		perAgent[k.Agent]++
	}
	for agent, n := range perAgent {
		if n != 1 {
			t.Fatalf("%s got %d review kicks in one cycle, want at most 1: %v", agent, n, perAgent)
		}
	}
	for _, p := range plan.State.Pending {
		if p.Head == "" {
			t.Fatalf("pending entry without head: %+v", p)
		}
	}

	// Next cycle with everyone still busy: nothing new, and nothing stacks.
	if again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, plan.State, headsOpts()); len(again.ReviewKicks) != 0 {
		t.Fatalf("re-dispatched pending head reviews: %+v", again.ReviewKicks)
	}

	// sol answers correctness: its replica is free, so the following cycle
	// asks it security. The sonnet entries are still pending and untouched.
	answered := AggregateReports([]PerspectiveReport{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Verdict: VerdictApprove, Head: "cr-sol"},
	}, AggregateOptions{Perspectives: headsOpts().Perspectives, ExpectedHeads: []string{"cr-sol", "cr-sonnet"}})
	next := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{answered}}, plan.State, headsOpts())
	if len(next.ReviewKicks) != 1 || next.ReviewKicks[0].Agent != "cr-sol-1" || next.ReviewKicks[0].Perspective != PerspectiveSecurity {
		t.Fatalf("freed sol replica was not given security: %+v", next.ReviewKicks)
	}
}

// Two replicas of one head and five perspectives of one PR: each cycle gives
// each replica at most one kick, alternating as they answer, instead of
// stacking four kicks on the first replica in name order.
func TestHeadReplicasTakeOneKickEachUntilTheyAnswer(t *testing.T) {
	opts := headsOpts()
	opts.Perspectives = PerspectiveSet{} // the five built-in perspectives
	base := reviewer("cr")
	base.HasReviewHeads = true
	opts.Agents = []AgentCapability{
		base,
		headReviewer("cr-sonnet-1", "cr-sonnet", "claude", "claude-sonnet-5-5"),
		headReviewer("cr-sonnet-2", "cr-sonnet", "claude", "claude-sonnet-5-5"),
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 2 || plan.ReviewKicks[0].Agent == plan.ReviewKicks[1].Agent {
		t.Fatalf("first cycle kicks = %+v, want one each for cr-sonnet-1 and cr-sonnet-2", plan.ReviewKicks)
	}
	if again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, plan.State, opts); len(again.ReviewKicks) != 0 {
		t.Fatalf("kick stacked on a busy replica: %+v", again.ReviewKicks)
	}
	// cr-sonnet-2 answers its perspective; only it is free next cycle.
	var done Perspective
	var keep []PendingReview
	for _, p := range plan.State.Pending {
		if p.Agent == "cr-sonnet-2" {
			done = p.Perspective
			continue
		}
		keep = append(keep, p)
	}
	answered := AggregateReports([]PerspectiveReport{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: done, Verdict: VerdictApprove, Head: "cr-sonnet"},
	}, AggregateOptions{Perspectives: opts.Perspectives, ExpectedHeads: []string{"cr-sonnet"}})
	state := plan.State
	state.Pending = keep
	next := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{answered}}, state, opts)
	if len(next.ReviewKicks) != 1 || next.ReviewKicks[0].Agent != "cr-sonnet-2" {
		t.Fatalf("next cycle kicks = %+v, want one for the freed cr-sonnet-2", next.ReviewKicks)
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

// Raw head reports are evidence, never a verdict (part 2 of #13542): until
// the decision step records its reconciled reports, a disagreement between
// heads leaves the PR head incomplete. Perspectives still shows the strictest
// head's verdict, and Heads which heads answered, for the queue and planner.
func TestHeadsRawReportsNeverRoute(t *testing.T) {
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
	if agg.Verdict != VerdictIncomplete || agg.FixCycle || agg.RequiresHuman || agg.MergeEligible {
		t.Fatalf("aggregate routed on raw head reports: %+v", agg)
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
	// Three head kicks, not four: the single sol replica takes one perspective
	// and security/cr-sol waits for it to answer (one kick in flight per agent).
	// sec still gets nothing, because security is already pending on sonnet.
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("kicks = %d, want three head kicks", len(plan.ReviewKicks))
	}
	for _, k := range plan.ReviewKicks {
		if k.Agent == "sec" {
			t.Fatalf("sec took a perspective the heads still own: %+v", plan.ReviewKicks)
		}
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

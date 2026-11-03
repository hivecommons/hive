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

// Combined heads (hivecommons/hive#15058): one record-only kick per head
// carrying every perspective it owes, not one per (perspective, head), with
// the pending entries still kept per pair.
func TestCombinedHeadsDispatchOneKickPerHead(t *testing.T) {
	opts := headsOpts()
	opts.CombinedPerspectives = true
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 2 {
		t.Fatalf("kicks = %+v, want one per head", plan.ReviewKicks)
	}
	heads := map[string]bool{}
	for _, k := range plan.ReviewKicks {
		if k.Agent == "cr" || k.Head == "" || !strings.HasPrefix(k.Agent, k.Head+"-") {
			t.Fatalf("combined head kick went to the wrong agent: %+v", k)
		}
		heads[k.Head] = true
		if len(k.Perspectives) != 2 || k.Perspectives[0] != PerspectiveCorrectness || k.Perspectives[1] != PerspectiveSecurity {
			t.Fatalf("kick for %s carries %v, want every perspective", k.Head, k.Perspectives)
		}
		for _, want := range []string{
			"[review-perspective:correctness,security]",
			"INDEPENDENT REVIEW HEAD (" + k.Head + ")",
			"--record-verdict --verdict-file <verdict>",
			"ONE JSON array",
			"Do NOT post anything on the PR",
		} {
			if !strings.Contains(k.Message, want) {
				t.Errorf("combined head kick missing %q", want)
			}
		}
		if strings.Contains(k.Message, "--comment --body-file") || strings.Contains(k.Message, "ONE COMMENT, EVERY PERSPECTIVE") {
			t.Fatalf("combined head kick tells the head to post a comment:\n%s", k.Message)
		}
	}
	if !heads["cr-sol"] || !heads["cr-sonnet"] {
		t.Fatalf("heads kicked = %v, want cr-sol and cr-sonnet", heads)
	}
	pairs := map[string]bool{}
	for _, p := range plan.State.Pending {
		pairs[string(p.Perspective)+"/"+p.Head] = true
	}
	if len(plan.State.Pending) != 4 || len(pairs) != 4 {
		t.Fatalf("pending = %+v, want one entry per (perspective, head)", plan.State.Pending)
	}
	if again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, plan.State, opts); len(again.ReviewKicks) != 0 {
		t.Fatalf("re-dispatched pending combined heads: %+v", again.ReviewKicks)
	}
}

// A combined head that answers some perspectives settles only those pairs;
// the next plan re-asks that head for the rest only, in one kick.
func TestCombinedHeadsReaskOnlyMissingPerspectives(t *testing.T) {
	opts := headsOpts()
	opts.CombinedPerspectives = true
	opts.Perspectives = PerspectiveSet{} // the five built-in perspectives
	all := opts.Perspectives.List()
	if len(all) != 5 {
		t.Fatalf("built-in perspectives = %v, want five", all)
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 2 || len(plan.State.Pending) != 10 {
		t.Fatalf("kicks=%d pending=%d, want 2 kicks and 10 pending pairs", len(plan.ReviewKicks), len(plan.State.Pending))
	}
	// sol records an array with 3 of 5 verdicts; sonnet answers all 5.
	var reports []PerspectiveReport
	for i, p := range all {
		if i < 3 {
			reports = append(reports, PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: p, Verdict: VerdictApprove, Head: "cr-sol"})
		}
		reports = append(reports, PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: p, Verdict: VerdictApprove, Head: "cr-sonnet"})
	}
	agg := AggregateReports(reports, AggregateOptions{Perspectives: opts.Perspectives, ExpectedHeads: []string{"cr-sol", "cr-sonnet"}})
	if agg.Verdict != VerdictIncomplete {
		t.Fatalf("aggregate = %s, want incomplete while sol owes two perspectives", agg.Verdict)
	}
	next := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{agg}}, plan.State, opts)
	if len(next.ReviewKicks) != 0 {
		t.Fatalf("re-asked while sol's missing pairs are still pending: %+v", next.ReviewKicks)
	}
	for _, p := range next.State.Pending {
		if p.Head != "cr-sol" || p.Perspective == all[0] || p.Perspective == all[1] || p.Perspective == all[2] {
			t.Fatalf("settled pair still pending: %+v", p)
		}
	}
	if len(next.State.Pending) != 2 {
		t.Fatalf("pending = %+v, want only sol's two unanswered pairs", next.State.Pending)
	}
	// With sol's entries gone (e.g. expired), sol is re-asked the two it
	// missed, in one kick, and sonnet is not asked again.
	again := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(again.ReviewKicks) != 1 {
		t.Fatalf("kicks = %+v, want one re-ask for cr-sol", again.ReviewKicks)
	}
	k := again.ReviewKicks[0]
	if k.Head != "cr-sol" || len(k.Perspectives) != 2 || k.Perspectives[0] != all[3] || k.Perspectives[1] != all[4] {
		t.Fatalf("re-ask = head %s perspectives %v, want cr-sol for %v", k.Head, k.Perspectives, all[3:])
	}
}

// The decision step is unchanged under combined heads: it fires once every
// head has settled every perspective, with the same prompt.
func TestCombinedHeadsDecisionOnceEveryHeadSettled(t *testing.T) {
	split := headsOpts()
	opts := headsOpts()
	opts.CombinedPerspectives = true
	unsettled := AggregateReports([]PerspectiveReport{
		headRep(PerspectiveCorrectness, "cr-sol", VerdictChangesRequested, nilDeref),
		headRep(PerspectiveCorrectness, "cr-sonnet", VerdictApprove),
		headRep(PerspectiveSecurity, "cr-sonnet", VerdictApprove),
	}, headAggOpts())
	early := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{unsettled}}, DispatchState{}, opts)
	if len(early.ReviewKicks) != 1 || early.ReviewKicks[0].Decision || early.ReviewKicks[0].Head != "cr-sol" || len(early.ReviewKicks[0].Perspectives) != 1 || early.ReviewKicks[0].Perspectives[0] != PerspectiveSecurity {
		t.Fatalf("early kicks = %+v, want only security re-asked of cr-sol", early.ReviewKicks)
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{awaitingAgg()}}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || !plan.ReviewKicks[0].Decision || plan.ReviewKicks[0].Agent != "cr" {
		t.Fatalf("kicks = %+v, want exactly the decision kick", plan.ReviewKicks)
	}
	want := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{Items: []Aggregate{awaitingAgg()}}, DispatchState{}, split)
	if plan.ReviewKicks[0].Message != want.ReviewKicks[0].Message {
		t.Fatal("decision prompt differs under combined perspectives")
	}
}

// A combined head kick fans the perspectives out to parallel sub-agents
// (hivecommons/hive#15058): the head reads the PR once, every perspective's
// focus (overrides included) is in the kick, the head alone records one array
// and nothing is posted. A single-perspective re-ask has nothing to fan out.
func TestCombinedHeadPromptFansOutPerPerspective(t *testing.T) {
	set, err := NewPerspectiveSet(nil, map[string]string{"security": "custom security focus from perspective_prompts"})
	if err != nil {
		t.Fatal(err)
	}
	all := set.List()
	pr := dispatchPR("sha1")
	pr.BodyDigest = "digest1"
	fanOut := []string{
		"FAN OUT: ONE SUB-AGENT PER PERSPECTIVE, IN PARALLEL.",
		"ONCE, yourself",
		"IN PARALLEL, not one after another",
		"must NOT post anything on the PR, must NOT call hive-review",
		"must NOT fetch the PR or its diff again",
		"record it ONCE with the --record-verdict command above. Only you call hive-review.",
		"If sub-agents are unavailable",
		"cover this whole review, not each sub-agent",
	}
	for _, tc := range []struct {
		name         string
		perspectives []Perspective
		opts         PromptOptions
		wantFanOut   bool
	}{
		{name: "head, every perspective", perspectives: all, opts: PromptOptions{HeadRecordOnly: "cr-sol", PostComments: true}, wantFanOut: true},
		{name: "head, two missing perspectives", perspectives: all[3:], opts: PromptOptions{HeadRecordOnly: "cr-sol", PostComments: true}, wantFanOut: true},
		{name: "head, single perspective re-ask", perspectives: all[:1], opts: PromptOptions{HeadRecordOnly: "cr-sol", PostComments: true}},
		{name: "plain combined kick, not a head", perspectives: all, opts: PromptOptions{PostComments: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Perspectives = set
			opts.WritingGuideSection = "WRITING GUIDE MARKER"
			msg := BuildCombinedPrompt(pr, tc.perspectives, opts)
			want := []string{"WRITING GUIDE MARKER", "Head SHA: sha1", "digest1"}
			for _, p := range tc.perspectives {
				want = append(want, "[perspective:"+string(p)+"]", string(p)+" — "+set.Focus(p))
			}
			isHead := opts.HeadRecordOnly != ""
			if isHead {
				want = append(want, "INDEPENDENT REVIEW HEAD (cr-sol)", "Do NOT post anything on the PR", "--record-verdict --verdict-file <verdict>", "ONE JSON array")
			}
			if tc.wantFanOut {
				want = append(want, fanOut...)
			}
			for _, w := range want {
				if !strings.Contains(msg, w) {
					t.Errorf("prompt missing %q", w)
				}
			}
			if !tc.wantFanOut && strings.Contains(msg, "FAN OUT") {
				t.Error("prompt carries a fan-out section it should not")
			}
			if isHead {
				for _, bad := range []string{"--comment --body-file", "ONE COMMENT, EVERY PERSPECTIVE"} {
					if strings.Contains(msg, bad) {
						t.Errorf("combined head prompt tells the head to post: %q", bad)
					}
				}
			}
		})
	}
	if !strings.Contains(BuildCombinedPrompt(pr, all, PromptOptions{HeadRecordOnly: "cr-sol", Perspectives: set}), "custom security focus from perspective_prompts") {
		t.Fatal("perspective_prompts override missing from the combined head prompt")
	}

	// The fan-out must not break the per-head model guarantee: every
	// sub-agent runs on the head's model, and replaces (not adds to) the
	// single pinned sub-agent of the independent-review section.
	pinned := BuildCombinedPrompt(pr, all, PromptOptions{HeadRecordOnly: "cr-sol", Perspectives: set, ReviewBackend: "codex", ReviewModel: "gpt-6-sol"})
	for _, w := range []string{
		"Launch EVERY sub-agent with `model` set to `codex/gpt-6-sol`",
		"instead of that single sub-agent, not in addition to it",
		"cannot pin `gpt-6-sol`, review the perspectives yourself",
		`review_model "gpt-6-sol"`,
	} {
		if !strings.Contains(pinned, w) {
			t.Errorf("fan-out does not pin sub-agents to the head model: missing %q", w)
		}
	}
	unpinned := BuildCombinedPrompt(pr, all, PromptOptions{HeadRecordOnly: "cr-sol", Perspectives: set})
	if !strings.Contains(unpinned, "Run every sub-agent on the model you are running on now") {
		t.Error("fan-out without a configured head model must keep sub-agents on the session's own model")
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
	heads, others := dispatchReviewers(reviewCapableAgents(opts))
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

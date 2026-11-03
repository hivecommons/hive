package review

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
)

func reviewer(name string) AgentCapability {
	return AgentCapability{Name: name, Enabled: true, UsesKick: true, Role: "reviewer"}
}

func replica(name, base string) AgentCapability {
	c := reviewer(name)
	c.ReplicaOf = base
	return c
}

func dispatchPR(sha string) PullRequest {
	return PullRequest{Repo: "hive", Number: 7, Title: "fix bug", Author: "hive-bot[bot]", HeadSHA: sha, URL: "https://example.invalid/pr/7", Lane: "quality"}
}

func TestPlanDispatchStateMachine(t *testing.T) {
	now := time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name        string
		opts        DispatchOptions
		state       DispatchState
		artifact    Artifact
		wantReviews int
		wantPending int
	}{
		{name: "fan out disabled is no-op", opts: DispatchOptions{RequireApproval: true, FanOut: false, Agents: []AgentCapability{reviewer("r1")}}, wantReviews: 0, wantPending: 0},
		{name: "parallel reviewers dispatch up to cap", opts: DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 3, Agents: []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")}}, wantReviews: 3, wantPending: 3},
		{name: "single reviewer dispatches one sequential round", opts: DispatchOptions{RequireApproval: true, FanOut: true, Agents: []AgentCapability{reviewer("r1")}}, wantReviews: 1, wantPending: 1},
		{name: "fresh aggregate suppresses duplicate review", opts: DispatchOptions{RequireApproval: true, FanOut: true, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}, artifact: Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictApprove}}}, wantReviews: 0, wantPending: 0},
		// r1 is mid-review, so only r2 takes a kick this cycle: one review kick
		// in flight per agent, the rest wait instead of stacking on r1.
		{name: "pending same head suppresses duplicate perspective and a busy reviewer waits", opts: DispatchOptions{RequireApproval: true, FanOut: true, MaxParallelReviews: 5, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}, state: DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"}}}, wantReviews: 1, wantPending: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.ProjectOrg = "acme"
			tt.opts.Now = now
			plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, tt.artifact, tt.state, tt.opts)
			if len(plan.ReviewKicks) != tt.wantReviews || len(plan.State.Pending) != tt.wantPending {
				t.Fatalf("reviews=%d pending=%d, want %d/%d", len(plan.ReviewKicks), len(plan.State.Pending), tt.wantReviews, tt.wantPending)
			}
		})
	}
}

func TestPlanDispatchSHAInvalidation(t *testing.T) {
	state := DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "old", Perspective: PerspectiveSecurity, Agent: "r1"}}}
	plan := PlanDispatch([]PullRequest{dispatchPR("new")}, Artifact{}, state, DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}})
	// The stale entry is pruned, freeing r1, so each reviewer takes one kick
	// for the new head.
	if len(plan.State.Pending) != 2 {
		t.Fatalf("old head pending reviews were not invalidated: %+v", plan.State.Pending)
	}
	for _, p := range plan.State.Pending {
		if p.HeadSHA != "new" {
			t.Fatalf("stale pending entry survived: %+v", p)
		}
	}
}

func TestPlanDispatchDoesNotAssignAuthorAgentAsReviewer(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.AuthorAgent = "r1"

	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:    true,
		FanOut:             true,
		ProjectOrg:         "acme",
		MaxParallelReviews: 5,
		Agents:             []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("reviews=%d, want only the non-author reviewer", len(plan.ReviewKicks))
	}
	if plan.ReviewKicks[0].Agent != "r2" || plan.State.Pending[0].Agent != "r2" {
		t.Fatalf("author agent was assigned review work: kicks=%+v pending=%+v", plan.ReviewKicks, plan.State.Pending)
	}

	onlyAuthor := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval: true,
		FanOut:          true,
		ProjectOrg:      "acme",
		Agents:          []AgentCapability{reviewer("r1")},
	})
	if len(onlyAuthor.ReviewKicks) != 0 || len(onlyAuthor.State.Pending) != 0 {
		t.Fatalf("single author reviewer must not create unrecordable pending work: %+v", onlyAuthor)
	}

	legacyState := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"},
	}}
	reassigned := PlanDispatch([]PullRequest{pr}, Artifact{}, legacyState, DispatchOptions{
		RequireApproval:    true,
		FanOut:             true,
		ProjectOrg:         "acme",
		MaxParallelReviews: 5,
		Agents:             []AgentCapability{reviewer("r1"), reviewer("r2")},
	})
	if len(reassigned.ReviewKicks) != 1 || reassigned.ReviewKicks[0].Agent != "r2" || reassigned.ReviewKicks[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("legacy author-owned pending review was not reassigned to a non-author: %+v", reassigned)
	}
}

func TestConfirmDeliveredRollsBackFailedKicks(t *testing.T) {
	planned := []DispatchKick{
		{Kind: "review", Agent: "r1", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness},
		{Kind: "review", Agent: "r2", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity},
		{Kind: "fix", Agent: "quality", Repo: "acme/hive", Number: 7, HeadSHA: "sha1"},
	}
	state := DispatchState{
		Pending: []PendingReview{
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"},
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "r2"},
		},
		Fixes: []PendingFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: "quality", Attempts: 1}},
	}
	got := ConfirmDelivered(state, planned, planned[:1])
	if len(got.Pending) != 1 || got.Pending[0].Perspective != PerspectiveCorrectness {
		t.Fatalf("undelivered review was not rolled back: %+v", got.Pending)
	}
	if len(got.Fixes) != 0 {
		t.Fatalf("undelivered fix was not rolled back: %+v", got.Fixes)
	}
}

// A persist made while the kick pass is still delivering must keep the pending
// rows of a planned kick that has not been confirmed yet: it may already have
// reached its agent, and its verdict binds to those rows (hivecommons/hive#15559).
func TestConfirmDeliveredInProgressKeepsKicksStillDelivering(t *testing.T) {
	first := DispatchKick{Kind: "review", Agent: "r1", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness}
	decision := DispatchKick{Kind: "review", Agent: "base", Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Perspectives: []Perspective{PerspectiveSecurity, PerspectiveStyle}, Decision: true}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1"},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "base", Decision: true},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveStyle, Agent: "base", Decision: true},
	}}

	got := ConfirmDeliveredInProgress(state, []DispatchKick{first})
	if len(got.Pending) != 3 {
		t.Fatalf("in-progress persist dropped pending rows of a kick still delivering: %+v", got.Pending)
	}
	if len(got.Recent) != 1 || got.Recent[0].Agent != "r1" {
		t.Fatalf("in-progress persist did not record the delivered kick: %+v", got.Recent)
	}

	final := ConfirmDelivered(got, []DispatchKick{first, decision}, []DispatchKick{first})
	if len(final.Pending) != 1 || final.Pending[0].Agent != "r1" {
		t.Fatalf("end-of-pass persist kept rows of an undelivered kick: %+v", final.Pending)
	}
}

func TestBuildFixPromptAndCapExhaustion(t *testing.T) {
	finding := PerspectiveFinding{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{Title: "nil panic", Severity: outputschema.SeverityMedium, Summary: "guard nil input"}}
	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictChangesRequested, Findings: []PerspectiveFinding{finding}}
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	prompt := BuildFixPrompt(pr, agg, 1, escalation.MaxReEngagements)
	if !strings.Contains(prompt, "nil panic") || !strings.Contains(prompt, "attempt 1") || !strings.Contains(prompt, "Hive-Fix-Attempt: 1/") || !strings.Contains(prompt, "acme/hive#7") {
		t.Fatalf("fix prompt missing expected context:\n%s", prompt)
	}
	if !strings.Contains(prompt, "git commit -s") || !strings.Contains(prompt, "git rebase --signoff") || !strings.Contains(prompt, "Signed-off-by") {
		t.Fatalf("fix prompt must require sign-off on follow-up commits:\n%s", prompt)
	}
	state := DispatchState{}
	for i := 1; i <= escalation.MaxReEngagements; i++ {
		state.Fixes = append(state.Fixes, PendingFix{Repo: "acme/hive", Number: 7, HeadSHA: "old", Attempts: i})
	}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, state, DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1")}})
	if len(plan.FixKicks) != 0 || len(plan.State.Human) != 1 || plan.State.Human[0].Reason != FixAttemptsExhaustedHumanDecisionReason {
		t.Fatalf("cap exhaustion did not require human: kicks=%d human=%+v", len(plan.FixKicks), plan.State.Human)
	}
}

func TestConfirmDeliveredRecordsFixAttemptOnlyAfterDelivery(t *testing.T) {
	pending := PendingFix{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 1}
	kick := DispatchKick{Kind: "fix", Repo: pending.Repo, Number: pending.Number, HeadSHA: pending.HeadSHA, Agent: pending.Agent}
	planned := []DispatchKick{kick}
	state := DispatchState{Fixes: []PendingFix{pending}}

	undelivered := ConfirmDelivered(state, planned, nil)
	if len(undelivered.FixAttempts) != 0 {
		t.Fatalf("undelivered fix recorded attempts: %+v", undelivered.FixAttempts)
	}

	delivered := ConfirmDelivered(state, planned, planned)
	if len(delivered.FixAttempts) != 1 || delivered.FixAttempts[0].Attempts != 1 {
		t.Fatalf("delivered fix attempts = %+v, want one attempt", delivered.FixAttempts)
	}
	again := ConfirmDelivered(delivered, planned, planned)
	if len(again.FixAttempts) != 1 {
		t.Fatalf("delivered fix attempt duplicated: %+v", again.FixAttempts)
	}
}

func TestFixDispatchFallsBackToAvailableScanner(t *testing.T) {
	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictChangesRequested}
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	pr.Lane = "quality"
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, DispatchOptions{
		RequireApproval: true,
		FanOut:          true,
		ProjectOrg:      "acme",
		Agents: []AgentCapability{
			{Name: "quality", Enabled: false, UsesKick: true},
			{Name: DefaultFixerAgent, Enabled: true, UsesKick: true},
			reviewer("r1"),
		},
	})
	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Agent != DefaultFixerAgent {
		t.Fatalf("fixer fallback = %+v, want scanner", plan.FixKicks)
	}
}

func TestConfigReviewDefaults(t *testing.T) {
	var cfg config.ReviewConfig
	if cfg.FanOut || cfg.RequireApproval {
		t.Fatalf("review fan-out/approval must default off: %+v", cfg)
	}
	if got := cfg.EffectiveMaxParallelReviews(); got != config.DefaultMaxParallelReviews {
		t.Fatalf("default max parallel = %d, want %d", got, config.DefaultMaxParallelReviews)
	}
	cfg.MaxParallelReviews = 2
	if got := cfg.EffectiveMaxParallelReviews(); got != 2 {
		t.Fatalf("configured max parallel = %d, want 2", got)
	}
}

func dispatchPRNum(n int, sha string) PullRequest {
	pr := dispatchPR(sha)
	pr.Number = n
	return pr
}

// TestDispatchSpendsSlotsDepthFirstByDefault documents the status quo the cap
// exists to change: the parallel budget is spent in PR order, so the head of
// the queue absorbs every slot for its own perspectives and the PRs behind it
// get nothing this cycle. That is the right behavior when the queue is short.
func TestDispatchSpendsSlotsDepthFirstByDefault(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2"), dispatchPRNum(3, "sha3")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:    true,
		FanOut:             true,
		MaxParallelReviews: 3,
		ProjectOrg:         "acme",
		Agents:             []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")},
	})

	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("got %d kicks, want 3 (the full slot budget)", len(plan.ReviewKicks))
	}
	for _, k := range plan.ReviewKicks {
		if k.Number != 1 {
			t.Fatalf("uncapped dispatch should concentrate on the first PR, got a kick for #%d", k.Number)
		}
	}
}

// TestMaxPerspectivesPerPRSpreadsAcrossPRs is the point of the cap: the same
// budget, spent breadth-first, reviews every PR in the queue once instead of
// one PR three times.
//
// Coverage is traded away on purpose. Each perspective is a separate review
// comment, so letting the skipped ones through on later cycles produces the
// same pile of comments on one PR, merely spread over an hour. A hive that
// wants every perspective leaves the cap unset, which is the default.
func TestMaxPerspectivesPerPRSpreadsAcrossPRs(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2"), dispatchPRNum(3, "sha3")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		MaxParallelReviews:   3,
		MaxPerspectivesPerPR: 1,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2"), reviewer("r3")},
	})

	if len(plan.ReviewKicks) != 3 {
		t.Fatalf("got %d kicks, want 3", len(plan.ReviewKicks))
	}
	seen := map[int]int{}
	for _, k := range plan.ReviewKicks {
		seen[k.Number]++
	}
	if len(seen) != 3 {
		t.Fatalf("capped dispatch covered %d distinct PRs, want 3: %+v", len(seen), seen)
	}
	for num, n := range seen {
		if n != 1 {
			t.Errorf("PR #%d got %d perspectives, want 1 under the cap", num, n)
		}
	}
}

// TestMaxPerspectivesPerPRNeverExceedsSlotBudget keeps the cap subordinate to
// the parallel budget: raising it must not let dispatch run more reviews at
// once than the hive allows.
func TestMaxPerspectivesPerPRNeverExceedsSlotBudget(t *testing.T) {
	prs := []PullRequest{dispatchPRNum(1, "sha1"), dispatchPRNum(2, "sha2")}
	plan := PlanDispatch(prs, Artifact{}, DispatchState{}, DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		MaxParallelReviews:   2,
		MaxPerspectivesPerPR: 4,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
	})

	if len(plan.ReviewKicks) != 2 {
		t.Fatalf("got %d kicks, want 2 (the slot budget, not the per-PR cap)", len(plan.ReviewKicks))
	}
}

func humanPR(sha string) PullRequest {
	pr := dispatchPR(sha)
	pr.Author = "clubanderson"
	return pr
}

// TestDispatchReviewsEveryAuthorClass pins the owner's decision
// (hivecommons/hive#12974): when review is on, every open PR is dispatched
// regardless of who opened it — hive agent, human, clanker at either trust
// tier, dependabot, any other bot. No author class is skipped and the
// deprecated AllAuthors knob changes nothing either way.
func TestDispatchReviewsEveryAuthorClass(t *testing.T) {
	withLabels := func(pr PullRequest, labels ...string) PullRequest {
		pr.Labels = append(pr.Labels, labels...)
		return pr
	}
	clanker := func(sha, login string, labels ...string) PullRequest {
		pr := dispatchPR(sha)
		pr.Author = login
		return withLabels(pr, labels...)
	}
	tests := []struct {
		name string
		pr   PullRequest
	}{
		{name: "hive agent", pr: dispatchPR("sha1")},
		{name: "human", pr: withLabels(humanPR("sha1"), "author/human", "role/trusted")},
		{name: "clanker trusted", pr: clanker("sha1", "relay-contrib", "author/clanker", "role/trusted")},
		{name: "clanker contributor", pr: clanker("sha1", "relay-contrib", "author/clanker", "role/contributor")},
		{name: "dependabot", pr: clanker("sha1", "dependabot[bot]", "author/bot")},
		{name: "other bot", pr: clanker("sha1", "renovate[bot]", "author/bot")},
		{name: "unlabelled unknown author", pr: clanker("sha1", "someone-new")},
	}
	for _, tt := range tests {
		for _, allAuthors := range []bool{false, true} {
			t.Run(tt.name+"/all_authors="+strconvBool(allAuthors), func(t *testing.T) {
				plan := PlanDispatch([]PullRequest{tt.pr}, Artifact{}, DispatchState{}, DispatchOptions{
					RequireApproval: true,
					FanOut:          true,
					AllAuthors:      allAuthors,
					ProjectOrg:      "acme",
					AIAuthor:        "hive-bot[bot]",
					Agents:          []AgentCapability{reviewer("r1")},
				})
				if len(plan.ReviewKicks) != 1 {
					t.Fatalf("got %d kicks for a %s PR, want 1: %+v", len(plan.ReviewKicks), tt.name, plan.ReviewKicks)
				}
				if plan.ReviewKicks[0].Number != 7 {
					t.Fatalf("dispatched the wrong PR: %+v", plan.ReviewKicks[0])
				}
			})
		}
	}
}

func strconvBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestDispatchOnlyGateIsReviewEnabled pins the one switch that remains: with
// review itself off (require_approval or fan_out), nobody's PR is dispatched —
// a human's no more than an agent's.
func TestDispatchOnlyGateIsReviewEnabled(t *testing.T) {
	for _, opts := range []DispatchOptions{
		{RequireApproval: false, FanOut: true},
		{RequireApproval: true, FanOut: false},
	} {
		opts.ProjectOrg = "acme"
		opts.AIAuthor = "hive-bot[bot]"
		opts.Agents = []AgentCapability{reviewer("r1")}
		plan := PlanDispatch([]PullRequest{humanPR("sha1"), dispatchPR("sha2")}, Artifact{}, DispatchState{}, opts)
		if len(plan.ReviewKicks) != 0 {
			t.Fatalf("review off (%+v) still dispatched: %+v", opts, plan.ReviewKicks)
		}
	}
}

// TestHumanPRReviewedButNeverFixPushed guards the boundary the owner kept:
// reviewing everyone's PR is not a licence to push to it. A changes_requested
// verdict on a human PR withholds the fix kick exactly as before.
func TestHumanPRReviewedButNeverFixPushed(t *testing.T) {
	pr := humanPR("sha1")
	opts := DispatchOptions{
		RequireApproval: true, FanOut: true, PostComments: true, ProjectOrg: "acme", AIAuthor: "hive-bot[bot]",
		Agents: []AgentCapability{reviewer("r1"), {Name: DefaultFixerAgent, Enabled: true, UsesKick: true}},
	}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, opts)
	if len(plan.FixKicks) != 0 {
		t.Fatalf("human PR got a fix push: %+v", plan.FixKicks)
	}
	if len(plan.State.Withheld) != 1 {
		t.Fatalf("expected the withheld-fix audit entry, got %+v", plan.State.Withheld)
	}
}

func TestRequiresHumanVerdictProducesHold(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1")}}

	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictRequiresHuman, RequiresHuman: true,
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman, PerspectiveSecurity: VerdictApprove},
		Reasons:      []string{"correctness finding \"boot path\" is high (threshold high)"}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(plan.State.Human) != 1 || plan.State.Human[0].Number != 7 || plan.State.Human[0].HeadSHA != "sha1" {
		t.Fatalf("requires_human verdict did not produce a hold: %+v", plan.State.Human)
	}
	if !strings.Contains(plan.State.Human[0].Reason, "boot path") {
		t.Fatalf("hold reason should carry the aggregate's reason, got %q", plan.State.Human[0].Reason)
	}
	if len(plan.ReviewKicks) != 0 || len(plan.FixKicks) != 0 {
		t.Fatalf("settled requires_human verdict should not kick anything: reviews=%d fixes=%d", len(plan.ReviewKicks), len(plan.FixKicks))
	}

	// Re-planning must not duplicate the hold.
	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, plan.State, opts)
	if len(again.State.Human) != 1 {
		t.Fatalf("hold duplicated on re-plan: %+v", again.State.Human)
	}

	// An aggregate with no perspectives is an unreviewed PR, not a decision.
	empty := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictRequiresHuman, RequiresHuman: true, Reasons: []string{"no review reports were collected"}}
	plan = PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{empty}}, DispatchState{}, opts)
	if len(plan.State.Human) != 0 {
		t.Fatalf("empty aggregate must not hold for human: %+v", plan.State.Human)
	}

	// An approve verdict never holds.
	ok := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictApprove, Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove}}
	plan = PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{ok}}, DispatchState{}, opts)
	if len(plan.State.Human) != 0 {
		t.Fatalf("approve verdict must not hold for human: %+v", plan.State.Human)
	}
}

// A reject verdict is a human hold on the review pipeline, so the dispatcher
// records it and fans it out once per head like requires_human (#12377).
func TestRejectVerdictProducesHold(t *testing.T) {
	pr := dispatchPR("sha1")
	pr.Repo = "acme/hive"
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer("r1")}}

	agg := Aggregate{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictReject, CloseRecommended: true,
		Perspectives: map[Perspective]Verdict{PerspectiveIntentAlignment: VerdictReject, PerspectiveSecurity: VerdictApprove}}
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
	if len(plan.State.Human) != 1 || plan.State.Human[0].HeadSHA != "sha1" || plan.State.Human[0].Reason != "review verdict reject" {
		t.Fatalf("reject verdict did not produce a hold: %+v", plan.State.Human)
	}
	if len(plan.NewHuman) != 1 {
		t.Fatalf("reject hold not fanned out: new human = %+v", plan.NewHuman)
	}
	if len(plan.ReviewKicks) != 0 || len(plan.FixKicks) != 0 {
		t.Fatalf("settled reject verdict should not kick anything: reviews=%d fixes=%d", len(plan.ReviewKicks), len(plan.FixKicks))
	}
	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, plan.State, opts)
	if len(again.State.Human) != 1 || len(again.NewHuman) != 0 {
		t.Fatalf("re-plan duplicated the reject hold: human=%+v new=%+v", again.State.Human, again.NewHuman)
	}
}

func TestPlanDispatchFixesKnownHiveNormalizedAggregateDespiteStaleHumanHold(t *testing.T) {
	pr := dispatchPR("bbb5676")
	pr.Repo = "acme/hive"
	agg := Aggregate{
		Repo:              "acme/hive",
		Number:            7,
		HeadSHA:           "bbb5676",
		Verdict:           VerdictRequiresHuman,
		RequiresHuman:     true,
		HiveAuthoredKnown: true,
		HiveAuthored:      true,
		Reasons:           []string{`correctness finding "LaneQueuedWork is never populated" is high (threshold high)`},
		Findings: []PerspectiveFinding{{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{
			Title:       "LaneQueuedWork is never populated",
			Severity:    outputschema.SeverityHigh,
			Summary:     "Add the producer in AgentActivityFor or trim the changelog claim.",
			ReviewScope: "in-scope",
		}}},
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictChangesRequested},
	}
	state := DispatchState{Human: []HumanReviewHold{{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: "reviewer verdict requires_human"}}}
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Agents: []AgentCapability{reviewer(DefaultFixerAgent)}}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, state, opts)

	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Kind != "fix" {
		t.Fatalf("fix kicks = %+v, want a stale-label-agnostic fix kick", plan.FixKicks)
	}
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("review kicks = %+v, want no re-review", plan.ReviewKicks)
	}
}

func TestReviewCapableUsesConfiguredSetOverNameToken(t *testing.T) {
	set := ReviewerAgentSet([]string{" auditor ", "", "sec-check"})
	if len(set) != 2 || !set["auditor"] || !set["sec-check"] {
		t.Fatalf("ReviewerAgentSet = %v", set)
	}
	if ReviewCapable(AgentCapability{Name: "reviewer"}, set) {
		t.Fatal("lane outside reviewer_agents judged review-capable")
	}
	if !ReviewCapable(AgentCapability{Name: "auditor"}, set) {
		t.Fatal("lane inside reviewer_agents judged not review-capable")
	}
	if !ReviewCapable(AgentCapability{Name: "qa", Role: "code review"}, nil) {
		t.Fatal("review-token role judged not review-capable with no configured set")
	}
	if ReviewCapable(AgentCapability{Name: "docs-writer"}, nil) {
		t.Fatal("non-review lane judged review-capable with no configured set")
	}
}

func TestReviewersForPRExcludesAuthorFamily(t *testing.T) {
	all := []AgentCapability{reviewer("alpha"), replica("alpha-2", "alpha"), replica("alpha-3", "alpha"), reviewer("beta")}
	tests := []struct {
		name   string
		author string
		want   []string
	}{
		{name: "base author excludes its replicas", author: "alpha", want: []string{"beta"}},
		{name: "replica author excludes base and sibling replicas", author: "alpha-2", want: []string{"beta"}},
		{name: "unrelated author keeps others", author: "beta", want: []string{"alpha", "alpha-2", "alpha-3"}},
		{name: "empty author keeps all", author: "", want: []string{"alpha", "alpha-2", "alpha-3", "beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, r := range reviewersForPR(all, tt.author, agentBase(all, tt.author)) {
				got = append(got, r.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("reviewersForPR() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReviewersForPRResolvesAuthorBaseFromFullAgentSet(t *testing.T) {
	full := []AgentCapability{reviewer("alpha"), replica("alpha-2", "alpha"), replica("alpha-3", "alpha"), reviewer("beta")}
	// The authoring replica alpha-2 was filtered out of the reviewer pool.
	pool := []AgentCapability{full[0], full[2], full[3]}
	var got []string
	for _, r := range reviewersForPR(pool, "alpha-2", agentBase(full, "alpha-2")) {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "beta" {
		t.Errorf("reviewersForPR() = %v, want [beta]", got)
	}
}

func TestCapabilityFromConfigCarriesEveryField(t *testing.T) {
	ac := config.AgentConfig{
		ReplicaOf: "alpha", Enabled: true, Paused: true, OnDemand: true,
		Role: "reviewer", LaneKeywords: []string{"lane"}, DetectKeywords: []string{"detect"},
		Aliases: []string{"alias"}, Backend: "claude", Model: "opus",
	}
	got := CapabilityFromConfig("alpha-2", ac)
	want := AgentCapability{
		Name: "alpha-2", ReplicaOf: "alpha", Enabled: true, Paused: true, OnDemand: true,
		UsesKick: ac.UsesGovernorKick(), Role: "reviewer", LaneKeywords: []string{"lane"},
		DetectKeywords: []string{"detect"}, Aliases: []string{"alias"}, Backend: "claude", Model: "opus",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CapabilityFromConfig() = %+v, want %+v", got, want)
	}
	if baseOf(got) != "alpha" || baseOf(reviewer("beta")) != "beta" {
		t.Error("baseOf() did not resolve replica to base / base to itself")
	}
}

// A caller-resolved AuthorBase wins over the dispatch agent slice, which holds
// only enabled lanes: a disabled or retired authoring replica is absent from
// it, so resolving there would fall back to the raw name and let the base and
// sibling replicas review their own family's PR.
func TestPRAuthorBasePrefersCallerResolvedBase(t *testing.T) {
	enabled := []AgentCapability{reviewer("alpha"), replica("alpha-2", "alpha"), reviewer("beta")}
	pr := PullRequest{AuthorAgent: "alpha-3"}
	if got := prAuthorBase(pr, enabled); got != "alpha-3" {
		t.Fatalf("prAuthorBase() without AuthorBase = %q, want alpha-3", got)
	}
	pr.AuthorBase = "alpha"
	if got := prAuthorBase(pr, enabled); got != "alpha" {
		t.Fatalf("prAuthorBase() = %q, want alpha", got)
	}
	var got []string
	for _, r := range reviewersForPR(enabled, pr.AuthorAgent, prAuthorBase(pr, enabled)) {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "beta" {
		t.Errorf("reviewersForPR() = %v, want [beta]", got)
	}
}

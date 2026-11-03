package review

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// The live case behind hivecommons/hive#13041: PR #12968, hive-authored,
// parked at stage/085-needs-decision on a swarm requires_human verdict for
// head e653b6d, and a maintainer's `/hive decision` with a three-point code
// change that nothing acted on.
const (
	liveCaseRepo   = "hivecommons/hive"
	liveCaseNumber = 12968
	liveCaseHead   = "e653b6d"
	liveCaseBody   = "/hive decision keep the change but (1) gate it behind the flag, (2) drop the retry loop, (3) add the regression test."
)

func liveCasePR() PullRequest {
	return PullRequest{Repo: liveCaseRepo, Number: liveCaseNumber, Title: "fix: tunnel drain", Author: "kubestellar-hive[bot]", HeadSHA: liveCaseHead, URL: "https://github.com/hivecommons/hive/pull/12968", Lane: "quality", Labels: []string{"stage/085-needs-decision", "author/hive"}}
}

func liveCaseAggregate() Aggregate {
	return Aggregate{
		Repo: liveCaseRepo, Number: liveCaseNumber, HeadSHA: liveCaseHead,
		Verdict: VerdictRequiresHuman, RequiresHuman: true, HiveAuthoredKnown: true, HiveAuthored: true,
		Reasons: []string{"architecture finding is high (threshold high)"},
		Findings: []PerspectiveFinding{{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{
			Title: "retry loop masks the drain timeout", Severity: outputschema.SeverityHigh, Summary: "The loop retries past the deadline.", ReviewScope: "in-scope",
		}}},
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictRequiresHuman},
	}
}

func liveCaseRequest() HumanChangeRequest {
	return HumanChangeRequest{Repo: liveCaseRepo, Number: liveCaseNumber, HeadSHA: liveCaseHead, Source: HumanChangeRequestSourceDecision, SourceID: 4242, Actor: "clubanderson", Body: liveCaseBody, URL: "https://github.com/hivecommons/hive/pull/12968#issuecomment-4242"}
}

func humanRequestOpts() DispatchOptions {
	return DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "hivecommons", Agents: []AgentCapability{reviewer(DefaultFixerAgent)}}
}

func TestHumanChangeRequestsRecordDedupesAndSupersedesHeads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hcr.json")
	store := NewHumanChangeRequests(path)
	first := liveCaseRequest()
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if err := store.Record(first); err != nil {
		t.Fatal(err)
	}
	if n := store.Len(); n != 1 {
		t.Fatalf("items after duplicate record = %d, want 1", n)
	}
	got, ok := store.For(liveCaseRepo, liveCaseNumber, liveCaseHead)
	if !ok || got.Body != liveCaseBody || got.Actor != "clubanderson" {
		t.Fatalf("For = %+v, %v", got, ok)
	}
	if _, ok := store.For(liveCaseRepo, liveCaseNumber, "newhead"); ok {
		t.Fatal("request matched a head it was not recorded against")
	}

	// A fresh store at the same path sees the persisted entry.
	if _, ok := NewHumanChangeRequests(path).For(liveCaseRepo, liveCaseNumber, liveCaseHead); !ok {
		t.Fatal("persisted request not reloaded")
	}

	// A request on the new head drops the old head's entry.
	second := first
	second.HeadSHA = "newhead"
	second.SourceID = 4343
	if err := store.Record(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.For(liveCaseRepo, liveCaseNumber, liveCaseHead); ok {
		t.Fatal("old-head request survived a request on the new head")
	}
	if _, ok := store.For(liveCaseRepo, liveCaseNumber, "newhead"); !ok {
		t.Fatal("new-head request missing")
	}

	if err := store.Record(HumanChangeRequest{Repo: liveCaseRepo}); err == nil {
		t.Fatal("incomplete request accepted")
	}
	var nilStore *HumanChangeRequests
	if _, ok := nilStore.For(liveCaseRepo, 1, "x"); ok {
		t.Fatal("nil store returned a request")
	}
	if HumanChangeRequestFor(nil, liveCaseRepo, 1, "x") != nil {
		t.Fatal("nil store resolved a request")
	}
	if HumanChangeRequestFor(store, liveCaseRepo, liveCaseNumber, "newhead") == nil {
		t.Fatal("HumanChangeRequestFor missed the recorded request")
	}
}

// TestPlanDispatchHumanChangeRequestKicksFixOverRequiresHuman reproduces
// #12968: the swarm said requires_human, the maintainer answered with a code
// change, and the planner must kick the author agent exactly once with that
// answer in the prompt.
func TestPlanDispatchHumanChangeRequestKicksFixOverRequiresHuman(t *testing.T) {
	now := time.Date(2026, 10, 9, 17, 29, 0, 0, time.UTC)
	pr := liveCasePR()
	req := liveCaseRequest()
	pr.HumanChangeRequest = &req
	state := DispatchState{Human: []HumanReviewHold{{Repo: liveCaseRepo, Number: liveCaseNumber, HeadSHA: liveCaseHead, Reason: "reviewer verdict requires_human"}}}
	opts := humanRequestOpts()
	opts.Now = now

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{liveCaseAggregate()}}, state, opts)

	if len(plan.FixKicks) != 1 || plan.FixKicks[0].Kind != "fix" || plan.FixKicks[0].HeadSHA != liveCaseHead {
		t.Fatalf("fix kicks = %+v, want one fix kick for %s", plan.FixKicks, liveCaseHead)
	}
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("review kicks = %+v, want none on a head the maintainer already answered", plan.ReviewKicks)
	}
	if len(plan.State.Human) != 0 {
		t.Fatalf("human holds = %+v, want the superseded hold cleared", plan.State.Human)
	}
	msg := plan.FixKicks[0].Message
	for _, want := range []string{
		"Maintainer instruction (@clubanderson via /hive decision, https://github.com/hivecommons/hive/pull/12968#issuecomment-4242)",
		"> " + liveCaseBody,
		"retry loop masks the drain timeout",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, msg)
		}
	}

	// Second sweep, same head: the pending fix dedupes the kick.
	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{liveCaseAggregate()}}, plan.State, opts)
	if len(again.FixKicks) != 0 {
		t.Fatalf("second plan fix kicks = %+v, want none", again.FixKicks)
	}
	if len(again.FixExclusions) != 1 || again.FixExclusions[0].Reason != FixDispatchExcludedPending {
		t.Fatalf("second plan exclusions = %+v, want %s", again.FixExclusions, FixDispatchExcludedPending)
	}

	// The agent pushed: the request no longer matches the head, so the new
	// head goes through the normal review path.
	moved := pr
	moved.HeadSHA = "f00dfeed"
	third := PlanDispatch([]PullRequest{moved}, Artifact{}, again.State, opts)
	if len(third.FixKicks) != 0 {
		t.Fatalf("new head fix kicks = %+v, want none", third.FixKicks)
	}
	if len(third.ReviewKicks) == 0 {
		t.Fatal("new head was not re-reviewed")
	}
}

func TestPlanDispatchHumanChangeRequestWithoutSwarmVerdictStillKicks(t *testing.T) {
	pr := liveCasePR()
	req := liveCaseRequest()
	req.Source = HumanChangeRequestSourceReview
	req.URL = ""
	pr.HumanChangeRequest = &req

	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, humanRequestOpts())

	if len(plan.FixKicks) != 1 {
		t.Fatalf("fix kicks = %+v, want one", plan.FixKicks)
	}
	msg := plan.FixKicks[0].Message
	for _, want := range []string{"via changes-requested review)", "> " + liveCaseBody, "maintainer instruction above is the whole scope"} {
		if !strings.Contains(msg, want) {
			t.Errorf("fix prompt missing %q:\n%s", want, msg)
		}
	}
}

func TestPlanDispatchHumanChangeRequestOnHumanPRIsWithheld(t *testing.T) {
	pr := liveCasePR()
	pr.Author = "alice"
	pr.Labels = nil
	req := liveCaseRequest()
	pr.HumanChangeRequest = &req

	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, humanRequestOpts())

	if len(plan.FixKicks) != 0 {
		t.Fatalf("fix kicks = %+v, want none on a PR the hive did not open", plan.FixKicks)
	}
	if len(plan.State.Withheld) != 1 {
		t.Fatalf("withheld = %+v, want one entry", plan.State.Withheld)
	}
}

func TestBuildFixPromptQuotesMaintainerInstructionVerbatim(t *testing.T) {
	pr := liveCasePR()
	req := liveCaseRequest()
	req.Body = "line one\n\n- bullet `with code`\nline three"
	pr.HumanChangeRequest = &req

	got := BuildFixPrompt(pr, liveCaseAggregate(), 1, 6)

	want := "> line one\n> \n> - bullet `with code`\n> line three\n"
	if !strings.Contains(got, want) {
		t.Fatalf("prompt does not quote the body verbatim:\n%s", got)
	}
	if !strings.Contains(got, "primary instruction") || !strings.Contains(got, "Aggregated review findings:") {
		t.Fatalf("prompt lacks the instruction framing or the swarm findings:\n%s", got)
	}
}

// TestPlanDispatchPendingFixExpires covers #14604: a pending fix whose head
// never moved is replaced by the next bounded attempt after PendingFixTTL.
func TestPlanDispatchPendingFixExpires(t *testing.T) {
	dispatched := time.Date(2026, 10, 9, 17, 29, 0, 0, time.UTC)
	pr := liveCasePR()
	req := liveCaseRequest()
	pr.HumanChangeRequest = &req
	opts := humanRequestOpts()
	for _, tc := range []struct {
		name      string
		age       time.Duration
		ttl       time.Duration
		attempts  int
		wantKicks int
		wantExcl  string
	}{
		{name: "fresh", age: time.Hour, attempts: 1, wantExcl: FixDispatchExcludedPending},
		{name: "expired", age: 3 * time.Hour, attempts: 1, wantKicks: 1},
		{name: "custom ttl", age: 10 * time.Minute, ttl: 5 * time.Minute, attempts: 1, wantKicks: 1},
		{name: "expiry disabled", age: 48 * time.Hour, ttl: -1, attempts: 1, wantExcl: FixDispatchExcludedPending},
		{name: "expired at cap", age: 3 * time.Hour, attempts: escalation.MaxReEngagements, wantExcl: FixDispatchExcludedAttemptsExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := opts
			o.Now = dispatched.Add(tc.age)
			o.PendingFixTTL = tc.ttl
			state := DispatchState{Fixes: []PendingFix{{Repo: liveCaseRepo, Number: liveCaseNumber, HeadSHA: liveCaseHead, Agent: "scanner", Attempts: tc.attempts, Dispatched: dispatched}}}
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{liveCaseAggregate()}}, state, o)
			if len(plan.FixKicks) != tc.wantKicks {
				t.Fatalf("fix kicks = %+v, want %d", plan.FixKicks, tc.wantKicks)
			}
			if tc.wantExcl != "" && (len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != tc.wantExcl) {
				t.Fatalf("exclusions = %+v, want %s", plan.FixExclusions, tc.wantExcl)
			}
			if tc.wantKicks == 1 && plan.State.Fixes[len(plan.State.Fixes)-1].Attempts != tc.attempts+1 {
				t.Fatalf("pending fixes = %+v, want attempts %d", plan.State.Fixes, tc.attempts+1)
			}
		})
	}
}

// hivecommons/hive#14679: a bare `/hive decision approve` is recorded against
// the head so the lanes stop asking for a decision, but it is not a change
// request — the planner must not kick a fix whose instruction is "approve".
func TestHumanChangeRequestApprovalKindIsNotAFixRequest(t *testing.T) {
	store := NewHumanChangeRequests(filepath.Join(t.TempDir(), HumanChangeRequestsFile))
	approve := liveCaseRequest()
	approve.Body, approve.Kind = "approve", HumanChangeRequestKindApprove
	approve.RecordedAt = time.Date(2026, 10, 27, 18, 35, 0, 0, time.UTC)
	if err := store.Record(approve); err != nil {
		t.Fatal(err)
	}
	if got := HumanChangeRequestFor(store, liveCaseRepo, liveCaseNumber, liveCaseHead); got != nil {
		t.Fatalf("HumanChangeRequestFor returned an approval: %+v", got)
	}
	if got := HumanApprovalFor(store, liveCaseRepo, liveCaseNumber, liveCaseHead); got == nil || !got.IsApproval() || got.Actor != "clubanderson" {
		t.Fatalf("HumanApprovalFor = %+v", got)
	}
	if got := HumanApprovalFor(store, liveCaseRepo, liveCaseNumber, "0ther"); got != nil {
		t.Fatalf("approval leaked to another head: %+v", got)
	}
	pr := liveCasePR()
	pr.HumanChangeRequest = HumanChangeRequestFor(store, pr.Repo, pr.Number, pr.HeadSHA)
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, humanRequestOpts())
	if len(plan.FixKicks) != 0 {
		t.Fatalf("a bare approval kicked a fix: %+v", plan.FixKicks)
	}

	// A later instruction on the same head supersedes the approval, and the
	// record survives a reload with its kind.
	changes := liveCaseRequest()
	changes.SourceID, changes.RecordedAt = 4343, approve.RecordedAt.Add(time.Minute)
	if err := store.Record(changes); err != nil {
		t.Fatal(err)
	}
	reloaded := NewHumanChangeRequests(store.path)
	if got := HumanChangeRequestFor(reloaded, liveCaseRepo, liveCaseNumber, liveCaseHead); got == nil || got.SourceID != 4343 || !strings.Contains(got.Body, "gate it behind the flag") {
		t.Fatalf("reloaded request = %+v", got)
	}
	if got := HumanApprovalFor(reloaded, liveCaseRepo, liveCaseNumber, liveCaseHead); got != nil {
		t.Fatalf("superseded approval still standing: %+v", got)
	}
}

// The fix kick carries every line of the decision, not just the first:
// the numbered instructions under `/hive decision approve` on hive#14628
// are the whole scope of the fix (hivecommons/hive#14679).
func TestHumanChangeRequestPromptSectionCarriesEveryDecisionLine(t *testing.T) {
	req := liveCaseRequest()
	req.Source, req.Body = HumanChangeRequestSourceDecision, "approve\n1. Change `Closes #14605` to `Refs #14605`.\n2. Keep #14605 open.\n3. Merge on green."
	got := humanChangeRequestPromptSection(req)
	for _, want := range []string{"> approve\n", "> 1. Change `Closes #14605` to `Refs #14605`.\n", "> 2. Keep #14605 open.\n", "> 3. Merge on green.\n", "via /hive decision"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt section missing %q:\n%s", want, got)
		}
	}
	if req.Directive() != DecisionDirectiveChanges {
		t.Fatalf("directive = %q", req.Directive())
	}
	if _, ok := DecisionAnswerFor(nil, liveCaseRepo, liveCaseNumber, liveCaseHead); ok {
		t.Fatal("nil store answered")
	}
	store := NewHumanChangeRequests("")
	if err := store.Record(req); err != nil {
		t.Fatal(err)
	}
	if got, ok := DecisionAnswerFor(store, liveCaseRepo, liveCaseNumber, liveCaseHead); !ok || got.Actor != req.Actor || got.Directive() != DecisionDirectiveChanges {
		t.Fatalf("DecisionAnswerFor = %+v ok=%v", got, ok)
	}
	if _, ok := DecisionAnswerFor(store, liveCaseRepo, liveCaseNumber, "0ther"); ok {
		t.Fatal("answer leaked onto another head")
	}
}

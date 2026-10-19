package review

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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

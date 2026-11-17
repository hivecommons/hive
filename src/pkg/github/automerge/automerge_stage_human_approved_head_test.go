package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// approvedHeadStage is projectbluefin/utah-packages#1181: a fork PR that
// edits a workflow, parked by the lane at the fork guard (`Canary` refuses
// fork PRs) before it reached the review gate or the workflow fence, with
// the swarm's requires_human verdict. reviews is what GET /pulls/9/reviews
// answers; reads counts those calls.
type approvedHeadStage struct {
	e     *Engine
	clock *contributorLaneClock
	obs   stageObservation
	key   string
	reads *int
}

func newApprovedHeadStage(t *testing.T, reviews []map[string]any) (*approvedHeadStage, func()) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	api.prFiles = map[int][]string{9: {".github/workflows/release.yml"}}
	reads := 0
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/9/reviews" {
			return false
		}
		reads++
		_ = json.NewEncoder(w).Encode(reviews)
		return true
	}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 17, 11, 26, 0, 0, time.UTC)}
	verdict := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "Is the new release step safe to run?"}
	e, _ := humanStageEngine(t, api.url, clock, &verdict, 0)
	e.contributorLanePolicy = func() ContributorLanePolicy {
		return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}, OwnerApprovalReleasesWorkflowFence: true, WorkflowFenceScope: config.ContributorLaneWorkflowFenceScopeAll}
	}
	e.SetMergerAuthorizer(func(login string) bool { return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))] })
	obs := stageObservation{Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha",
		Reason: stageReasonForkGuard + "Canary", ForkGuardChecks: []string{"Canary"}}
	return &approvedHeadStage{e: e, clock: clock, obs: obs, key: stageKey("acme", "widget", 9), reads: &reads}, closeAPI
}

func (s *approvedHeadStage) evidence(at time.Time) stagelabels.Evidence {
	return s.e.humanEvidence(context.Background(), s.e.stageTrackerOrShared(), s.key, s.obs, stagelabels.CauseDecision, at)
}

// The lane noted no approval (it stopped at the fork guard), so the route
// reads it: the 085 note names only the guard and says nothing is left to
// approve. The read is cached per head for humanChecksTTL.
func TestHumanEvidenceReadsApprovalALaneStoppedBefore(t *testing.T) {
	s, closeAPI := newApprovedHeadStage(t, []map[string]any{
		{"id": 1, "state": "APPROVED", "commit_id": "sha", "user": map[string]any{"login": "bob"}},
	})
	defer closeAPI()
	ev := s.evidence(s.clock.now())
	if ev.HumanApprover != "bob" || !ev.CauseAwaitsApproval {
		t.Fatalf("approver = %q awaits = %v, want @bob read for the head", ev.HumanApprover, ev.CauseAwaitsApproval)
	}
	body := stagelabels.DecisionComment(stagelabels.RouteHuman(ev))
	for _, gone := range []string{"the reviewer swarm returned `requires_human`", "reviewer question", "awaiting owner approval", "a trusted merger approves this head"} {
		if strings.Contains(body, gone) {
			t.Fatalf("085 note still says %q:\n%s", gone, body)
		}
	}
	for _, want := range []string{"`Canary`", "nothing more to approve: @bob approved this head"} {
		if !strings.Contains(body, want) {
			t.Fatalf("085 note missing %q:\n%s", want, body)
		}
	}

	if s.evidence(s.clock.now().Add(time.Minute)); *s.reads != 1 {
		t.Fatalf("review reads = %d after a second pass on the head, want 1 (cached)", *s.reads)
	}
	if s.evidence(s.clock.now().Add(humanChecksTTL + time.Minute)); *s.reads != 2 {
		t.Fatalf("review reads = %d past the TTL, want 2", *s.reads)
	}
	s.obs.HeadSHA = "sha2"
	if s.evidence(s.clock.now().Add(humanChecksTTL + time.Minute)); *s.reads != 3 {
		t.Fatalf("review reads = %d for a new head within the TTL, want a fresh read (3)", *s.reads)
	}
}

// No approval on the head: the note is today's, asking for it.
func TestHumanEvidenceWithoutApprovalStillAsks(t *testing.T) {
	s, closeAPI := newApprovedHeadStage(t, nil)
	defer closeAPI()
	ev := s.evidence(s.clock.now())
	if ev.HumanApprover != "" {
		t.Fatalf("approver = %q, want none", ev.HumanApprover)
	}
	body := stagelabels.DecisionComment(stagelabels.RouteHuman(ev))
	for _, want := range []string{"`Canary`", "the reviewer swarm returned `requires_human` @sha", "awaiting owner approval", "a trusted merger approves this head"} {
		if !strings.Contains(body, want) {
			t.Fatalf("085 note missing %q:\n%s", want, body)
		}
	}
}

// An approval a lane already noted is used as is: no extra read.
func TestHumanEvidenceUsesTheNotedApproval(t *testing.T) {
	s, closeAPI := newApprovedHeadStage(t, nil)
	defer closeAPI()
	s.obs.Detail.HumanApprover, s.obs.Detail.HumanApprovalHead = "carol", "sha"
	if ev := s.evidence(s.clock.now()); ev.HumanApprover != "carol" || *s.reads != 0 {
		t.Fatalf("approver = %q reads = %d, want the noted @carol and no read", ev.HumanApprover, *s.reads)
	}
}

// The same-head 085 re-park reads the approval too: an approved head is
// not re-parked on the question it answered, and the approval is stamped on
// the observation for the steps after it.
func TestSameHeadApprovedReadsTheApproval(t *testing.T) {
	s, closeAPI := newApprovedHeadStage(t, []map[string]any{
		{"id": 1, "state": "APPROVED", "commit_id": "sha", "user": map[string]any{"login": "bob"}},
	})
	defer closeAPI()
	obs := s.obs
	obs.Detail.VerdictKnown = true
	obs.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "Is the new release step safe to run?"}
	if !s.e.sameHeadApproved(context.Background(), s.e.stageTrackerOrShared(), s.key, &obs, s.clock.now()) {
		t.Fatal("sameHeadApproved = false for a head @bob approved")
	}
	if obs.Detail.HumanApprover != "bob" || obs.Detail.HumanApprovalHead != "sha" {
		t.Fatalf("detail = %+v, want the approval stamped", obs.Detail)
	}
	route := sameHeadDecisionRoute(obs)
	if route.HumanApprover != "bob" || len(route.Reasons) != 0 {
		t.Fatalf("route = %+v, want the approver and no requires_human reasons", route)
	}

	none, closeNone := newApprovedHeadStage(t, nil)
	defer closeNone()
	plain := none.obs
	plain.Detail = obs.Detail
	plain.Detail.HumanApprover, plain.Detail.HumanApprovalHead = "", ""
	if none.e.sameHeadApproved(context.Background(), none.e.stageTrackerOrShared(), none.key, &plain, none.clock.now()) {
		t.Fatal("sameHeadApproved = true with no approval on the head")
	}
	if r := sameHeadDecisionRoute(plain); len(r.Reasons) == 0 || !strings.Contains(r.Reasons[0], "requires_human") {
		t.Fatalf("unapproved re-park route = %+v, want the requires_human reasons", r)
	}
}

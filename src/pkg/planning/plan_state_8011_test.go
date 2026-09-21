package planning

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#8011: the dashboard could not say whether an issue had a
// plan, what state it was in, who was on its tasks or where their PRs were.
// These pin the derived plan state, the claimant/PR surfacing on the review
// tree, and the checklist mirrored back to the source issue.

func TestPlanStateOf(t *testing.T) {
	cases := []struct {
		name string
		p    PlanSummary
		want string
	}{
		{"stuck beats queued", PlanSummary{PendingDecompose: true, DecomposeFailed: true, PlanStatus: PlanStatusDraft}, PlanStateStuck},
		{"queued", PlanSummary{PendingDecompose: true, PlanStatus: PlanStatusDraft}, PlanStateQueued},
		{"draft awaiting review", PlanSummary{PlanStatus: PlanStatusDraft, ChildrenTotal: 3, ChildrenOpen: 3}, PlanStateReview},
		{"approved with open children", PlanSummary{PlanStatus: PlanStatusApproved, ChildrenTotal: 3, ChildrenOpen: 1}, PlanStateExecuting},
		{"approved all closed", PlanSummary{PlanStatus: PlanStatusApproved, ChildrenTotal: 3, ChildrenOpen: 0}, PlanStateDone},
	}
	for _, tc := range cases {
		if got := PlanStateOf(tc.p); got != tc.want {
			t.Errorf("%s: PlanStateOf = %q, want %q", tc.name, got, tc.want)
		}
	}
	if !(PlanSummary{State: PlanStateReview}).NeedsHuman() || !(PlanSummary{State: PlanStateStuck}).NeedsHuman() {
		t.Error("review and stuck must need a human")
	}
	if (PlanSummary{State: PlanStateExecuting}).NeedsHuman() {
		t.Error("executing must not need a human")
	}
}

func TestListPlans_SetsStateAndIssueLink(t *testing.T) {
	store := newStore(t)
	issue := github.Issue{Repo: "a/b", Number: 7, Title: "big feature", URL: "https://github.com/a/b/issues/7", Labels: []string{"plan"}}
	epic, err := EpicFromIssue(store, issue, "")
	if err != nil {
		t.Fatal(err)
	}
	plans := ListPlans(map[string]*beads.Store{"architect": store})
	if len(plans) != 1 {
		t.Fatalf("got %d plans, want 1", len(plans))
	}
	p := plans[0]
	if p.State != PlanStateQueued || p.IssueRepo != "a/b" || p.IssueNumber != "7" {
		t.Fatalf("fresh issue epic: %+v, want queued + issue link", p)
	}

	if _, err := DecomposeFromOutput(store, epic, "1. [T1] do it [agent_suitable]\n", Options{}); err != nil {
		t.Fatal(err)
	}
	if got := ListPlans(map[string]*beads.Store{"architect": store})[0].State; got != PlanStateReview {
		t.Fatalf("after decompose: state=%q, want review", got)
	}
	if err := ApprovePlan(store, epic.ID); err != nil {
		t.Fatal(err)
	}
	if got := ListPlans(map[string]*beads.Store{"architect": store})[0].State; got != PlanStateExecuting {
		t.Fatalf("after approve: state=%q, want executing", got)
	}
	for _, c := range childrenOf(store, epic.ID) {
		if err := store.Close(c.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := ListPlans(map[string]*beads.Store{"architect": store})[0].State; got != PlanStateDone {
		t.Fatalf("after children closed: state=%q, want done", got)
	}
}

func TestGetPlanTree_ClaimantAndPR(t *testing.T) {
	store := newStore(t)
	epic, err := store.Create("epic", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecomposeFromOutput(store, epic, "1. [T1] tagged [agent_suitable]\n2. [T2] via external ref [agent_suitable]\n3. [T3] untouched [human_required]\n", Options{}); err != nil {
		t.Fatal(err)
	}
	kids := childrenOf(store, epic.ID)
	if len(kids) != 3 {
		t.Fatalf("got %d children, want 3", len(kids))
	}
	if err := store.SetMetadata(kids[0].ID, MetaClaimedBy, "coder-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(kids[0].ID, MetaPRURL, "https://github.com/a/b/pull/10"); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(kids[1].ID, func(b *beads.Bead) { b.ExternalRef = "https://github.com/a/b/pull/11" }); err != nil {
		t.Fatal(err)
	}

	tree, err := GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tree.Children[0].ClaimedBy != "coder-1" || tree.Children[0].PRURL != "https://github.com/a/b/pull/10" {
		t.Errorf("T1: %+v, want claimant coder-1 and tagged PR", tree.Children[0])
	}
	if tree.Children[1].PRURL != "https://github.com/a/b/pull/11" {
		t.Errorf("T2: PRURL=%q, want ExternalRef PR fallback", tree.Children[1].PRURL)
	}
	if tree.Children[2].ClaimedBy != "" || tree.Children[2].PRURL != "" {
		t.Errorf("T3: %+v, want no claimant/PR", tree.Children[2])
	}

	body := MirrorChecklist(tree, "https://hive.example/#plan="+epic.ID)
	for _, want := range []string{
		MirrorMarker,
		"- [ ] **T1** tagged — @coder-1 · https://github.com/a/b/pull/10",
		"- [ ] **T2** via external ref — https://github.com/a/b/pull/11",
		"- [ ] **T3** untouched — human",
		"0/3 tasks done",
		"[review in the hive dashboard](https://hive.example/#plan=" + epic.ID + ")",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mirror body missing %q:\n%s", want, body)
		}
	}
	if err := store.Close(kids[0].ID); err != nil {
		t.Fatal(err)
	}
	tree, _ = GetPlanTree(store, epic.ID)
	body = MirrorChecklist(tree, "")
	if !strings.Contains(body, "- [x] **T1** tagged") || !strings.Contains(body, "1/3 tasks done") || strings.Contains(body, "review in the hive dashboard") {
		t.Errorf("closed child not ticked / link not omitted:\n%s", body)
	}
	if MirrorChecklist(nil, "") != "" {
		t.Error("nil tree must render empty")
	}
}

func TestDecomposeStuck_ByAgeWithoutFailureMarker(t *testing.T) {
	store := newStore(t)
	now := withDecomposeClock(t, time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC))
	issue := github.Issue{Repo: "a/b", Number: 3, Title: "button kicked", URL: "https://github.com/a/b/issues/3"}
	epic, err := EpicFromIssue(store, issue, "")
	if err != nil {
		t.Fatal(err)
	}
	if DecomposeStuck(epic) {
		t.Fatal("never-kicked pending epic must not read as stuck")
	}
	if err := RecordDecomposeKick(store, epic.ID, *now); err != nil {
		t.Fatal(err)
	}
	epic, _ = store.Get(epic.ID)
	*now = now.Add(DecomposeStuckAfter - time.Minute)
	if DecomposeStuck(epic) {
		t.Fatal("inside the window must not be stuck")
	}
	*now = now.Add(2 * time.Minute)
	if !DecomposeStuck(epic) {
		t.Fatal("past DecomposeStuckAfter with no children must be stuck")
	}
	if got := ListPlans(map[string]*beads.Store{"a": store})[0].State; got != PlanStateStuck {
		t.Fatalf("ListPlans state=%q, want stuck", got)
	}
	// Children arriving clears pending, and with it the stuck reading.
	if _, err := DecomposeFromOutput(store, epic, "1. [T1] x [agent_suitable]\n", Options{}); err != nil {
		t.Fatal(err)
	}
	epic, _ = store.Get(epic.ID)
	if DecomposeStuck(epic) {
		t.Fatal("decomposed epic must not be stuck")
	}
}

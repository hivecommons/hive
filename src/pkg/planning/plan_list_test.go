package planning

import (
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// ListPlans is the reader behind GET /api/plans (the dashboard plan view's
// listing, #7537). These tests pin its selection (epics with a plan_status,
// across stores), its ordering (drafts first, queued second, approved last,
// ties by title), and its child counts.

func TestListPlans_SelectionAndCounts(t *testing.T) {
	store := newStore(t)

	// An epic never decomposed (no plan_status) must not appear.
	mustEpic(t, store, "undecomposed epic")
	// A plain task must not appear.
	mustTask(t, store, "not an epic")

	epic, res := decomposeEpic(t, store, "decomposed epic", Options{})
	if len(res.Children) == 0 {
		t.Fatal("decompose produced no children")
	}

	plans := ListPlans(map[string]*beads.Store{"architect": store})
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d: %+v", len(plans), plans)
	}
	p := plans[0]
	if p.EpicID != epic.ID || p.Agent != "architect" || p.PlanStatus != PlanStatusDraft {
		t.Fatalf("unexpected summary: %+v", p)
	}
	if p.ChildrenTotal != len(res.Children) || p.ChildrenOpen != len(res.Children) {
		t.Fatalf("want %d/%d children, got %d/%d", len(res.Children), len(res.Children), p.ChildrenOpen, p.ChildrenTotal)
	}
}

func TestListPlans_OrderingDraftsFirst(t *testing.T) {
	store := newStore(t)

	approved, _ := decomposeEpic(t, store, "b approved epic", Options{})
	if err := ApprovePlan(store, approved.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	draftB, _ := decomposeEpic(t, store, "b draft epic", Options{})
	draftA, _ := decomposeEpic(t, store, "a draft epic", Options{})

	plans := ListPlans(map[string]*beads.Store{"architect": store})
	if len(plans) != 3 {
		t.Fatalf("want 3 plans, got %d", len(plans))
	}
	// Drafts first (title-sorted within the group), approved last.
	if plans[0].EpicID != draftA.ID || plans[1].EpicID != draftB.ID || plans[2].EpicID != approved.ID {
		t.Fatalf("wrong order: %s, %s, %s", plans[0].EpicTitle, plans[1].EpicTitle, plans[2].EpicTitle)
	}
}

func TestListPlans_NilAndEmptyStores(t *testing.T) {
	if got := ListPlans(nil); len(got) != 0 {
		t.Fatalf("nil stores: want empty, got %+v", got)
	}
	if got := ListPlans(map[string]*beads.Store{"a": nil}); len(got) != 0 {
		t.Fatalf("nil store value: want empty, got %+v", got)
	}
}

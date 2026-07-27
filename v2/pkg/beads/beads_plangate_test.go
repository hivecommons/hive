package beads

import "testing"

// setPlanMeta is a test helper mirroring the pkg/planning metadata convention
// without importing planning (which would be a cycle). The key strings match
// planning.MetaParentEpic / planning.MetaPlanStatus.
func setPlanMeta(t *testing.T, s *Store, id, key, val string) {
	t.Helper()
	if err := s.SetMetadata(id, key, val); err != nil {
		t.Fatalf("SetMetadata(%s,%s): %v", id, key, err)
	}
}

// TestReady_PlanGate_DraftHidesChildren verifies a child of a draft epic is
// withheld from Ready, while a child of an approved epic is released, and a
// normal (parentless) bead is unaffected.
func TestReady_PlanGate_DraftHidesChildren(t *testing.T) {
	s, _ := NewStore(t.TempDir())

	normal, _ := s.Create("normal", TypeTask, PriorityLow, "worker", "")

	draftEpic, _ := s.Create("draft epic", TypeEpic, PriorityHigh, "architect", "")
	setPlanMeta(t, s, draftEpic.ID, metaPlanStatus, "draft")
	draftChild, _ := s.Create("draft child", TypeTask, PriorityLow, "worker", "")
	setPlanMeta(t, s, draftChild.ID, metaParentEpic, draftEpic.ID)

	approvedEpic, _ := s.Create("approved epic", TypeEpic, PriorityHigh, "architect", "")
	setPlanMeta(t, s, approvedEpic.ID, metaPlanStatus, planStatusApproved)
	approvedChild, _ := s.Create("approved child", TypeTask, PriorityLow, "worker", "")
	setPlanMeta(t, s, approvedChild.ID, metaParentEpic, approvedEpic.ID)

	ready := s.Ready("")
	got := map[string]bool{}
	for _, b := range ready {
		got[b.ID] = true
	}

	if !got[normal.ID] {
		t.Error("normal parentless bead should be ready")
	}
	if got[draftChild.ID] {
		t.Error("child of draft epic must be withheld")
	}
	if !got[approvedChild.ID] {
		t.Error("child of approved epic must be ready")
	}
	// The epics themselves are open beads with no parent_epic → not gated.
	if !got[draftEpic.ID] || !got[approvedEpic.ID] {
		t.Error("epic beads themselves should not be gated")
	}
}

// TestReady_PlanGate_ApprovalReleases verifies flipping the epic's plan_status
// from draft to approved releases the previously-hidden child.
func TestReady_PlanGate_ApprovalReleases(t *testing.T) {
	s, _ := NewStore(t.TempDir())

	epic, _ := s.Create("epic", TypeEpic, PriorityHigh, "architect", "")
	setPlanMeta(t, s, epic.ID, metaPlanStatus, "draft")
	child, _ := s.Create("child", TypeTask, PriorityLow, "worker", "")
	setPlanMeta(t, s, child.ID, metaParentEpic, epic.ID)

	if inReady(s.Ready(""), child.ID) {
		t.Fatal("child should be gated while draft")
	}

	setPlanMeta(t, s, epic.ID, metaPlanStatus, planStatusApproved)
	if !inReady(s.Ready(""), child.ID) {
		t.Fatal("child should be released after approval")
	}
}

// TestReady_PlanGate_MissingParentFailsClosed verifies a child pointing at a
// non-existent epic is withheld (fail-closed).
func TestReady_PlanGate_MissingParentFailsClosed(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	child, _ := s.Create("orphan child", TypeTask, PriorityLow, "worker", "")
	setPlanMeta(t, s, child.ID, metaParentEpic, "no-such-epic")

	if inReady(s.Ready(""), child.ID) {
		t.Fatal("child with missing parent must be withheld (fail-closed)")
	}
}

// TestReady_PlanGate_ActorFilter verifies the gate composes with the actor
// filter: an actor-scoped Ready still hides draft children.
func TestReady_PlanGate_ActorFilter(t *testing.T) {
	s, _ := NewStore(t.TempDir())

	epic, _ := s.Create("epic", TypeEpic, PriorityHigh, "architect", "")
	setPlanMeta(t, s, epic.ID, metaPlanStatus, "draft")
	child, _ := s.Create("child", TypeTask, PriorityLow, "alice", "")
	setPlanMeta(t, s, child.ID, metaParentEpic, epic.ID)
	s.Create("alice normal", TypeTask, PriorityLow, "alice", "")

	ready := s.Ready("alice")
	if inReady(ready, child.ID) {
		t.Error("draft child should be hidden even in actor-scoped Ready")
	}
	if len(ready) != 1 {
		t.Errorf("expected only the normal alice bead, got %d", len(ready))
	}
}

// TestReady_PlanGate_MalformedMetadata verifies a non-string plan_status is
// treated as absent (not "approved"), so the child stays gated.
func TestReady_PlanGate_MalformedMetadata(t *testing.T) {
	s, _ := NewStore(t.TempDir())

	epic, _ := s.Create("epic", TypeEpic, PriorityHigh, "architect", "")
	// Inject a non-string metadata value directly.
	if err := s.Update(epic.ID, func(b *Bead) {
		if b.Metadata == nil {
			b.Metadata = map[string]interface{}{}
		}
		b.Metadata[metaPlanStatus] = 42 // not a string
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	child, _ := s.Create("child", TypeTask, PriorityLow, "worker", "")
	setPlanMeta(t, s, child.ID, metaParentEpic, epic.ID)

	if inReady(s.Ready(""), child.ID) {
		t.Fatal("child of epic with malformed plan_status must be gated")
	}
}

// TestMetaString covers the internal metaString helper's branches.
func TestMetaString(t *testing.T) {
	if metaString(nil, "k") != "" {
		t.Error("nil bead should return empty")
	}
	b := &Bead{}
	if metaString(b, "k") != "" {
		t.Error("nil metadata should return empty")
	}
	b.Metadata = map[string]interface{}{"s": "v", "n": 7}
	if metaString(b, "s") != "v" {
		t.Error("string value should be returned")
	}
	if metaString(b, "n") != "" {
		t.Error("non-string value should return empty")
	}
	if metaString(b, "missing") != "" {
		t.Error("missing key should return empty")
	}
}

func inReady(beads []*Bead, id string) bool {
	for _, b := range beads {
		if b.ID == id {
			return true
		}
	}
	return false
}

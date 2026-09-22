package planning

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// ============================================================
// design.go — ApproveDesign (Gate 1 approval) and the RequestDesign
// branches the label-loop tests don't reach
// ============================================================
//
// ApproveDesign is what both the label path and the dashboard's owner-gated
// "approve design" call to lift Gate 1; until it runs, DesignGated keeps the
// epic out of decomposition. These tests pin the transition and its refusals.

// epicInDesign returns an epic whose design_status is st (or untouched when "").
func epicInDesign(t *testing.T, store *beads.Store, st string) *beads.Bead {
	t.Helper()
	epic := mustEpic(t, store, "Rework the widget pipeline")
	if st != "" {
		if err := store.SetMetadata(epic.ID, MetaDesignStatus, st); err != nil {
			t.Fatalf("seed design_status: %v", err)
		}
	}
	return epic
}

func TestApproveDesign_LiftsGate(t *testing.T) {
	store := newStore(t)
	epic := epicInDesign(t, store, DesignStatusRequested)

	if err := ApproveDesign(store, epic.ID); err != nil {
		t.Fatalf("ApproveDesign: %v", err)
	}
	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if DesignStatus(got) != DesignStatusApproved {
		t.Errorf("design_status = %q, want %q", DesignStatus(got), DesignStatusApproved)
	}
	if DesignGated(got) {
		t.Error("epic still DesignGated after approval — decomposition stays blocked")
	}
}

// TestApproveDesign_NothingToApprove: approving an epic that never entered the
// design step is refused, so a stray approved-label cannot mint an approval
// out of thin air.
func TestApproveDesign_NothingToApprove(t *testing.T) {
	store := newStore(t)
	epic := epicInDesign(t, store, "")

	err := ApproveDesign(store, epic.ID)
	if err == nil || !strings.Contains(err.Error(), "no design to approve") {
		t.Errorf("err = %v, want 'no design to approve'", err)
	}
	if got, _ := store.Get(epic.ID); DesignStatus(got) != "" {
		t.Errorf("design_status = %q, want untouched", DesignStatus(got))
	}
}

func TestApproveDesign_EpicNotFound(t *testing.T) {
	store := newStore(t)
	if err := ApproveDesign(store, "bd-nope"); err == nil {
		t.Error("ApproveDesign on a missing epic: err = nil, want error")
	}
}

// TestApproveDesign_NonEpicRefused: only epics carry Gate 1; approving a task
// bead is a caller bug and must be refused by the shared loadEpic guard.
func TestApproveDesign_NonEpicRefused(t *testing.T) {
	store := newStore(t)
	task, err := store.Create("a task", beads.TypeTask, beads.PriorityHigh, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApproveDesign(store, task.ID); err == nil || !strings.Contains(err.Error(), "not an epic") {
		t.Errorf("err = %v, want 'not an epic'", err)
	}
}

// TestRequestDesign_AlreadyDecomposed: once the epic has been decomposed there
// is nothing left to design; the dashboard affordance must be refused.
func TestRequestDesign_AlreadyDecomposed(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Already planned epic") // no decompose_pending marker

	err := RequestDesign(store, epic.ID)
	if err == nil || !strings.Contains(err.Error(), "already decomposed") {
		t.Errorf("err = %v, want 'already decomposed'", err)
	}
}

// TestRequestDesign_AlreadyInDesign: re-requesting while a design is already
// requested is a silent no-op — it must NOT reset the revision counter the
// label loop maintains.
func TestRequestDesign_AlreadyInDesign(t *testing.T) {
	store := newStore(t)
	epic := epicInDesign(t, store, DesignStatusRequested)
	for k, v := range map[string]string{
		MetaDecomposePending: "true",
		MetaDesignRevision:   "2",
	} {
		if err := store.SetMetadata(epic.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}

	if err := RequestDesign(store, epic.ID); err != nil {
		t.Fatalf("RequestDesign: %v", err)
	}
	got, _ := store.Get(epic.ID)
	if DesignStatus(got) != DesignStatusRequested {
		t.Errorf("design_status = %q, want %q untouched", DesignStatus(got), DesignStatusRequested)
	}
	if DesignRevision(got) != 2 {
		t.Errorf("design_revision = %d, want 2 preserved", DesignRevision(got))
	}
}

// TestRequestDesign_ReopensAfterNeedsHuman: needs_human is the one in-design
// state RequestDesign may leave — a human explicitly asking for a fresh design
// re-queues the epic and clears the exhausted revision counter.
func TestRequestDesign_ReopensAfterNeedsHuman(t *testing.T) {
	store := newStore(t)
	epic := epicInDesign(t, store, DesignStatusNeedsHuman)
	for k, v := range map[string]string{
		MetaDecomposePending: "true",
		MetaDesignRevision:   "3",
	} {
		if err := store.SetMetadata(epic.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}

	if err := RequestDesign(store, epic.ID); err != nil {
		t.Fatalf("RequestDesign: %v", err)
	}
	got, _ := store.Get(epic.ID)
	if DesignStatus(got) != DesignStatusQueued {
		t.Errorf("design_status = %q, want %q", DesignStatus(got), DesignStatusQueued)
	}
	if DesignRevision(got) != 0 {
		t.Errorf("design_revision = %d, want cleared", DesignRevision(got))
	}
}

func TestRequestDesign_EpicNotFound(t *testing.T) {
	store := newStore(t)
	if err := RequestDesign(store, "bd-nope"); err == nil {
		t.Error("RequestDesign on a missing epic: err = nil, want error")
	}
}

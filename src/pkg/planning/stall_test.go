package planning

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// baseTime is a fixed reference "now" for deterministic stall tests.
var baseTime = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// epicSnap builds an approved (or draft) epic SnapBead.
func epicSnap(id, title, planStatus string, replans int) SnapBead {
	return SnapBead{
		ID:          id,
		Title:       title,
		Type:        beads.TypeEpic,
		Status:      beads.StatusInProgress,
		PlanStatus:  planStatus,
		ReplanCount: replans,
		UpdatedAt:   baseTime.Add(-100 * time.Hour),
	}
}

// childSnap builds a child SnapBead with an explicit last-activity time.
func childSnap(id, parent string, status beads.Status, updated, closed time.Time) SnapBead {
	return SnapBead{
		ID:         id,
		Title:      "child " + id,
		Type:       beads.TypeTask,
		Status:     status,
		ParentEpic: parent,
		UpdatedAt:  updated,
		ClosedAt:   closed,
	}
}

func TestDetectStalledPlans(t *testing.T) {
	cfg := StallConfig{StallThreshold: 6 * time.Hour, MaxReplans: 5}
	stale := baseTime.Add(-10 * time.Hour) // older than threshold
	fresh := baseTime.Add(-1 * time.Hour)  // within threshold
	var zero time.Time

	tests := []struct {
		name       string
		snap       []SnapBead
		wantEpics  []string // stalled epic IDs, in returned order
		wantRemain map[string]int
		wantCap    map[string]bool
	}{
		{
			name:      "no approved epics — draft epic never stalls",
			snap:      []SnapBead{epicSnap("e1", "E1", PlanStatusDraft, 0), childSnap("c1", "e1", beads.StatusOpen, stale, zero)},
			wantEpics: nil,
		},
		{
			name:      "empty snapshot",
			snap:      nil,
			wantEpics: nil,
		},
		{
			name: "stalled under cap",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 1),
				childSnap("c1", "e1", beads.StatusDone, stale, stale),
				childSnap("c2", "e1", beads.StatusOpen, stale, zero),
			},
			wantEpics:  []string{"e1"},
			wantRemain: map[string]int{"e1": 1},
			wantCap:    map[string]bool{"e1": false},
		},
		{
			name: "stalled at cap → CapReached",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 5),
				childSnap("c1", "e1", beads.StatusBlocked, stale, zero),
			},
			wantEpics:  []string{"e1"},
			wantRemain: map[string]int{"e1": 1},
			wantCap:    map[string]bool{"e1": true},
		},
		{
			name: "not stalled — recent child activity within window",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 0),
				childSnap("c1", "e1", beads.StatusOpen, fresh, zero),
			},
			wantEpics: nil,
		},
		{
			name: "not stalled — all children finished (no active child)",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 0),
				childSnap("c1", "e1", beads.StatusDone, stale, stale),
				childSnap("c2", "e1", beads.StatusClosed, stale, stale),
			},
			wantEpics: nil,
		},
		{
			name:      "approved epic with no children at all",
			snap:      []SnapBead{epicSnap("e1", "E1", PlanStatusApproved, 0)},
			wantEpics: nil,
		},
		{
			name: "child of a missing/non-approved parent is ignored",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 0),
				childSnap("c1", "ghost", beads.StatusOpen, stale, zero), // parent not in snap
				childSnap("c2", "e1", beads.StatusDone, stale, stale),   // e1 fully done
			},
			wantEpics: nil,
		},
		{
			name: "one child recent keeps whole plan fresh",
			snap: []SnapBead{
				epicSnap("e1", "E1", PlanStatusApproved, 0),
				childSnap("c1", "e1", beads.StatusOpen, stale, zero),
				childSnap("c2", "e1", beads.StatusInProgress, fresh, zero), // recent activity
			},
			wantEpics: nil,
		},
		{
			name: "two stalled epics returned sorted by ID",
			snap: []SnapBead{
				epicSnap("e2", "E2", PlanStatusApproved, 0),
				childSnap("c2", "e2", beads.StatusOpen, stale, zero),
				epicSnap("e1", "E1", PlanStatusApproved, 0),
				childSnap("c1", "e1", beads.StatusOpen, stale, zero),
			},
			wantEpics:  []string{"e1", "e2"},
			wantRemain: map[string]int{"e1": 1, "e2": 1},
			wantCap:    map[string]bool{"e1": false, "e2": false},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectStalledPlans(tc.snap, baseTime, cfg)
			if len(got) != len(tc.wantEpics) {
				t.Fatalf("got %d stalled, want %d: %+v", len(got), len(tc.wantEpics), got)
			}
			for i, sp := range got {
				if sp.EpicID != tc.wantEpics[i] {
					t.Errorf("stalled[%d] epic = %q, want %q", i, sp.EpicID, tc.wantEpics[i])
				}
				if want, ok := tc.wantRemain[sp.EpicID]; ok && len(sp.Remaining) != want {
					t.Errorf("%s remaining = %d, want %d", sp.EpicID, len(sp.Remaining), want)
				}
				if want, ok := tc.wantCap[sp.EpicID]; ok && sp.CapReached != want {
					t.Errorf("%s CapReached = %v, want %v", sp.EpicID, sp.CapReached, want)
				}
				if sp.StalledFor <= 0 {
					t.Errorf("%s StalledFor = %v, want > 0", sp.EpicID, sp.StalledFor)
				}
			}
		})
	}
}

func TestDetectStalledPlans_DefaultsFromZeroConfig(t *testing.T) {
	// Zero StallConfig → DefaultStallThreshold (6h) and MaxReplans (5).
	stale := baseTime.Add(-(DefaultStallThreshold + time.Hour))
	snap := []SnapBead{
		epicSnap("e1", "E1", PlanStatusApproved, MaxReplans),
		childSnap("c1", "e1", beads.StatusOpen, stale, time.Time{}),
	}
	got := DetectStalledPlans(snap, baseTime, StallConfig{})
	if len(got) != 1 {
		t.Fatalf("want 1 stalled with default config, got %d", len(got))
	}
	if !got[0].CapReached {
		t.Errorf("replan_count==MaxReplans should be CapReached under default cap")
	}
}

func TestSnapshotFromStore(t *testing.T) {
	store := newStore(t)
	epic := mustEpic(t, store, "Epic A")
	if err := store.SetMetadata(epic.ID, MetaPlanStatus, PlanStatusApproved); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(epic.ID, MetaReplanCount, "3"); err != nil {
		t.Fatal(err)
	}
	child, err := store.Create("child", beads.TypeTask, beads.PriorityMedium, "architect", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(child.ID, MetaParentEpic, epic.ID); err != nil {
		t.Fatal(err)
	}

	snap := SnapshotFromStore(store)
	if len(snap) != 2 {
		t.Fatalf("want 2 snap beads, got %d", len(snap))
	}
	var gotEpic *SnapBead
	for i := range snap {
		if snap[i].ID == epic.ID {
			gotEpic = &snap[i]
		}
	}
	if gotEpic == nil {
		t.Fatal("epic missing from snapshot")
	}
	if gotEpic.PlanStatus != PlanStatusApproved {
		t.Errorf("PlanStatus = %q", gotEpic.PlanStatus)
	}
	if gotEpic.ReplanCount != 3 {
		t.Errorf("ReplanCount = %d, want 3", gotEpic.ReplanCount)
	}
	if gotEpic.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt not populated")
	}
}

func TestSnapshotFromStore_ClosedAtPopulated(t *testing.T) {
	store := newStore(t)
	b, err := store.Create("done", beads.TypeTask, beads.PriorityMedium, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	snap := SnapshotFromStore(store)
	if len(snap) != 1 || snap[0].ClosedAt.IsZero() {
		t.Fatalf("closed bead should carry a non-zero ClosedAt: %+v", snap)
	}
}

func TestSnapshotFromStore_NilStore(t *testing.T) {
	if got := SnapshotFromStore(nil); got != nil {
		t.Errorf("nil store → nil snapshot, got %+v", got)
	}
}

func TestParseReplanCount(t *testing.T) {
	cases := map[string]int{"": 0, "0": 0, "1": 1, "42": 42, "abc": 0, "1x": 0, "-3": 0}
	for in, want := range cases {
		if got := parseReplanCount(in); got != want {
			t.Errorf("parseReplanCount(%q) = %d, want %d", in, got, want)
		}
	}
}

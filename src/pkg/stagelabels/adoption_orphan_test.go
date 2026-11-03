package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// Orphaned-adoption memory (hivecommons/hive#15052): the closed copy, the
// loop window, the loop-guard park, the once-per-head marker scan; all of
// it resets with the adoption.
func TestStoreAdoptionOrphaned(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 46, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("hivecommons/hive", 14947)

	// Unknown key or no copy: no-op.
	st.NoteAdoptionOrphaned(key, 14977, now)
	st.MarkAdoptMarkerScanned(key, "bc3a6ea")
	if _, ok := st.Get(key); ok {
		t.Fatal("note created a record")
	}
	st.Observe(key, AdoptedByHive, "")
	st.MarkAdopted(key, "bc3a6ea", 14977, now.Add(-6*time.Minute))
	st.NoteAdoptionOrphaned(key, 0, now)
	if r, _ := st.Get(key); r.AdoptOrphanCount != 0 || r.AdoptionOrphanedWithin(now, AdoptionOrphanLoopWindow) {
		t.Fatalf("empty note recorded: %+v", r)
	}

	st.NoteAdoptionOrphaned(key, 14977, now)
	r, _ := st.Get(key)
	if r.AdoptOrphanedBy != 14977 || !r.AdoptOrphanedAt.Equal(now) || r.AdoptOrphanCount != 1 {
		t.Fatalf("orphan not recorded: %+v", r)
	}
	if !r.AdoptionOrphanedWithin(now.Add(59*time.Minute), AdoptionOrphanLoopWindow) || r.AdoptionOrphanedWithin(now.Add(AdoptionOrphanLoopWindow), AdoptionOrphanLoopWindow) {
		t.Fatal("loop window misread")
	}
	// The fresh copy keeps the orphan memory; a second orphan counts up.
	st.MarkAdopted(key, "bc3a6ea", 15060, now)
	st.NoteAdoptionOrphaned(key, 15060, now.Add(10*time.Minute))
	r, _ = st.Get(key)
	if r.AdoptedBy != 15060 || r.AdoptOrphanedBy != 15060 || r.AdoptOrphanCount != 2 {
		t.Fatalf("second orphan: %+v", r)
	}

	// Loop-guard park: escalated at the adopted head for the reason.
	if r.AdoptionChildClosedTwice() {
		t.Fatal("parked before escalation")
	}
	st.MarkAdoptionEscalated(key, "bc3a6ea", LaneReasonAdoptionChildClosedTwice)
	r, _ = st.Get(key)
	if !r.AdoptionChildClosedTwice() {
		t.Fatalf("park not read: %+v", r)
	}
	st.MarkAdoptionEscalated(key, "bc3a6ea", "adoption-stalled:x")
	if r, _ = st.Get(key); r.AdoptionChildClosedTwice() {
		t.Fatal("another reason read as the park")
	}

	// Marker scan: once per head, idempotent, empty head ignored.
	st.MarkAdoptMarkerScanned(key, "")
	st.MarkAdoptMarkerScanned(key, "bc3a6ea")
	st.MarkAdoptMarkerScanned(key, "bc3a6ea")
	if r, _ = st.Get(key); r.AdoptMarkerScanHead != "bc3a6ea" {
		t.Fatalf("scan head: %+v", r)
	}

	// The adoption's end forgets the orphan memory; the scan head survives
	// (the comments do not change when the store does).
	st.ClearAdoption(key)
	r, _ = st.Get(key)
	if r.AdoptOrphanedBy != 0 || !r.AdoptOrphanedAt.IsZero() || r.AdoptOrphanCount != 0 || r.AdoptMarkerScanHead != "bc3a6ea" {
		t.Fatalf("clear: %+v", r)
	}
	if r.AdoptionOrphanedWithin(now, AdoptionOrphanLoopWindow) || r.AdoptionChildClosedTwice() {
		t.Fatal("cleared record still reads orphaned")
	}
}

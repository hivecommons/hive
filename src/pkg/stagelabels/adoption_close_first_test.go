package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// Close-first supersession memory (hivecommons/hive#15135): the original is
// superseded the moment its copy exists, the copy is polled on its own gate,
// red copy heads are counted once each, and a reopen starts the 24h window.
func TestStoreCloseFirstSupersession(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("hivecommons/hive", 15029)
	interval := 10 * time.Minute

	// Unknown key or no copy: no-op.
	st.MarkSupersededClosedFirst(key, 15123, false, now)
	st.MarkCloseFirstSettled(key)
	st.MarkAdoptionReopened(key, 15123, now)
	if st.NoteAdoptCopyRedHead(key, "c1") != 0 {
		t.Fatal("red head on an unknown key")
	}
	if _, ok := st.Get(key); ok {
		t.Fatal("note created a record")
	}
	st.Observe(key, AdoptedByHive, "")
	st.MarkAdopted(key, "aaa", 15123, now)
	st.MarkSupersededClosedFirst(key, 0, false, now)
	if r, _ := st.Get(key); r.SupersededClosedFirst || r.Superseded() {
		t.Fatalf("zero copy recorded: %+v", r)
	}

	st.MarkSupersededClosedFirst(key, 15123, false, now)
	r, _ := st.Get(key)
	if !r.SupersededClosedFirst || r.SupersededBy != 15123 || !r.SupersededAt.Equal(now) || r.SupersededOpen || !r.Superseded() || r.AdoptionCopyFailed() {
		t.Fatalf("close-first not recorded: %+v", r)
	}
	// Superseded: the keep-open poll is off, the close-first poll is on
	// (the adoption's MarkAdoptChecked gates it, from the adoption time).
	if r.SupersederPollDue(now.Add(interval), interval) {
		t.Fatal("keep-open poll still due")
	}
	if r.CloseFirstPollDue(now.Add(interval-time.Second), interval) || !r.CloseFirstPollDue(now.Add(interval), interval) {
		t.Fatal("close-first poll interval misread")
	}
	if (Record{SupersededClosedFirst: true, AdoptedBy: 1, SupersededBy: 1}).CloseFirstPollDue(now, interval) != true || (Record{SupersededClosedFirst: true, AdoptedBy: 1, SupersededBy: 2}).CloseFirstPollDue(now, interval) {
		t.Fatal("close-first poll gate misread")
	}
	// A close GitHub refused: superseded but open.
	st.MarkSupersededClosedFirst(key, 15123, true, now.Add(time.Minute))
	if r, _ = st.Get(key); !r.SupersededOpen || !r.SupersededAt.Equal(now) {
		t.Fatalf("refused close: %+v", r)
	}

	// Red copy heads: distinct heads count, repeats and blanks do not.
	for i, tc := range []struct {
		head string
		want int
	}{{"c1", 1}, {"c1", 1}, {" ", 1}, {"c2", 2}, {"c2", 2}} {
		if got := st.NoteAdoptCopyRedHead(key, tc.head); got != tc.want {
			t.Fatalf("red head %d (%q) = %d, want %d", i, tc.head, got, tc.want)
		}
	}

	// Copy merged: settled, still superseded, no more polling.
	st.MarkCloseFirstSettled(key)
	r, _ = st.Get(key)
	if r.SupersededClosedFirst || len(r.AdoptCopyRedHeads) != 0 || !r.Superseded() || r.CloseFirstPollDue(now.Add(time.Hour), interval) {
		t.Fatalf("settled: %+v", r)
	}
	st.MarkCloseFirstSettled(key) // idempotent

	// Reopen: the supersession is forgotten, the adoption stays (one
	// attempt per head), the window starts, the park reads as copy-failed.
	st.MarkSupersededClosedFirst(key, 15123, false, now)
	st.NoteAdoptCopyRedHead(key, "c1")
	reopened := now.Add(2 * time.Hour)
	st.MarkAdoptionReopened(key, 15123, reopened)
	st.MarkAdoptionEscalated(key, "aaa", LaneReasonAdoptionCopyFailed)
	r, _ = st.Get(key)
	if r.Superseded() || r.SupersededClosedFirst || r.SupersededOpen || !r.SupersededAt.IsZero() || len(r.AdoptCopyRedHeads) != 0 {
		t.Fatalf("reopen left supersession: %+v", r)
	}
	if r.AdoptedBy != 15123 || r.AdoptedHead != "aaa" || r.AdoptReopenedFrom != 15123 || !r.AdoptReopenedAt.Equal(reopened) || !r.AdoptCheckedAt.Equal(reopened) {
		t.Fatalf("reopen memory: %+v", r)
	}
	if !r.AdoptionCopyFailed() || !r.AdoptionEscalated("aaa") {
		t.Fatalf("copy-failed park not read: %+v", r)
	}
	if !r.AdoptionReopenedWithin(reopened.Add(AdoptionReopenWindow-time.Minute), AdoptionReopenWindow) || r.AdoptionReopenedWithin(reopened.Add(AdoptionReopenWindow), AdoptionReopenWindow) {
		t.Fatal("reopen window misread")
	}
	if (Record{}).AdoptionReopenedWithin(now, AdoptionReopenWindow) {
		t.Fatal("zero record read as reopened")
	}
	// Another reason, or a different copy, is not the copy-failed park.
	st.MarkAdoptionEscalated(key, "aaa", LaneReasonAdoptionChildClosedTwice)
	if r, _ = st.Get(key); r.AdoptionCopyFailed() {
		t.Fatal("another reason read as copy-failed")
	}
	st.MarkAdoptionEscalated(key, "aaa", LaneReasonAdoptionCopyFailed)
	st.MarkAdopted(key, "aaa", 15200, reopened.Add(time.Hour))
	r, _ = st.Get(key)
	if r.AdoptionCopyFailed() {
		t.Fatalf("fresh copy still read as failed: %+v", r)
	}
	// A fresh adoption keeps the reopen time (the window outlives the copy).
	if !r.AdoptReopenedAt.Equal(reopened) {
		t.Fatalf("reopen time lost on re-adoption: %+v", r)
	}
	// ClearAdoption keeps the reopen time too but forgets the rest.
	st.ClearAdoption(key)
	r, _ = st.Get(key)
	if r.SupersededClosedFirst || r.AdoptReopenedFrom != 0 || len(r.AdoptCopyRedHeads) != 0 || !r.AdoptReopenedAt.Equal(reopened) {
		t.Fatalf("ClearAdoption: %+v", r)
	}
}

package meetings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandOff(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	st := m.opts.Store
	now := time.Now()
	_ = st.Save(Meeting{ID: "mtg-live", NativeID: "q-live", State: StateLive, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-end", NativeID: "q-end", State: StateEnded, EndedAt: &now, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-local", State: StateFailed, EndedAt: &now, CreatedAt: now})
	if err := m.HandOff(context.Background(), "mtg-live"); !errors.Is(err, ErrNotEnded) {
		t.Fatalf("live = %v", err)
	}
	if err := m.HandOff(context.Background(), "mtg-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if err := m.HandOff(context.Background(), "mtg-end"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("mtg-end")
	if got.State != StateHandedOff || got.HandedOffAt == nil || len(q.deletes) != 1 || q.deletes[0] != "q-end" {
		t.Fatalf("handed off %+v deletes %v", got, q.deletes)
	}
	if err := m.HandOff(context.Background(), "mtg-end"); err != nil || len(q.deletes) != 1 {
		t.Fatalf("second hand-off = %v, deletes %v", err, q.deletes)
	}
	if err := m.HandOff(context.Background(), "mtg-local"); err != nil {
		t.Fatal(err)
	}
	q.deleteErr = &ToolError{Code: CodeNotFound}
	_ = st.Save(Meeting{ID: "mtg-gone", NativeID: "q-gone", State: StateEnded, EndedAt: &now, CreatedAt: now})
	if err := m.HandOff(context.Background(), "mtg-gone"); err != nil {
		t.Fatalf("not_found upstream is fine: %v", err)
	}
	q.deleteErr = &ToolError{Code: CodeUpstreamError}
	_ = st.Save(Meeting{ID: "mtg-err", NativeID: "q-err", State: StateEnded, EndedAt: &now, CreatedAt: now})
	if err := m.HandOff(context.Background(), "mtg-err"); err == nil {
		t.Fatal("want the upstream error")
	}
}

func TestSweepPurgesOldTranscripts(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	st := m.opts.Store
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	_ = st.Save(Meeting{ID: "mtg-old", NativeID: "q-old", State: StateEnded, EndedAt: &old, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-handed", NativeID: "q-h", State: StateHandedOff, EndedAt: &old, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-new", NativeID: "q-new", State: StateEnded, EndedAt: &now, CreatedAt: now})
	_ = st.Save(Meeting{ID: "mtg-live", NativeID: "q-live", State: StateLive, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-noend", State: StateFailed, CreatedAt: old})
	for _, id := range []string{"mtg-old", "mtg-handed", "mtg-new"} {
		_ = st.AppendTranscript(id, Event{Type: EventChat, Text: "x"})
		_ = st.SaveDoc(id, "session.json", map[string]int{"a": 1})
	}
	if n := m.Sweep(context.Background(), 0); n != 0 {
		t.Fatal("zero retention disables the sweeper")
	}
	if n := m.Sweep(context.Background(), 24*time.Hour, "session.json"); n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	for _, id := range []string{"mtg-old", "mtg-handed"} {
		got, _ := m.Get(id)
		tr, _ := st.Transcript(id)
		var doc map[string]int
		if got.State != StatePurged || got.PurgedAt == nil || len(tr) != 0 || !errors.Is(st.LoadDoc(id, "session.json", &doc), ErrNotFound) {
			t.Fatalf("%s not purged: %+v", id, got)
		}
	}
	if tr, _ := st.Transcript("mtg-new"); len(tr) != 1 {
		t.Fatal("a recent meeting keeps its transcript")
	}
	if len(q.deletes) != 1 || q.deletes[0] != "q-old" {
		t.Fatalf("only the never-handed-off meeting is deleted upstream: %v", q.deletes)
	}
	if n := m.Sweep(context.Background(), 24*time.Hour); n != 0 {
		t.Fatal("purged meetings stay purged")
	}

	// Upstream failure leaves the meeting for the next sweep.
	_ = st.Save(Meeting{ID: "mtg-retry", NativeID: "q-r", State: StateEnded, EndedAt: &old, CreatedAt: old})
	q.deleteErr = &ToolError{Code: CodeUpstreamError}
	if n := m.Sweep(context.Background(), 24*time.Hour); n != 0 {
		t.Fatal("an upstream failure must not purge")
	}
	q.deleteErr = nil
	// A transcript that cannot be removed is retried, a doc failure is not fatal.
	_ = os.MkdirAll(filepath.Join(st.Dir(), "mtg-retry", transcriptFile, "x"), 0o700)
	if n := m.Sweep(context.Background(), 24*time.Hour); n != 0 {
		t.Fatal("an unremovable transcript must not purge")
	}
	_ = os.RemoveAll(filepath.Join(st.Dir(), "mtg-retry", transcriptFile))
	if n := m.Sweep(context.Background(), 24*time.Hour, "Bad Name"); n != 1 {
		t.Fatal("a document error is logged, not fatal")
	}
	_ = os.RemoveAll(st.Dir())
	if n := m.Sweep(context.Background(), 24*time.Hour); n != 0 {
		t.Fatal("an unlistable store purges nothing")
	}
}

func TestStartRetention(t *testing.T) {
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	old := time.Now().Add(-48 * time.Hour)
	_ = m.opts.Store.Save(Meeting{ID: "mtg-old", State: StateEnded, EndedAt: &old, CreatedAt: old})
	m.StartRetention(24*time.Hour, 10*time.Millisecond)
	eventually(t, "purged", stateIs(m, "mtg-old", StatePurged))
	m2 := newTestManager(t, q, 0)
	m2.StartRetention(time.Hour, 0)
	m2.Close()
}

func TestRemoveDoc(t *testing.T) {
	st, _ := NewStore(t.TempDir())
	if err := st.RemoveDoc("mtg-a", "Bad"); err == nil {
		t.Fatal("bad name")
	}
	if err := st.RemoveDoc("../x", "session.json"); err == nil {
		t.Fatal("bad id")
	}
	if err := st.RemoveDoc("mtg-a", "session.json"); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(st.Dir(), "mtg-a", "session.json", "x"), 0o700)
	if err := st.RemoveDoc("mtg-a", "session.json"); err == nil {
		t.Fatal("non-empty dir")
	}
}

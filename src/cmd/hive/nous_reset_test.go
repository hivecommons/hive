package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// The reset store wired at boot moves the selected directories aside under
// one trash stamp and drops the live reader's cache, so the panel reads an
// empty ledger immediately after a knowledge reset.
func TestWireNousKnowledgeResetStoreRoundTrip(t *testing.T) {
	layout := nousstore.New(filepath.Join(t.TempDir(), "nous"))
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	writeAt := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, t0, t0); err != nil {
			t.Fatal(err)
		}
	}
	writeAt(layout.LedgerPath(), `{"iterations":[{"iteration":0,"family":"baseline"},{"iteration":1,"family":"routing"}]}`)
	writeAt(layout.PrinciplesPath(), `{"principles":[{"id":"p1","statement":"s"}]}`)
	writeAt(filepath.Join(layout.SnapshotDir(), "1.json"), `{}`)
	writeAt(filepath.Join(layout.SnapshotDir(), "2.json"), `{}`)
	writeAt(layout.GatePendingPath(), `{"id":"g1","question":"q"}`)

	state := newNousState(layout.SnapshotDir())
	wireNousKnowledgeWith(state, layout, "", restoreTestLogger(), func() time.Time { return t0 })
	if state.ResetStore == nil || len(state.Ledger) != 2 {
		t.Fatalf("wired: store=%v ledger=%d", state.ResetStore, len(state.Ledger))
	}

	counts, err := state.ResetStore.Reset(dashboard.NousResetScope{Baseline: true, Knowledge: true, Gates: true})
	if err != nil || counts != (dashboard.NousResetCounts{Snapshots: 2, LedgerEntries: 2, Principles: 1}) {
		t.Fatalf("Reset = %+v, %v", counts, err)
	}
	if err := state.SyncKnowledge(); err != nil || len(state.Ledger) != 0 || len(state.Principles) != 0 {
		t.Fatalf("after reset: err=%v ledger=%d principles=%d", err, len(state.Ledger), len(state.Principles))
	}
	entries, err := os.ReadDir(layout.TrashDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %v, %v (want one entry for the whole reset)", entries, err)
	}
	for _, sub := range []string{nousstore.SnapshotsSubdir, nousstore.GovernorSubdir, nousstore.GatesSubdir} {
		if _, err := os.Stat(filepath.Join(layout.TrashDir(), entries[0].Name(), sub)); err != nil {
			t.Fatalf("trash missing %s: %v", sub, err)
		}
	}
	if _, ok, err := layout.LoadGatePending(); err != nil || ok {
		t.Fatalf("pending gate survived: ok=%v err=%v", ok, err)
	}

	// Baseline-only leaves knowledge alone and the reader cache intact.
	writeAt(layout.LedgerPath(), `{"iterations":[{"iteration":0,"family":"baseline"}]}`)
	if err := state.SyncKnowledge(); err != nil || len(state.Ledger) != 1 {
		t.Fatalf("re-seeded ledger: err=%v len=%d", err, len(state.Ledger))
	}
	counts, err = state.ResetStore.Reset(dashboard.NousResetScope{Baseline: true})
	if err != nil || counts != (dashboard.NousResetCounts{}) {
		t.Fatalf("Reset(baseline) on empty dir = %+v, %v", counts, err)
	}
	if err := state.SyncKnowledge(); err != nil || len(state.Ledger) != 1 {
		t.Fatalf("knowledge touched by baseline reset: err=%v len=%d", err, len(state.Ledger))
	}
}

// The dashboard mirrors nousstore's reset scope shape without importing
// the package; pin the adapter's mapping.
func TestNousResetScopeMapping(t *testing.T) {
	layout := nousstore.New(t.TempDir())
	store := nousResetStore{layout: layout, now: time.Now}
	if _, err := store.Reset(dashboard.NousResetScope{}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(layout.TrashDir()); len(entries) != 0 {
		t.Fatalf("empty scope created trash entries: %v", entries)
	}
}

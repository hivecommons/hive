package nousstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var resetT0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func seedResetTree(t *testing.T) Layout {
	t.Helper()
	l := New(t.TempDir())
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"1.json", "2.json", "3.json"} {
		write(t, filepath.Join(l.SnapshotDir(), n), `{"timestamp":"x"}`, resetT0)
	}
	write(t, l.LedgerPath(), `{"iterations":[{"iteration":0,"family":"baseline"},{"iteration":1,"family":"routing","h_main_result":"CONFIRMED"}]}`, resetT0)
	write(t, l.PrinciplesPath(), `{"principles":[{"id":"p1","statement":"one"},{"id":"p2","statement":"two"},{"id":"p3","statement":"three"}]}`, resetT0)
	write(t, l.GatePendingPath(), `{"id":"g1","question":"ok?"}`, resetT0)
	write(t, l.GateHistoryPath(), "{}\n", resetT0)
	if err := l.SavePanelConfig(PanelConfig{Mode: "suggest"}, resetT0); err != nil {
		t.Fatal(err)
	}
	return l
}

func trashEntries(t *testing.T, l Layout) []string {
	t.Helper()
	entries, err := os.ReadDir(l.TrashDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func mustCount(t *testing.T, dir string) int {
	t.Helper()
	n, err := countFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResetBaselineMovesSnapshotsToTrash(t *testing.T) {
	l := seedResetTree(t)
	n, err := l.ResetBaseline(resetT0)
	if err != nil || n != 3 {
		t.Fatalf("ResetBaseline = %d, %v; want 3, nil", n, err)
	}
	if got := mustCount(t, l.SnapshotDir()); got != 0 {
		t.Fatalf("snapshot dir holds %d files after reset", got)
	}
	// Everything else is untouched.
	if read(t, l.LedgerPath()) == "" || read(t, l.GatePendingPath()) == "" {
		t.Fatal("knowledge and gates must survive a baseline reset")
	}
	if _, ok, err := l.LoadPanelConfig(); err != nil || !ok {
		t.Fatalf("panel config must survive: ok=%v err=%v", ok, err)
	}
	trash := trashEntries(t, l)
	if len(trash) != 1 {
		t.Fatalf("trash = %v, want one entry", trash)
	}
	if got := mustCount(t, filepath.Join(l.TrashDir(), trash[0], SnapshotsSubdir)); got != 3 {
		t.Fatalf("trash holds %d snapshots, want 3", got)
	}
	// A second reset of an already-empty dir is a no-op count.
	n, err = l.ResetBaseline(resetT0.Add(time.Second))
	if err != nil || n != 0 {
		t.Fatalf("second ResetBaseline = %d, %v", n, err)
	}
	// Two resets at the same stamp never overwrite each other in the trash.
	at := resetT0.Add(2 * time.Second)
	write(t, filepath.Join(l.SnapshotDir(), "a.json"), "{}", at)
	if _, err := l.ResetBaseline(at); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(l.SnapshotDir(), "b.json"), "{}", at)
	if _, err := l.ResetBaseline(at); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(l.TrashDir(), at.UTC().Format(trashStampLayout))
	if read(t, filepath.Join(entry, SnapshotsSubdir, "a.json")) != "{}" || read(t, filepath.Join(entry, SnapshotsSubdir+".1", "b.json")) != "{}" {
		t.Fatal("same-stamp resets must keep both trash copies")
	}
}

func TestResetKnowledgeTruncatesToEmptyDocuments(t *testing.T) {
	l := seedResetTree(t)
	reader := l.KnowledgeReader("")
	if k, _, err := reader.Load(); err != nil || len(k.Ledger) != 2 || len(k.Principles) != 3 {
		t.Fatalf("pre-reset load = %d/%d, %v", len(k.Ledger), len(k.Principles), err)
	}

	ledger, principles, err := l.ResetKnowledge(resetT0)
	if err != nil || ledger != 2 || principles != 3 {
		t.Fatalf("ResetKnowledge = %d, %d, %v; want 2, 3, nil", ledger, principles, err)
	}
	if got := read(t, l.LedgerPath()); got != string(emptyLedger) {
		t.Fatalf("ledger after reset = %q", got)
	}
	if got := read(t, l.PrinciplesPath()); got != string(emptyPrinciples) {
		t.Fatalf("principles after reset = %q", got)
	}
	// The durable dir still wins over the legacy fallback because it holds
	// (empty) files rather than nothing.
	legacy := t.TempDir()
	write(t, filepath.Join(legacy, LedgerFile), nousLedgerTemplate, resetT0)
	if dir, fallback := l.ResolveGovernorDir(legacy); fallback || dir != l.GovernorDir() {
		t.Fatalf("ResolveGovernorDir = %q fallback=%v after reset", dir, fallback)
	}
	// Snapshots, gates and panel config are untouched.
	if got := mustCount(t, l.SnapshotDir()); got != 3 {
		t.Fatalf("snapshots after knowledge reset = %d", got)
	}
	if read(t, l.GatePendingPath()) == "" {
		t.Fatal("gates must survive a knowledge reset")
	}

	// Invalidate forces a re-read even if the stat signature matched.
	reader.Invalidate()
	k, changed, err := reader.Load()
	if err != nil || !changed || len(k.Ledger) != 0 || len(k.Principles) != 0 {
		t.Fatalf("post-reset load = %d/%d changed=%v err=%v", len(k.Ledger), len(k.Principles), changed, err)
	}
}

func TestResetKnowledgeCountsIgnoreUnparseableFiles(t *testing.T) {
	l := seedResetTree(t)
	write(t, l.LedgerPath(), "{not json", resetT0)
	ledger, principles, err := l.ResetKnowledge(resetT0)
	if err != nil || ledger != 0 || principles != 3 {
		t.Fatalf("ResetKnowledge = %d, %d, %v", ledger, principles, err)
	}
	trash := trashEntries(t, l)
	if len(trash) != 1 {
		t.Fatalf("trash = %v", trash)
	}
	if got := read(t, filepath.Join(l.TrashDir(), trash[0], GovernorSubdir, LedgerFile)); got != "{not json" {
		t.Fatalf("corrupt ledger must be preserved in the trash, got %q", got)
	}
}

func TestResetGatesClearsProtocolFiles(t *testing.T) {
	l := seedResetTree(t)
	if err := l.ResetGates(resetT0); err != nil {
		t.Fatal(err)
	}
	if got := mustCount(t, l.GatesDir()); got != 0 {
		t.Fatalf("gates dir holds %d files after reset", got)
	}
	if _, ok, err := l.LoadGatePending(); err != nil || ok {
		t.Fatalf("pending gate after reset: ok=%v err=%v", ok, err)
	}
	if got := mustCount(t, l.SnapshotDir()); got != 3 {
		t.Fatalf("snapshots after gates reset = %d", got)
	}
}

func TestResetMissingDirsIsNoop(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "fresh"))
	n, err := l.ResetBaseline(resetT0)
	if err != nil || n != 0 {
		t.Fatalf("ResetBaseline on missing root = %d, %v", n, err)
	}
	ledger, principles, err := l.ResetKnowledge(resetT0)
	if err != nil || ledger != 0 || principles != 0 {
		t.Fatalf("ResetKnowledge on missing root = %d, %d, %v", ledger, principles, err)
	}
	if err := l.ResetGates(resetT0); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{l.SnapshotDir(), l.GovernorDir(), l.GatesDir()} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s must exist after reset: %v", dir, err)
		}
	}
	if got := read(t, l.LedgerPath()); got != string(emptyLedger) {
		t.Fatalf("ledger = %q", got)
	}
}

func TestResetTrashKeepsOnlyNewestEntry(t *testing.T) {
	l := seedResetTree(t)
	stamps := []time.Time{resetT0, resetT0.Add(time.Minute), resetT0.Add(2 * time.Minute)}
	for _, at := range stamps {
		write(t, filepath.Join(l.SnapshotDir(), "s.json"), "{}", at)
		if _, err := l.ResetBaseline(at); err != nil {
			t.Fatal(err)
		}
	}
	trash := trashEntries(t, l)
	if len(trash) != trashKeep {
		t.Fatalf("trash = %v, want %d entry", trash, trashKeep)
	}
	if want := stamps[2].UTC().Format(trashStampLayout); trash[0] != want {
		t.Fatalf("trash keeps %q, want newest %q", trash[0], want)
	}
	// One reset of several scopes shares one entry.
	at := resetT0.Add(3 * time.Minute)
	write(t, filepath.Join(l.SnapshotDir(), "s.json"), "{}", at)
	res, err := l.Reset(ResetScope{Baseline: true, Knowledge: true, Gates: true}, at)
	if err != nil || res.Snapshots != 1 || res.LedgerEntries != 2 || res.Principles != 3 {
		t.Fatalf("Reset(all) = %+v, %v", res, err)
	}
	trash = trashEntries(t, l)
	if len(trash) != 1 {
		t.Fatalf("trash after all-scope reset = %v", trash)
	}
	for _, sub := range []string{SnapshotsSubdir, GovernorSubdir, GatesSubdir} {
		if _, err := os.Stat(filepath.Join(l.TrashDir(), trash[0], sub)); err != nil {
			t.Fatalf("trash entry missing %s: %v", sub, err)
		}
	}
}

func TestResetScopeSelectsDirs(t *testing.T) {
	l := seedResetTree(t)
	res, err := l.Reset(ResetScope{Knowledge: true}, resetT0)
	if err != nil || res.Snapshots != 0 || res.LedgerEntries != 2 || res.Principles != 3 {
		t.Fatalf("Reset(knowledge) = %+v, %v", res, err)
	}
	if got := mustCount(t, l.SnapshotDir()); got != 3 {
		t.Fatalf("snapshots touched: %d", got)
	}
	res, err = l.Reset(ResetScope{}, resetT0)
	if err != nil || res != (ResetResult{}) {
		t.Fatalf("Reset(nothing) = %+v, %v", res, err)
	}
	if got := trashEntries(t, l); len(got) != 1 {
		t.Fatalf("empty scope must not create trash entries: %v", got)
	}
}

func TestResetRenameFailureLeavesLiveDirIntact(t *testing.T) {
	l := seedResetTree(t)
	orig := rename
	rename = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { rename = orig })

	n, err := l.ResetBaseline(resetT0)
	if err == nil || !strings.Contains(err.Error(), "injected rename failure") || n != 0 {
		t.Fatalf("ResetBaseline = %d, %v; want injected failure", n, err)
	}
	if got := mustCount(t, l.SnapshotDir()); got != 3 {
		t.Fatalf("live snapshots after failed rename = %d, want 3 (nothing half-deleted)", got)
	}
	if _, _, err := l.ResetKnowledge(resetT0); err == nil {
		t.Fatal("ResetKnowledge must surface the rename failure")
	}
	if read(t, l.LedgerPath()) == string(emptyLedger) {
		t.Fatal("ledger must not be truncated when the move failed")
	}
	if err := l.ResetGates(resetT0); err == nil {
		t.Fatal("ResetGates must surface the rename failure")
	}
	for _, scope := range []ResetScope{{Baseline: true}, {Knowledge: true}, {Gates: true}} {
		if _, err := l.Reset(scope, resetT0); err == nil {
			t.Fatalf("Reset(%+v) must surface the rename failure", scope)
		}
	}
}

func TestResetDirErrors(t *testing.T) {
	// A file where the trash dir should be: MkdirAll fails.
	l := New(t.TempDir())
	write(t, l.TrashDir(), "x", resetT0)
	if _, err := l.ResetBaseline(resetT0); err == nil || !strings.Contains(err.Error(), "trash") {
		t.Fatalf("ResetBaseline with file as trash = %v", err)
	}

	// A file where the snapshot dir should be: stat succeeds, rename
	// succeeds, and the dir is recreated.
	l2 := New(t.TempDir())
	write(t, l2.SnapshotDir(), "not a dir", resetT0)
	if _, err := l2.ResetBaseline(resetT0); err == nil || !strings.Contains(err.Error(), "list") {
		t.Fatalf("ResetBaseline with file as snapshots = %v", err)
	}

	// Recreating the live dir fails when its parent is a file.
	l3 := New(filepath.Join(t.TempDir(), "root"))
	if err := os.MkdirAll(l3.TrashDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, l3.GovernorDir(), "file", resetT0)
	// GovernorDir is a file: stat ok, rename moves the file, then MkdirAll
	// recreates a dir there — success path for a stray file.
	if _, _, err := l3.ResetKnowledge(resetT0); err != nil {
		t.Fatalf("ResetKnowledge over a stray file = %v", err)
	}
	if read(t, l3.LedgerPath()) != string(emptyLedger) {
		t.Fatal("ledger must be recreated after the stray file is moved aside")
	}
}

func TestPruneTrashReportsRemoveFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	l := New(t.TempDir())
	old := filepath.Join(l.TrashDir(), "20200101T000000.000000000Z")
	if err := os.MkdirAll(filepath.Join(old, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(old, "sub", "f"), "x", resetT0)
	if err := os.Chmod(filepath.Join(old, "sub"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(old, "sub"), 0o700) })
	if _, err := l.ResetBaseline(resetT0); err == nil || !strings.Contains(err.Error(), "prune") {
		t.Fatalf("ResetBaseline with unremovable old trash = %v", err)
	}
	// The live tree was still reset before the prune failed.
	if info, err := os.Stat(l.SnapshotDir()); err != nil || !info.IsDir() {
		t.Fatalf("snapshot dir after prune failure: %v", err)
	}
	if entries := trashEntries(t, l); len(entries) != 2 {
		t.Fatalf("trash = %v", entries)
	}
}

func TestResetKnowledgeSurfacesWriteFailures(t *testing.T) {
	orig := rename
	t.Cleanup(func() { rename = orig })
	for _, file := range []string{LedgerFile, PrinciplesFile} {
		l := seedResetTree(t)
		rename = func(from, to string) error {
			if filepath.Base(to) == file {
				return errors.New("injected write failure")
			}
			return os.Rename(from, to)
		}
		if _, _, err := l.ResetKnowledge(resetT0); err == nil || !strings.Contains(err.Error(), "injected write failure") {
			t.Fatalf("%s: ResetKnowledge = %v", file, err)
		}
	}
}

func TestResetDirStatAndRecreateFailures(t *testing.T) {
	l := New(t.TempDir())
	write(t, filepath.Join(l.Root, "blocker"), "x", resetT0)
	// A path component that is a file: stat fails with something other
	// than not-exist.
	if err := l.resetDir(filepath.Join(l.Root, "blocker", "x"), "x", resetT0); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("resetDir through a file = %v", err)
	}

	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	// The move succeeds, then the parent is made read-only so the empty
	// dir cannot be recreated.
	parent := filepath.Join(l.Root, "parent")
	dir := filepath.Join(parent, "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := rename
	rename = func(from, to string) error {
		if err := os.Rename(from, to); err != nil {
			return err
		}
		return os.Chmod(parent, 0o500)
	}
	t.Cleanup(func() { rename = orig; _ = os.Chmod(parent, 0o755) })
	if err := l.resetDir(dir, "sub", resetT0); err == nil || !strings.Contains(err.Error(), "recreate") {
		t.Fatalf("resetDir with read-only parent = %v", err)
	}
}

func TestPruneTrashMissingDir(t *testing.T) {
	l := New(t.TempDir())
	if err := l.pruneTrash("x"); err == nil {
		t.Fatal("pruneTrash without a trash dir must fail")
	}
}

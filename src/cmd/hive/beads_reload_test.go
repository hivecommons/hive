package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
)

// TestBeadReloadWarner_DedupesIdenticalFailures pins the dedupe contract: the
// first failure per agent warns, an identical repeat does not, a changed error
// text warns again, and clear() (a successful reload) re-arms the WARN.
func TestBeadReloadWarner_DedupesIdenticalFailures(t *testing.T) {
	w := &beadReloadWarner{}

	if !w.shouldWarn("supervisor", "permission denied") {
		t.Error("first failure should warn")
	}
	if w.shouldWarn("supervisor", "permission denied") {
		t.Error("identical repeat should not warn")
	}
	if !w.shouldWarn("supervisor", "no such file") {
		t.Error("changed error text should warn again")
	}
	if !w.shouldWarn("scanner", "permission denied") {
		t.Error("a different agent is an independent key")
	}

	w.clear("supervisor")
	if !w.shouldWarn("supervisor", "no such file") {
		t.Error("after clear, the next failure should warn again")
	}
}

// TestReloadBeadStores_WarnsOncePerDistinctError drives the real reload loop
// against a store whose beads.json read fails persistently (it is a
// directory), and asserts the retry-spam fix: cycle one logs WARN, cycle two
// logs the identical failure at DEBUG only. This is the hub-side half of
// #5505 — one actionable line, not one per eval cycle.
func TestReloadBeadStores_WarnsOncePerDistinctError(t *testing.T) {
	beadReloadWarns = &beadReloadWarner{} // isolate from other tests in this package
	dir := filepath.Join(t.TempDir(), "supervisor")
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Make every subsequent load fail the same way: beads.json as a directory.
	if err := os.Mkdir(filepath.Join(dir, "beads.json"), 0o755); err != nil {
		t.Fatalf("mkdir beads.json: %v", err)
	}
	stores := map[string]*beads.Store{"supervisor": store}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reloadBeadStores(stores, logger)
	first := buf.String()
	if !strings.Contains(first, "failed to reload beads from disk") {
		t.Fatalf("first cycle should WARN, got log: %q", first)
	}

	buf.Reset()
	reloadBeadStores(stores, logger)
	if repeat := buf.String(); strings.Contains(repeat, "failed to reload beads from disk") {
		t.Errorf("identical repeat should be demoted below WARN, got log: %q", repeat)
	}
}

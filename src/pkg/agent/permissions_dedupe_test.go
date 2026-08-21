package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

// These tests pin the #4488 fix: an entry the watcher cannot repair (EPERM
// chown after the privilege drop — the root-owned /data/agents/reviewer seed)
// must warn ONCE at WARN, not once per 10-second tick forever, with identical
// repeats demoted to DEBUG. The warning clears when the entry's ownership
// state changes and re-fires on a new failure.

// recordingHandler captures every slog record so tests can count exactly how
// many times a given (level, message) pair was emitted.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// count returns how many captured records match level and message and carry
// a "path" attr equal to path (any path when path is empty).
func (h *recordingHandler) count(level slog.Level, msg, path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level != level || r.Message != msg {
			continue
		}
		if path == "" {
			n++
			continue
		}
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "path" && a.Value.String() == path {
				n++
				return false
			}
			return true
		})
	}
	return n
}

// fakeOwnerInfo wraps a real FileInfo but reports a caller-chosen uid/gid,
// letting a test drive fixEntry's ownership decisions without privileges.
type fakeOwnerInfo struct {
	os.FileInfo
	sys *syscall.Stat_t
}

func (f fakeOwnerInfo) Sys() any { return f.sys }

// healthyAgentInfo returns a FileInfo for path that looks like a correctly
// seeded agent entry: owned by a per-agent uid (not root, not dev) with the
// shared node gid — the state every mapped agent dir is in, which fixEntry
// must leave alone without logging.
func healthyAgentInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return fakeOwnerInfo{FileInfo: fi, sys: &syscall.Stat_t{Uid: 2001, Gid: uint32(NodeGID)}}
}

// unfixableEntry creates a regular file whose real ownership makes fixEntry
// attempt a chown this unprivileged test process cannot perform (gid is not
// NodeGID, and we may not give the file to group NodeGID). It returns the
// path and its Lstat info, or skips when the environment can actually chown
// (root, or a member of gid 1000), where the failure arm is unreachable.
func unfixableEntry(t *testing.T, dir string) (string, os.FileInfo) {
	t.Helper()
	path := filepath.Join(dir, "stats.json")
	// FilePerms-satisfying mode so only the chown arm fires, matching the
	// reviewer seed once its mode is fine but ownership is not.
	if err := os.WriteFile(path, []byte("{}"), 0o664); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no syscall.Stat_t on this platform")
	}
	if stat.Uid == 0 || stat.Gid == uint32(NodeGID) {
		t.Skip("entry would not need a chown in this environment")
	}
	// Probe: if this process can actually chown to NodeGID, the failure arm
	// this test exists to exercise is unreachable here.
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	if os.Chown(probe, int(stat.Uid), NodeGID) == nil {
		t.Skip("process can chown to NodeGID; unfixable-entry arm unreachable")
	}
	return path, fi
}

// TestFixEntryUnfixableChownWarnsOnce is the core #4488 regression test: the
// same unfixable entry seen on successive ticks produces exactly one WARN,
// with the repeats demoted to DEBUG.
func TestFixEntryUnfixableChownWarnsOnce(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)

	path, fi := unfixableEntry(t, t.TempDir())
	h := &recordingHandler{}
	logger := slog.New(h)

	const ticks = 3
	for i := 0; i < ticks; i++ {
		fixEntry(path, fi, logger)
	}

	if got := h.count(slog.LevelWarn, "permissions watcher: chown failed", path); got != 1 {
		t.Errorf("WARN chown failed logged %d times across %d ticks, want exactly 1", got, ticks)
	}
	if got := h.count(slog.LevelDebug, "permissions watcher: chown failed", path); got != ticks-1 {
		t.Errorf("DEBUG chown failed logged %d times, want %d", got, ticks-1)
	}
}

// TestFixEntryDedupesPerPath verifies suppression is scoped to the failing
// path: a second unfixable entry still gets its own first WARN even after the
// first entry's warning was suppressed.
func TestFixEntryDedupesPerPath(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)

	pathA, fiA := unfixableEntry(t, t.TempDir())
	pathB, fiB := unfixableEntry(t, t.TempDir())
	h := &recordingHandler{}
	logger := slog.New(h)

	fixEntry(pathA, fiA, logger)
	fixEntry(pathA, fiA, logger) // suppressed repeat for A
	fixEntry(pathB, fiB, logger) // first sighting of B — must still WARN

	if got := h.count(slog.LevelWarn, "permissions watcher: chown failed", pathA); got != 1 {
		t.Errorf("WARN for path A logged %d times, want 1", got)
	}
	if got := h.count(slog.LevelWarn, "permissions watcher: chown failed", pathB); got != 1 {
		t.Errorf("WARN for path B logged %d times, want 1", got)
	}
}

// TestFixEntryHealthyMappedEntryIsSilent pins that a correctly owned agent
// entry (per-agent uid, node gid — every mapped agent dir) produces no log
// output at all: the watcher must only ever be loud about real findings.
func TestFixEntryHealthyMappedEntryIsSilent(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)

	dir := t.TempDir()
	path := filepath.Join(dir, "beads.json")
	if err := os.WriteFile(path, []byte("{}"), 0o664); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	h := &recordingHandler{}
	fixEntry(path, healthyAgentInfo(t, path), slog.New(h))

	h.mu.Lock()
	n := len(h.records)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("healthy mapped entry produced %d log records, want 0", n)
	}
}

// TestFixEntryWarnRefiresAfterOwnershipChanges verifies the dedupe entry is
// CLEARED when the path stops needing a chown (ownership fixed externally,
// e.g. the entrypoint's root-phase repair), so a later regression on the same
// path is a fresh finding that warns at WARN again — not a suppressed repeat.
func TestFixEntryWarnRefiresAfterOwnershipChanges(t *testing.T) {
	resetPermWarnDedupe()
	t.Cleanup(resetPermWarnDedupe)

	path, fi := unfixableEntry(t, t.TempDir())
	h := &recordingHandler{}
	logger := slog.New(h)

	fixEntry(path, fi, logger) // first failure: WARN
	fixEntry(path, fi, logger) // identical repeat: DEBUG

	// Ownership becomes correct (as the fixed entrypoint now guarantees for
	// seeded dirs): the no-chown-needed pass must clear the recorded failure.
	fixEntry(path, healthyAgentInfo(t, path), logger)

	fixEntry(path, fi, logger) // regression: must WARN again

	if got := h.count(slog.LevelWarn, "permissions watcher: chown failed", path); got != 2 {
		t.Errorf("WARN chown failed logged %d times, want 2 (before and after the ownership state changed)", got)
	}
}

// TestWarnDeduperErrorChangeRewarns pins that a DIFFERENT error on the same
// key is new information and is not suppressed.
func TestWarnDeduperErrorChangeRewarns(t *testing.T) {
	d := &warnDeduper{}
	key := dedupeKey("chown", "/data/agents/reviewer")
	if !d.shouldWarn(key, "operation not permitted") {
		t.Error("first failure should warn")
	}
	if d.shouldWarn(key, "operation not permitted") {
		t.Error("identical repeat should be suppressed")
	}
	if !d.shouldWarn(key, "read-only file system") {
		t.Error("changed error text should warn again")
	}
	d.clear(key)
	if !d.shouldWarn(key, "read-only file system") {
		t.Error("failure after clear should warn again")
	}
}

// TestWarnDeduperBoundsMemory pins the map cap: exceeding it resets state
// rather than growing without bound on a long-lived pod.
func TestWarnDeduperBoundsMemory(t *testing.T) {
	d := &warnDeduper{}
	for i := 0; i < maxDedupedWarnKeys; i++ {
		d.shouldWarn(dedupeKey("chown", string(rune(i))+"/p"), "e")
	}
	// The next insert crosses the cap and must reset, so a previously seen
	// key warns again instead of the map growing forever.
	if !d.shouldWarn(dedupeKey("chown", "overflow"), "e") {
		t.Error("insert at cap should warn")
	}
	if !d.shouldWarn(dedupeKey("chown", string(rune(0))+"/p"), "e") {
		t.Error("after cap reset, an old key should warn again")
	}
}

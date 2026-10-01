package sessionprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkTree writes a directory containing a single file and stamps BOTH the file
// and the directory to the given mtime, so newestModTime sees a deterministic
// age regardless of walk order. It mirrors mkSession in prune_test.go.
func mkTree(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	f := filepath.Join(path, "data")
	if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", f, err)
	}
	if err := os.Chtimes(f, mod, mod); err != nil {
		t.Fatalf("chtimes file: %v", err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes dir: %v", err)
	}
}

func TestPruneAgentHomesRemovesAgedPreSharedAndCaches(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	home := filepath.Join(root, "scanner")

	agedSnap := filepath.Join(home, preSharedPrefix+"20260801")
	agedCache := filepath.Join(home, ".cache")
	agedNpm := filepath.Join(home, ".npm", "_cacache")
	freshSnap := filepath.Join(home, preSharedPrefix+"20260914")
	freshCopilotCache := filepath.Join(home, ".copilot", "cache")

	mkTree(t, agedSnap, old)
	mkTree(t, agedCache, old)
	mkTree(t, agedNpm, old)
	mkTree(t, freshSnap, recent)
	mkTree(t, freshCopilotCache, recent)

	// Credentials and session-state under the same home must never be touched,
	// even when aged.
	creds := filepath.Join(home, ".claude")
	session := filepath.Join(home, ".copilot", "session-state")
	mkTree(t, creds, old)
	mkTree(t, session, old)

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}

	for _, gone := range []string{agedSnap, agedCache, agedNpm} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("aged target should have been removed: %s (stat err = %v)", gone, err)
		}
	}
	for _, kept := range []string{freshSnap, freshCopilotCache, creds, session} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("target should have survived: %s: %v", kept, err)
		}
	}
	if res.Removed != 3 || res.Failed != 0 {
		t.Errorf("got %+v, want Removed=3 Failed=0", res)
	}
}

// A cache whose directory mtime is stale but which holds a recently written
// file must survive: an agent actively populating ~/.cache should not have it
// reclaimed mid-run.
func TestPruneAgentHomesKeepsCacheWithRecentFile(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	staleDir := now.Add(-30 * 24 * time.Hour)
	recentWrite := now.Add(-2 * time.Minute)

	cache := filepath.Join(root, "quality", ".cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	hot := filepath.Join(cache, "hot")
	if err := os.WriteFile(hot, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hot, recentWrite, recentWrite); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cache, staleDir, staleDir); err != nil {
		t.Fatal(err)
	}

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("cache with a recent write was deleted: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0", res.Removed)
	}
}

// A symlinked cache (the interactive-home bridge to the shared /data/home/.cache)
// must never be followed or unlinked by the sweep.
func TestPruneAgentHomesNeverFollowsSymlinkedCache(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	shared := filepath.Join(root, "shared-cache")
	mkTree(t, shared, old)

	home := filepath.Join(root, "guide")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".cache")
	if err := os.Symlink(shared, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("symlinked cache bridge was removed: %v", err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Errorf("shared cache target was removed through the bridge: %v", err)
	}
	if res.Removed != 0 || res.Scanned != 0 {
		t.Errorf("got %+v, want zero — a symlink must not be scanned or removed", res)
	}
}

func TestPruneAgentHomesIgnoresUnknownEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	home := filepath.Join(root, "sec-check")
	// A repo checkout and a beads dir, both aged, neither on the allow-list.
	repo := filepath.Join(home, "api-server")
	beads := filepath.Join(home, ".beads")
	mkTree(t, repo, old)
	mkTree(t, beads, old)

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Stat(repo); err != nil {
		t.Errorf("repo checkout was removed: %v", err)
	}
	if _, err := os.Stat(beads); err != nil {
		t.Errorf("beads dir was removed: %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want Scanned=0 Removed=0", res)
	}
}

func TestPruneAgentHomesMissingRootIsNotAnError(t *testing.T) {
	res, err := PruneAgentHomes(filepath.Join(t.TempDir(), "nope"), 14*24*time.Hour, time.Now(), quietLogger())
	if err != nil {
		t.Fatalf("missing root should not error, got %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want zero result", res)
	}
}

func TestPruneAgentHomesDisabledWhenMaxAgeNonPositive(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-365 * 24 * time.Hour)
	snap := filepath.Join(root, "a", preSharedPrefix+"20250101")
	mkTree(t, snap, old)

	for _, maxAge := range []time.Duration{0, -time.Hour} {
		res, err := PruneAgentHomes(root, maxAge, now, quietLogger())
		if err != nil {
			t.Fatalf("PruneAgentHomes(maxAge=%v): %v", maxAge, err)
		}
		if res.Removed != 0 {
			t.Errorf("maxAge=%v removed %d, want 0", maxAge, res.Removed)
		}
		if _, err := os.Stat(snap); err != nil {
			t.Fatalf("maxAge=%v deleted a snapshot while disabled: %v", maxAge, err)
		}
	}
}

// Boundary: a target exactly at the cutoff is kept, matching Prune's
// strictly-older rule.
func TestPruneAgentHomesKeepsTargetExactlyAtCutoff(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	maxAge := 14 * 24 * time.Hour
	exact := now.Add(-maxAge)

	snap := filepath.Join(root, "b", preSharedPrefix+"20260901")
	mkTree(t, snap, exact)

	res, err := PruneAgentHomes(root, maxAge, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Stat(snap); err != nil {
		t.Errorf("target exactly at cutoff was removed: %v", err)
	}
	if res.Removed != 0 {
		t.Errorf("Removed = %d, want 0", res.Removed)
	}
}

func TestPruneAgentHomesReturnsReadDirErrors(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "regular-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := PruneAgentHomes(notADir, 14*24*time.Hour, time.Now(), quietLogger())
	if err == nil {
		t.Fatal("PruneAgentHomes on a non-directory returned nil error; want the ReadDir failure surfaced")
	}
	if res.Removed != 0 || res.Scanned != 0 {
		t.Errorf("PruneAgentHomes on error = %+v, want zero-valued Result", res)
	}
}

// A loose file or symlink directly under root is not an agent home and must be
// skipped without being descended into.
func TestPruneAgentHomesSkipsNonDirectoryRootEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	loose := filepath.Join(root, "README")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlinked "home" pointing at a directory outside root with an aged
	// cache: ReadDir reports the link as a non-directory, so the sweep must
	// not reach the cache through it.
	outside := filepath.Join(t.TempDir(), "real")
	mkTree(t, filepath.Join(outside, ".cache"), old)
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Stat(loose); err != nil {
		t.Errorf("loose file under root was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".cache")); err != nil {
		t.Errorf("aged cache was swept through a symlinked home: %v", err)
	}
	if res.Scanned != 0 || res.Removed != 0 {
		t.Errorf("got %+v, want zero — non-directory root entries are not homes", res)
	}
}

// A regular file occupying an allow-listed cache path is not a cache directory
// and must be left alone; a regular file where a cache's parent should be
// (.npm as a file) makes Lstat fail with ENOTDIR, which must also keep.
func TestPruneAgentHomesKeepsNonDirectoryTargets(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	home := filepath.Join(root, "docs")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(home, ".cache")
	npmFile := filepath.Join(home, ".npm")
	for _, f := range []string{cacheFile, npmFile} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	for _, f := range []string{cacheFile, npmFile} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("regular file at a cache path was removed: %s: %v", f, err)
		}
	}
	if res.Scanned != 0 || res.Removed != 0 || res.Failed != 0 {
		t.Errorf("got %+v, want zero", res)
	}
}

// An agent home that cannot be read is skipped, and the sweep continues across
// the remaining homes.
func TestPruneAgentHomesSkipsUnreadableHome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	locked := filepath.Join(root, "locked")
	mkTree(t, filepath.Join(locked, ".cache"), old)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	otherCache := filepath.Join(root, "open", ".cache")
	mkTree(t, otherCache, old)

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Stat(otherCache); !os.IsNotExist(err) {
		t.Errorf("sweep did not continue past the unreadable home: %v", err)
	}
	if res.Removed != 1 || res.Failed != 0 {
		t.Errorf("got %+v, want Removed=1 Failed=0", res)
	}
}

// A target whose contents cannot be walked is kept: deleting on an unreadable
// path is how a transient failure becomes data loss.
func TestPruneAgentHomesKeepsTargetWhenWalkFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny access")
	}
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	cache := filepath.Join(root, "walker", ".cache")
	mkTree(t, cache, old)
	if err := os.Chmod(cache, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cache, 0o755) })

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if _, err := os.Lstat(cache); err != nil {
		t.Errorf("unwalkable cache was removed: %v", err)
	}
	if res.Scanned != 1 || res.Removed != 0 || res.Failed != 0 {
		t.Errorf("got %+v, want Scanned=1 Removed=0 Failed=0", res)
	}
}

// A removal failure is counted in Failed and does not abort the sweep.
func TestPruneAgentHomesCountsRemoveFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny removal")
	}
	root := t.TempDir()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)

	home := filepath.Join(root, "pinned")
	cache := filepath.Join(home, ".cache")
	mkTree(t, cache, old)
	// A read-only home permits reading and walking .cache but forbids
	// unlinking it.
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	res, err := PruneAgentHomes(root, 14*24*time.Hour, now, quietLogger())
	if err != nil {
		t.Fatalf("PruneAgentHomes: %v", err)
	}
	if res.Scanned != 1 || res.Removed != 0 || res.Failed != 1 {
		t.Errorf("got %+v, want Scanned=1 Removed=0 Failed=1", res)
	}
}

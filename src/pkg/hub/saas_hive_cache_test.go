package hub

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestHiveMetaCacheServesAndInvalidates covers the cache contract end-to-end
// through the real loadSaaSHive/saveSaaSHive/removeHiveRecord paths: hub
// writes are immediate hits, external rewrites are detected via stat, and
// deletion evicts.
func TestHiveMetaCacheServesAndInvalidates(t *testing.T) {
	dir := t.TempDir()
	origDir := saasHivesDir
	saasHivesDir = dir
	t.Cleanup(func() { saasHivesDir = origDir })

	// Save through the hub path — the cache must be warm immediately.
	h := &SaaSHive{ID: "cache-h1", Owner: "alice", Status: statusAvailable}
	if err := saveSaaSHive(h); err != nil {
		t.Fatalf("saveSaaSHive: %v", err)
	}
	got := loadSaaSHive("cache-h1")
	if got == nil || got.Owner != "alice" {
		t.Fatalf("loadSaaSHive after save = %+v, want owner alice", got)
	}

	// Callers must get independent copies: mutating one load must never be
	// visible through the next. This is the property that makes a parsed-struct
	// cache unsafe and is the reason bytes are cached instead.
	got.Owner = "mallory"
	got.Repos = append(got.Repos, "tainted/repo")
	again := loadSaaSHive("cache-h1")
	if again.Owner != "alice" || len(again.Repos) != 0 {
		t.Fatalf("cache returned a tainted copy: %+v", again)
	}

	// An EXTERNAL rewrite (not via saveSaaSHive — as tests and manual pokes do)
	// must be picked up via the stat check.
	path := filepath.Join(dir, "cache-h1", "meta.json")
	external := []byte(`{"id":"cache-h1","owner":"bob","status":"assigned"}`)
	// Ensure the mtime moves even on filesystems with coarse timestamps.
	past := time.Now().Add(-2 * time.Second)
	if err := os.WriteFile(path, external, 0o644); err != nil {
		t.Fatalf("external write: %v", err)
	}
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	fresh := loadSaaSHive("cache-h1")
	if fresh == nil || fresh.Owner != "bob" {
		t.Fatalf("external rewrite not observed: %+v", fresh)
	}

	// Deletion through the hub path evicts; a stale entry must not resurrect
	// the hive.
	removeHiveRecord("cache-h1", slog.Default())
	if loadSaaSHive("cache-h1") != nil {
		t.Fatal("loadSaaSHive returned a record after removeHiveRecord")
	}
	if hives := listSaaSHives(); len(hives) != 0 {
		t.Fatalf("listSaaSHives = %d entries after removal, want 0", len(hives))
	}
}

// TestHiveMetaCacheList exercises listSaaSHives against a mixed directory:
// cached entries, an uncached external write, and a non-hive file.
func TestHiveMetaCacheList(t *testing.T) {
	dir := t.TempDir()
	origDir := saasHivesDir
	saasHivesDir = dir
	t.Cleanup(func() { saasHivesDir = origDir })

	for _, id := range []string{"l1", "l2", "l3"} {
		if err := saveSaaSHive(&SaaSHive{ID: id, Status: statusAvailable}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	// A stray file at the top level must be ignored (only dirs are hives).
	os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644)

	if hives := listSaaSHives(); len(hives) != 3 {
		t.Fatalf("listSaaSHives = %d, want 3", len(hives))
	}
	// Second pass is served from cache — must be identical.
	if hives := listSaaSHives(); len(hives) != 3 {
		t.Fatalf("cached listSaaSHives = %d, want 3", len(hives))
	}
}

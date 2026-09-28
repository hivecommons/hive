package mention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #9166: a torn store file (pod killed mid-save) must not disable the mention
// feature at boot. The corrupt bytes are kept for forensics and the store
// starts empty and usable.
func TestStoreRecoversFromTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "github-mention-triggers.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mark("org/repo", "node-1", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	torn := full[:len(full)/2]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on torn file: %v", err)
	}
	if loaded.Seen("node-1") || !loaded.Watermark("org/repo").IsZero() {
		t.Fatalf("recovered store kept torn state: seen=%v watermark=%v", loaded.Seen("node-1"), loaded.Watermark("org/repo"))
	}
	backups, _ := filepath.Glob(path + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatalf("want one corrupt backup, got %v", backups)
	}
	kept, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != string(torn) {
		t.Fatalf("backup = %q, want the torn bytes %q", kept, torn)
	}

	// The recovered store persists again, and what it writes loads cleanly.
	if err := loaded.Mark("org/repo", "node-2", time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	again, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Seen("node-2") {
		t.Fatal("mark after recovery was not persisted")
	}
}

// #9166: saves must replace the file via rename, never truncate it in place,
// so a crash mid-save leaves the previous complete document.
func TestStoreSaveReplacesFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mark("org/repo", "first", time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A hard link pins the old inode: an in-place truncate+write mutates it,
	// a temp-file rename leaves it untouched.
	snapshot := filepath.Join(dir, "snapshot.json")
	if err := os.Link(path, snapshot); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := s.Mark("org/repo", "second", time.Now()); err != nil {
		t.Fatal(err)
	}
	pinned, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(pinned) != string(before) {
		t.Fatalf("store file was rewritten in place:\nbefore=%s\nafter=%s", before, pinned)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".store.json") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// #9166: the seen set is bounded by seenTTL; keys younger than the TTL keep
// deduping, older ones are dropped on the next mutation and on load.
func TestStorePrunesSeenAfterTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.mu.Lock()
	s.state.Seen["expired"] = now.Add(-seenTTL - time.Hour)
	s.state.Seen["inside-ttl"] = now.Add(-seenTTL + time.Hour)
	s.mu.Unlock()

	if err := s.Mark("org/repo", "fresh", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if s.Seen("expired") {
		t.Fatal("key older than seenTTL survived a mutation")
	}
	if !s.Seen("inside-ttl") || !s.Seen("fresh") {
		t.Fatalf("live keys pruned: inside-ttl=%v fresh=%v", s.Seen("inside-ttl"), s.Seen("fresh"))
	}

	// Load-time pruning: an on-disk key past the TTL is dropped and the file
	// is rewritten without it.
	raw := map[string]any{
		"watermarks": map[string]time.Time{},
		"seen_at":    map[string]time.Time{"stale": now.Add(-seenTTL - time.Hour), "live": now},
	}
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Seen("stale") || !loaded.Seen("live") {
		t.Fatalf("load prune: stale=%v live=%v", loaded.Seen("stale"), loaded.Seen("live"))
	}
	onDisk, _ := os.ReadFile(path)
	if strings.Contains(string(onDisk), `"stale"`) {
		t.Fatalf("pruned key still on disk: %s", onDisk)
	}
}

// A store written before seen entries carried timestamps must keep deduping
// every key after upgrade, and be rewritten in the new shape.
func TestStoreMigratesLegacySeenSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	legacy := `{"watermarks":{"org/repo":"2026-09-01T00:00:00Z"},"seen":{"node-1":true,"stage-comment:r:1:plan":true}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Seen("node-1") || !s.Seen("stage-comment:r:1:plan") {
		t.Fatal("legacy seen keys lost on upgrade")
	}
	var onDisk map[string]json.RawMessage
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if _, ok := onDisk["seen"]; ok {
		t.Fatalf("legacy seen key rewritten: %s", b)
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Seen("node-1") || !reloaded.Seen("stage-comment:r:1:plan") {
		t.Fatal("migrated seen keys not persisted")
	}
}

package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readArchiveEntries parses archive.jsonl into a map keyed by bead ID.
func readArchiveEntries(t *testing.T, dir string) map[string]ArchivedBead {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, archiveFileName))
	if err != nil {
		t.Fatalf("reading archive file: %v", err)
	}
	entries := make(map[string]ArchivedBead)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e ArchivedBead
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parsing archive line %q: %v", line, err)
		}
		entries[e.ID] = e
	}
	return entries
}

// TestArchive_PreservesAuditFields is the regression test for #3971: the
// archive record must preserve the merge-outcome metadata (pr_merged, pr_ref,
// fix_attempts), the actor, the status, and CreatedAt — the fields the ACMM
// advisor's MergeSuccessRate (#3972) needs to be computed from history.
// Before the fix, ArchivedBead silently dropped all of them, so this test
// fails against the unfixed code (the JSON has no such keys and the asserted
// values come back zero).
func TestArchive_PreservesAuditFields(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("merged feature", TypeFeature, PriorityHigh, "merge-bot", "org/repo#42")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[string]string{
		"pr_merged":    "true",
		"pr_ref":       "org/repo#42",
		"fix_attempts": "2",
	} {
		if err := s.SetMetadata(b.ID, k, v); err != nil {
			t.Fatalf("SetMetadata %s: %v", k, err)
		}
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	entries := readArchiveEntries(t, dir)
	e, ok := entries[b.ID]
	if !ok {
		t.Fatalf("archive missing entry for %s: %v", b.ID, entries)
	}
	if got := e.Metadata["pr_merged"]; got != "true" {
		t.Errorf("archived metadata pr_merged = %v, want \"true\"", got)
	}
	if got := e.Metadata["pr_ref"]; got != "org/repo#42" {
		t.Errorf("archived metadata pr_ref = %v, want \"org/repo#42\"", got)
	}
	if got := e.Metadata["fix_attempts"]; got != "2" {
		t.Errorf("archived metadata fix_attempts = %v, want \"2\"", got)
	}
	if e.Actor != "merge-bot" {
		t.Errorf("archived actor = %q, want \"merge-bot\"", e.Actor)
	}
	if e.Status != StatusClosed {
		t.Errorf("archived status = %q, want %q", e.Status, StatusClosed)
	}
	if e.CreatedAt.IsZero() {
		t.Errorf("archived created_at is zero, want the bead's creation time")
	}
	if e.ClosedAt.IsZero() {
		t.Errorf("archived closed_at is zero, want the bead's close time")
	}
}

// TestEvictOldClosed_ArchivePreservesAuditFields is the eviction-path half of
// the #3971 regression: every bead removed by evictOldClosed must carry its
// full audit record (actor, metadata, status, created_at) into archive.jsonl,
// exactly as Archive() does — both paths share newArchivedBead. Before the
// fix, eviction wrote the same field-stripped record as Archive, so this test
// fails against the unfixed code with empty actor/metadata in every entry.
func TestEvictOldClosed_ArchivePreservesAuditFields(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	// Inject beads directly into the map to avoid 5000+ disk writes,
	// matching the existing eviction test pattern.
	const extra = 10
	total := maxBeadCount + extra
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("bead-%05d", i)
		status := StatusOpen
		if i%2 == 0 {
			status = StatusClosed
		}
		store.beads[id] = &Bead{
			ID:       id,
			Title:    fmt.Sprintf("test bead %d", i),
			Type:     TypeTask,
			Status:   status,
			Priority: PriorityLow,
			Actor:    "scanner",
			Metadata: map[string]interface{}{
				"pr_merged": "true",
				"pr_ref":    fmt.Sprintf("org/repo#%d", i),
			},
			CreatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Minute)},
			UpdatedAt: flexTime{time.Now().UTC().Add(-time.Duration(total-i) * time.Minute)},
		}
	}

	before := make(map[string]bool, len(store.beads))
	for id := range store.beads {
		before[id] = true
	}
	store.evictOldClosed()
	var evicted []string
	for id := range before {
		if _, still := store.beads[id]; !still {
			evicted = append(evicted, id)
		}
	}
	store.mu.Unlock()

	if len(evicted) == 0 {
		t.Fatalf("expected eviction to remove beads (total=%d, cap=%d)", total, maxBeadCount)
	}

	entries := readArchiveEntries(t, dir)
	for _, id := range evicted {
		e, ok := entries[id]
		if !ok {
			t.Errorf("evicted bead %s has no archive record", id)
			continue
		}
		if e.Actor != "scanner" {
			t.Errorf("archived actor for %s = %q, want \"scanner\"", id, e.Actor)
		}
		if got := e.Metadata["pr_merged"]; got != "true" {
			t.Errorf("archived metadata pr_merged for %s = %v, want \"true\"", id, got)
		}
		if e.Status != StatusClosed {
			t.Errorf("archived status for %s = %q, want %q", id, e.Status, StatusClosed)
		}
		if e.CreatedAt.IsZero() {
			t.Errorf("archived created_at for %s is zero, want the bead's creation time", id)
		}
	}
}

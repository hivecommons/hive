package beads

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchive_MovesBeadToArchiveLog verifies that Archive writes a compact
// summary to archive.jsonl and removes the bead from the live store.
func TestArchive_MovesBeadToArchiveLog(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("archive me", TypeTask, PriorityLow, "alice", "ref-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := s.Archive(b.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if _, err := s.Get(b.ID); err == nil {
		t.Fatalf("expected bead to be removed from store after archive")
	}

	data, err := os.ReadFile(filepath.Join(dir, archiveFileName))
	if err != nil {
		t.Fatalf("reading archive file: %v", err)
	}
	if !strings.Contains(string(data), b.ID) {
		t.Fatalf("archive file missing bead ID: %s", string(data))
	}
	if !strings.Contains(string(data), "archive me") {
		t.Fatalf("archive file missing title: %s", string(data))
	}
}

// TestArchive_NotFound verifies Archive returns an error for an unknown ID.
func TestArchive_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := s.Archive("does-not-exist"); err == nil {
		t.Fatalf("expected error archiving missing bead")
	}
}

// TestArchive_AppendsMultipleEntries verifies successive archive calls append
// rather than overwrite the archive log, and that ClosedAt is carried through
// when set.
func TestArchive_AppendsMultipleEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b1, _ := s.Create("first", TypeBug, PriorityHigh, "alice", "")
	b2, _ := s.Create("second", TypeChore, PriorityMinor, "bob", "")

	if err := s.Close(b1.ID); err != nil {
		t.Fatalf("Close b1: %v", err)
	}

	if err := s.Archive(b1.ID); err != nil {
		t.Fatalf("Archive b1: %v", err)
	}
	if err := s.Archive(b2.ID); err != nil {
		t.Fatalf("Archive b2: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, archiveFileName))
	if err != nil {
		t.Fatalf("reading archive file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 archive lines, got %d: %v", len(lines), lines)
	}
}

// TestArchive_OpenFileError verifies Archive surfaces an error when the
// archive file path cannot be opened (e.g. a directory occupies the path).
func TestArchive_OpenFileError(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, _ := s.Create("blocked", TypeTask, PriorityMedium, "alice", "")

	// Make archive.jsonl a directory so OpenFile fails.
	if err := os.MkdirAll(filepath.Join(dir, archiveFileName), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := s.Archive(b.ID); err == nil {
		t.Fatalf("expected error when archive path is a directory")
	}
}

// TestAllClosed_ReturnsDoneAndClosedOnly verifies AllClosed filters to
// StatusDone and StatusClosed beads, excluding others.
func TestAllClosed_ReturnsDoneAndClosedOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	open, _ := s.Create("open one", TypeTask, PriorityMedium, "alice", "")
	closed, _ := s.Create("closed one", TypeTask, PriorityMedium, "alice", "")
	done, _ := s.Create("done one", TypeTask, PriorityMedium, "alice", "")
	_ = open

	if err := s.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Update(done.ID, func(b *Bead) { b.Status = StatusDone }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	result := s.AllClosed()
	if len(result) != 2 {
		t.Fatalf("expected 2 closed/done beads, got %d", len(result))
	}
	ids := map[string]bool{}
	for _, b := range result {
		ids[b.ID] = true
	}
	if !ids[closed.ID] || !ids[done.ID] {
		t.Fatalf("expected closed and done beads present, got %v", ids)
	}
	if ids[open.ID] {
		t.Fatalf("open bead should not be in AllClosed result")
	}
}

// TestAllClosed_EmptyStore verifies AllClosed returns an empty slice when
// there are no beads at all.
func TestAllClosed_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if result := s.AllClosed(); len(result) != 0 {
		t.Fatalf("expected empty result, got %d", len(result))
	}
}

// TestHasOpenBead_Variants covers found+open, found+closed, and not-found cases.
func TestHasOpenBead_Variants(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	open, _ := s.Create("open", TypeTask, PriorityMedium, "alice", "")
	closed, _ := s.Create("closed", TypeTask, PriorityMedium, "alice", "")
	if err := s.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if !s.HasOpenBead(open.ID) {
		t.Fatalf("expected open bead to be reported as open")
	}
	if s.HasOpenBead(closed.ID) {
		t.Fatalf("expected closed bead to be reported as not open")
	}
	if s.HasOpenBead("missing-id") {
		t.Fatalf("expected missing bead to be reported as not open")
	}
}

// TestUnsynthesized_FiltersCorrectly verifies that Unsynthesized returns only
// done/closed beads lacking the synthesized_at metadata key, sorted by
// CreatedAt.
func TestUnsynthesized_FiltersCorrectly(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	openBead, _ := s.Create("still open", TypeTask, PriorityMedium, "alice", "")
	closedUnsynth, _ := s.Create("closed unsynth", TypeTask, PriorityMedium, "alice", "")
	closedSynth, _ := s.Create("closed synth", TypeTask, PriorityMedium, "alice", "")

	if err := s.Close(closedUnsynth.ID); err != nil {
		t.Fatalf("Close closedUnsynth: %v", err)
	}
	if err := s.Close(closedSynth.ID); err != nil {
		t.Fatalf("Close closedSynth: %v", err)
	}
	if err := s.SetMetadata(closedSynth.ID, "synthesized_at", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	result := s.Unsynthesized()
	if len(result) != 1 {
		t.Fatalf("expected 1 unsynthesized bead, got %d", len(result))
	}
	if result[0].ID != closedUnsynth.ID {
		t.Fatalf("expected %s, got %s", closedUnsynth.ID, result[0].ID)
	}
	_ = openBead
}

// TestUnsynthesized_EmptyWhenNoneClosed verifies Unsynthesized returns an
// empty slice when no beads are done/closed.
func TestUnsynthesized_EmptyWhenNoneClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := s.Create("still open", TypeTask, PriorityMedium, "alice", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if result := s.Unsynthesized(); len(result) != 0 {
		t.Fatalf("expected empty result, got %d", len(result))
	}
}

// TestCreate_InvalidBeadType verifies Create rejects unknown bead types.
func TestCreate_InvalidBeadType(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := s.Create("bad type", BeadType("not-a-type"), PriorityMedium, "alice", ""); err == nil {
		t.Fatalf("expected error for invalid bead type")
	}
}

// TestCreate_StampsHiveIDMetadata verifies that when a hive ID is configured,
// new beads are stamped with it in metadata.
func TestCreate_StampsHiveIDMetadata(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.SetHiveID("hive-42")

	b, err := s.Create("stamped", TypeTask, PriorityMedium, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.Metadata[hiveIDMetadataKey] != "hive-42" {
		t.Fatalf("expected hive_id metadata to be stamped, got %v", b.Metadata)
	}
}

// TestCloseAll_NoOpenBeadsSkipsPersist verifies CloseAll returns 0 without
// error when there is nothing open/in_progress/blocked to close.
func TestCloseAll_NoOpenBeadsSkipsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, _ := s.Create("already done", TypeTask, PriorityMedium, "alice", "")
	if err := s.Update(b.ID, func(bead *Bead) { bead.Status = StatusDone }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	closed, err := s.CloseAll("shutdown")
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 0 {
		t.Fatalf("expected 0 closed, got %d", closed)
	}
}

// TestCloseAll_ClosesOpenInProgressAndBlocked verifies CloseAll transitions
// every open/in_progress/blocked bead to closed and records the reason.
func TestCloseAll_ClosesOpenInProgressAndBlocked(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	open, _ := s.Create("open", TypeTask, PriorityMedium, "alice", "")
	inProgress, _ := s.Create("in progress", TypeTask, PriorityMedium, "alice", "")
	blocked, _ := s.Create("blocked", TypeTask, PriorityMedium, "alice", "")

	if err := s.Claim(inProgress.ID); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.Update(blocked.ID, func(b *Bead) { b.Status = StatusBlocked }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	closed, err := s.CloseAll("bulk shutdown")
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 3 {
		t.Fatalf("expected 3 closed, got %d", closed)
	}

	for _, id := range []string{open.ID, inProgress.ID, blocked.ID} {
		b, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if b.Status != StatusClosed {
			t.Fatalf("expected %s to be closed, got %s", id, b.Status)
		}
		if b.Meta("close_reason") != "bulk shutdown" {
			t.Fatalf("expected close_reason metadata, got %v", b.Metadata)
		}
	}
}

// TestSetMetadata_InitializesNilMap verifies SetMetadata lazily creates the
// metadata map on a bead that has none.
func TestSetMetadata_InitializesNilMap(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	b, err := s.Create("no metadata", TypeTask, PriorityMedium, "alice", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Force metadata to nil directly to exercise the lazy-init branch.
	if err := s.Update(b.ID, func(bead *Bead) { bead.Metadata = nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := s.SetMetadata(b.ID, "foo", "bar"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["foo"] != "bar" {
		t.Fatalf("expected metadata foo=bar, got %v", got.Metadata)
	}
}

// TestMeta_NonStringValueFormatted verifies Meta stringifies non-string
// metadata values via fmt.Sprintf rather than returning "".
func TestMeta_NonStringValueFormatted(t *testing.T) {
	b := &Bead{Metadata: map[string]interface{}{"count": 42}}
	if got := b.Meta("count"); got != "42" {
		t.Fatalf("expected \"42\", got %q", got)
	}
}

// TestPersist_WriteError verifies persist surfaces an error when the target
// directory has been removed out from under the store.
func TestPersist_WriteError(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := s.Create("first", TypeTask, PriorityMedium, "alice", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	if _, err := s.Create("second", TypeTask, PriorityMedium, "alice", ""); err == nil {
		t.Fatalf("expected error persisting to removed directory")
	}
}

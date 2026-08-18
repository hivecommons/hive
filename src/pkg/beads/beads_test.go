package beads

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ptr helpers

func statusPtr(s Status) *Status { return &s }
func strPtr(s string) *string    { return &s }

// TestNewStore_CreatesDirectoryAndEmptyStore verifies that NewStore creates the
// target directory if it does not exist and starts with an empty ledger.
func TestNewStore_CreatesDirectoryAndEmptyStore(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "beads")

	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("directory not created: %v", err)
	}

	if got := s.Count(); got != 0 {
		t.Fatalf("expected 0 beads, got %d", got)
	}
}

// TestNewStore_LoadsExistingBeadsJson verifies that NewStore reads an existing
// beads.json written by a previous store instance (persistence round-trip).
func TestNewStore_LoadsExistingBeadsJson(t *testing.T) {
	dir := t.TempDir()

	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (first): %v", err)
	}

	if _, err := s1.Create("task one", TypeTask, PriorityMedium, "alice", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s1.Create("task two", TypeBug, PriorityHigh, "bob", ""); err != nil {
		t.Fatalf("Create: %v", err)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (second): %v", err)
	}

	if got := s2.Count(); got != 2 {
		t.Fatalf("expected 2 beads after reload, got %d", got)
	}
}

// TestCreate_CorrectFields checks that a created bead has the right initial
// field values and that it is persisted to disk.
func TestCreate_CorrectFields(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	before := time.Now().UTC().Add(-time.Second)
	b, err := s.Create("fix login", TypeBug, PriorityCritical, "alice", "gh-123")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	after := time.Now().UTC().Add(time.Second)

	if b.ID == "" {
		t.Error("ID should not be empty")
	}
	if len(b.ID) != 12 {
		t.Errorf("expected ID length 12, got %d", len(b.ID))
	}
	if b.Title != "fix login" {
		t.Errorf("Title: got %q, want %q", b.Title, "fix login")
	}
	if b.Type != TypeBug {
		t.Errorf("Type: got %q, want %q", b.Type, TypeBug)
	}
	if b.Status != StatusOpen {
		t.Errorf("Status: got %q, want %q", b.Status, StatusOpen)
	}
	if b.Priority != PriorityCritical {
		t.Errorf("Priority: got %d, want %d", b.Priority, PriorityCritical)
	}
	if b.Actor != "alice" {
		t.Errorf("Actor: got %q, want %q", b.Actor, "alice")
	}
	if b.ExternalRef != "gh-123" {
		t.Errorf("ExternalRef: got %q, want %q", b.ExternalRef, "gh-123")
	}
	if b.Metadata == nil {
		t.Error("Metadata map should be initialised (not nil)")
	}
	if b.CreatedAt.Before(before) || b.CreatedAt.After(after) {
		t.Errorf("CreatedAt %v outside expected range [%v, %v]", b.CreatedAt, before, after)
	}
	if !b.UpdatedAt.Equal(b.CreatedAt.Time) {
		t.Errorf("UpdatedAt %v should equal CreatedAt %v on creation", b.UpdatedAt, b.CreatedAt)
	}
	if b.ClosedAt != nil {
		t.Error("ClosedAt should be nil on creation")
	}

	// Verify persisted to disk
	beadsFile := filepath.Join(dir, "beads.json")
	if _, err := os.Stat(beadsFile); err != nil {
		t.Fatalf("beads.json not created: %v", err)
	}
}

func TestUpdate_TransitionToDoneStampsClosedAt(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b, err := s.Create("finish me", TypeTask, PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if b.ClosedAt != nil {
		t.Fatal("new bead should not have ClosedAt")
	}
	if err := s.Update(b.ID, func(b *Bead) {
		b.Status = StatusDone
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ClosedAt == nil {
		t.Fatal("transition to done should stamp ClosedAt")
	}
	firstClosed := got.ClosedAt.Time
	if err := s.SetMetadata(b.ID, "later", "value"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err = s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after metadata: %v", err)
	}
	if !got.ClosedAt.Equal(firstClosed) {
		t.Fatalf("ClosedAt changed after metadata update: got %s want %s", got.ClosedAt.Time, firstClosed)
	}
}

// TestGet_RetrievesByID checks happy-path retrieval.
func TestGet_RetrievesByID(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	created, _ := s.Create("hello", TypeFeature, PriorityLow, "carol", "")
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("Get returned wrong bead: got ID %q, want %q", got.ID, created.ID)
	}
}

// TestGet_MissingIDReturnsError verifies that Get returns an error for an
// unknown bead ID.
func TestGet_MissingIDReturnsError(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	if _, err := s.Get("does-not-exist"); err == nil {
		t.Fatal("expected error for missing bead ID, got nil")
	}
}

// TestUpdate_ModifiesFieldsAndPersists checks that Update applies the callback
// and writes updated data to disk.
func TestUpdate_ModifiesFieldsAndPersists(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("original title", TypeTask, PriorityMedium, "dave", "")

	beforeUpdate := time.Now().UTC().Add(-time.Second)
	err := s.Update(b.ID, func(b *Bead) {
		b.Title = "updated title"
		b.Notes = "some notes"
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	afterUpdate := time.Now().UTC().Add(time.Second)

	got, _ := s.Get(b.ID)
	if got.Title != "updated title" {
		t.Errorf("Title not updated: got %q", got.Title)
	}
	if got.Notes != "some notes" {
		t.Errorf("Notes not updated: got %q", got.Notes)
	}
	if got.UpdatedAt.Before(beforeUpdate) || got.UpdatedAt.After(afterUpdate) {
		t.Errorf("UpdatedAt %v outside expected range", got.UpdatedAt)
	}
}

// TestUpdate_MissingIDReturnsError checks Update returns an error for an
// unknown ID.
func TestUpdate_MissingIDReturnsError(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	if err := s.Update("ghost", func(b *Bead) {}); err == nil {
		t.Fatal("expected error for missing bead ID, got nil")
	}
}

// TestClaim_SetsStatusInProgress verifies Claim transitions Status to in_progress.
func TestClaim_SetsStatusInProgress(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("work item", TypeChore, PriorityLow, "eve", "")
	if err := s.Claim(b.ID); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, _ := s.Get(b.ID)
	if got.Status != StatusInProgress {
		t.Errorf("Status: got %q, want %q", got.Status, StatusInProgress)
	}
}

// TestClose_SetsStatusClosedWithTimestamp verifies Close transitions Status to
// closed and sets ClosedAt.
func TestClose_SetsStatusClosedWithTimestamp(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("done task", TypeTask, PriorityHigh, "frank", "")
	before := time.Now().UTC().Add(-time.Second)
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	got, _ := s.Get(b.ID)
	if got.Status != StatusClosed {
		t.Errorf("Status: got %q, want %q", got.Status, StatusClosed)
	}
	if got.ClosedAt == nil {
		t.Fatal("ClosedAt should be set after Close")
	}
	if got.ClosedAt.Before(before) || got.ClosedAt.After(after) {
		t.Errorf("ClosedAt %v outside expected range [%v, %v]", *got.ClosedAt, before, after)
	}
}

// TestList_NoFilter returns all beads.
func TestList_NoFilter(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("a", TypeTask, PriorityLow, "actor1", "")
	s.Create("b", TypeBug, PriorityHigh, "actor2", "ref-1")
	s.Create("c", TypeFeature, PriorityMedium, "actor1", "")

	all := s.List(ListFilter{})
	if len(all) != 3 {
		t.Fatalf("expected 3, got %d", len(all))
	}
}

// TestList_FilterByStatus returns only beads matching the given status.
func TestList_FilterByStatus(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b1, _ := s.Create("open one", TypeTask, PriorityLow, "actor", "")
	b2, _ := s.Create("will close", TypeTask, PriorityLow, "actor", "")
	s.Create("open two", TypeTask, PriorityLow, "actor", "")

	s.Claim(b1.ID)
	s.Close(b2.ID)

	open := s.List(ListFilter{Status: statusPtr(StatusOpen)})
	if len(open) != 1 {
		t.Fatalf("expected 1 open bead, got %d", len(open))
	}
	if open[0].Status != StatusOpen {
		t.Errorf("unexpected status %q", open[0].Status)
	}

	inProgress := s.List(ListFilter{Status: statusPtr(StatusInProgress)})
	if len(inProgress) != 1 {
		t.Fatalf("expected 1 in_progress bead, got %d", len(inProgress))
	}

	closed := s.List(ListFilter{Status: statusPtr(StatusClosed)})
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed bead, got %d", len(closed))
	}
}

// TestList_FilterByActor returns only beads for the given actor.
func TestList_FilterByActor(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("alice task", TypeTask, PriorityLow, "alice", "")
	s.Create("bob task", TypeTask, PriorityLow, "bob", "")
	s.Create("alice bug", TypeBug, PriorityHigh, "alice", "")

	aliceBeads := s.List(ListFilter{Actor: strPtr("alice")})
	if len(aliceBeads) != 2 {
		t.Fatalf("expected 2 beads for alice, got %d", len(aliceBeads))
	}
	for _, b := range aliceBeads {
		if b.Actor != "alice" {
			t.Errorf("unexpected actor %q", b.Actor)
		}
	}
}

// TestList_FilterByExternalRef returns only beads with the given external ref.
func TestList_FilterByExternalRef(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("linked", TypeTask, PriorityLow, "actor", "gh-42")
	s.Create("not linked", TypeTask, PriorityLow, "actor", "")
	s.Create("also linked", TypeTask, PriorityLow, "actor", "gh-42")

	linked := s.List(ListFilter{ExternalRef: strPtr("gh-42")})
	if len(linked) != 2 {
		t.Fatalf("expected 2 beads with ref gh-42, got %d", len(linked))
	}
}

// TestList_IsSortedByCreatedAt verifies the returned slice is in creation order.
func TestList_IsSortedByCreatedAt(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	for i := 0; i < 5; i++ {
		s.Create("item", TypeTask, PriorityLow, "actor", "")
		// tiny pause so CreatedAt values are strictly ordered
		time.Sleep(2 * time.Millisecond)
	}

	all := s.List(ListFilter{})
	for i := 1; i < len(all); i++ {
		if !all[i-1].CreatedAt.Before(all[i].CreatedAt.Time) {
			t.Errorf("list not sorted: index %d (%v) >= index %d (%v)",
				i-1, all[i-1].CreatedAt, i, all[i].CreatedAt)
		}
	}
}

// TestReady_ReturnsOnlyOpenBeads verifies Ready filters to open status.
func TestReady_ReturnsOnlyOpenBeads(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b1, _ := s.Create("open", TypeTask, PriorityLow, "actor", "")
	b2, _ := s.Create("claimed", TypeTask, PriorityLow, "actor", "")
	s.Create("open2", TypeTask, PriorityLow, "actor", "")

	_ = b1
	s.Claim(b2.ID)

	ready := s.Ready("")
	if len(ready) != 2 {
		t.Fatalf("Ready(): expected 2, got %d", len(ready))
	}
	for _, b := range ready {
		if b.Status != StatusOpen {
			t.Errorf("Ready returned non-open bead with status %q", b.Status)
		}
	}
}

// TestReady_FiltersByActorWhenProvided verifies that Ready with a non-empty
// actor only returns open beads for that actor.
func TestReady_FiltersByActorWhenProvided(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("alice open", TypeTask, PriorityLow, "alice", "")
	s.Create("bob open", TypeTask, PriorityLow, "bob", "")
	s.Create("alice open 2", TypeTask, PriorityLow, "alice", "")

	aliceReady := s.Ready("alice")
	if len(aliceReady) != 2 {
		t.Fatalf("Ready(alice): expected 2, got %d", len(aliceReady))
	}
	for _, b := range aliceReady {
		if b.Actor != "alice" {
			t.Errorf("Ready(alice) returned bead for actor %q", b.Actor)
		}
	}
}

// TestFindByExternalRef_FindsMatchingBead checks happy path.
func TestFindByExternalRef_FindsMatchingBead(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("no ref", TypeTask, PriorityLow, "actor", "")
	want, _ := s.Create("has ref", TypeTask, PriorityLow, "actor", "gh-99")

	got := s.FindByExternalRef("gh-99")
	if got == nil {
		t.Fatal("FindByExternalRef returned nil for existing ref")
	}
	if got.ID != want.ID {
		t.Errorf("got ID %q, want %q", got.ID, want.ID)
	}
}

// TestFindByExternalRef_ReturnsNilWhenMissing checks the not-found case.
func TestFindByExternalRef_ReturnsNilWhenMissing(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("no ref", TypeTask, PriorityLow, "actor", "")

	if got := s.FindByExternalRef("nonexistent"); got != nil {
		t.Errorf("expected nil, got bead %q", got.ID)
	}
}

// TestSetMetadata_StoresKeyValue checks that SetMetadata persists a key/value.
func TestSetMetadata_StoresKeyValue(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("meta test", TypeTask, PriorityLow, "actor", "")
	if err := s.SetMetadata(b.ID, "env", "prod"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, _ := s.Get(b.ID)
	if got.Meta("env") != "prod" {
		t.Errorf("Metadata[env]: got %q, want %q", got.Meta("env"), "prod")
	}
}

// TestSetMetadata_OverwritesExistingKey checks that SetMetadata replaces a
// previously set value.
func TestSetMetadata_OverwritesExistingKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("meta test", TypeTask, PriorityLow, "actor", "")
	s.SetMetadata(b.ID, "color", "blue")
	s.SetMetadata(b.ID, "color", "red")

	got, _ := s.Get(b.ID)
	if got.Metadata["color"] != "red" {
		t.Errorf("expected overwritten value %q, got %q", "red", got.Metadata["color"])
	}
}

// TestUnsetMetadata_RemovesKey checks that UnsetMetadata deletes a key.
func TestUnsetMetadata_RemovesKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("meta test", TypeTask, PriorityLow, "actor", "")
	s.SetMetadata(b.ID, "tmp", "value")

	if err := s.UnsetMetadata(b.ID, "tmp"); err != nil {
		t.Fatalf("UnsetMetadata: %v", err)
	}

	got, _ := s.Get(b.ID)
	if _, exists := got.Metadata["tmp"]; exists {
		t.Error("expected key to be removed after UnsetMetadata")
	}
}

// TestUnsetMetadata_NonExistentKeyIsNoop verifies that unsetting a missing key
// does not error.
func TestUnsetMetadata_NonExistentKeyIsNoop(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	b, _ := s.Create("meta test", TypeTask, PriorityLow, "actor", "")
	if err := s.UnsetMetadata(b.ID, "ghost"); err != nil {
		t.Fatalf("UnsetMetadata on missing key should not error: %v", err)
	}
}

// TestAddDependency_AppendsDependency checks that a dependency ID is appended.
func TestAddDependency_AppendsDependency(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	parent, _ := s.Create("parent", TypeEpic, PriorityHigh, "actor", "")
	child, _ := s.Create("child", TypeTask, PriorityMedium, "actor", "")

	if err := s.AddDependency(parent.ID, child.ID); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}

	got, _ := s.Get(parent.ID)
	if len(got.DependsOn) != 1 || got.DependsOn[0] != child.ID {
		t.Errorf("DependsOn: got %v, want [%s]", got.DependsOn, child.ID)
	}
}

// TestAddDependency_Deduplicates checks that adding the same dependency twice
// results in only one entry.
func TestAddDependency_Deduplicates(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	parent, _ := s.Create("parent", TypeEpic, PriorityHigh, "actor", "")
	child, _ := s.Create("child", TypeTask, PriorityMedium, "actor", "")

	s.AddDependency(parent.ID, child.ID)
	s.AddDependency(parent.ID, child.ID)

	got, _ := s.Get(parent.ID)
	if len(got.DependsOn) != 1 {
		t.Errorf("expected 1 dependency after duplicate add, got %d", len(got.DependsOn))
	}
}

// TestCount_ReturnsCorrectCount checks the Count method at various stages.
func TestCount_ReturnsCorrectCount(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	if c := s.Count(); c != 0 {
		t.Fatalf("initial Count: got %d, want 0", c)
	}

	s.Create("one", TypeTask, PriorityLow, "actor", "")
	if c := s.Count(); c != 1 {
		t.Fatalf("after 1 create: got %d, want 1", c)
	}

	s.Create("two", TypeTask, PriorityLow, "actor", "")
	s.Create("three", TypeTask, PriorityLow, "actor", "")
	if c := s.Count(); c != 3 {
		t.Fatalf("after 3 creates: got %d, want 3", c)
	}
}

// TestPersistence_ReloadRestoresAllFields checks that all bead fields survive
// a store close/reopen cycle.
func TestPersistence_ReloadRestoresAllFields(t *testing.T) {
	dir := t.TempDir()

	s1, _ := NewStore(dir)
	b, _ := s1.Create("persist me", TypeDecision, PriorityCritical, "grace", "ext-007")
	s1.Claim(b.ID)
	s1.SetMetadata(b.ID, "key", "val")
	s1.Update(b.ID, func(b *Bead) { b.Notes = "important note" })

	dep, _ := s1.Create("dep bead", TypeTask, PriorityLow, "grace", "")
	s1.AddDependency(b.ID, dep.ID)

	// Re-open the store from the same directory.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}

	got, err := s2.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after reload: %v", err)
	}

	if got.Title != "persist me" {
		t.Errorf("Title: got %q, want %q", got.Title, "persist me")
	}
	if got.Type != TypeDecision {
		t.Errorf("Type: got %q, want %q", got.Type, TypeDecision)
	}
	if got.Status != StatusInProgress {
		t.Errorf("Status: got %q, want %q", got.Status, StatusInProgress)
	}
	if got.Priority != PriorityCritical {
		t.Errorf("Priority: got %d, want %d", got.Priority, PriorityCritical)
	}
	if got.Actor != "grace" {
		t.Errorf("Actor: got %q, want %q", got.Actor, "grace")
	}
	if got.ExternalRef != "ext-007" {
		t.Errorf("ExternalRef: got %q, want %q", got.ExternalRef, "ext-007")
	}
	if got.Notes != "important note" {
		t.Errorf("Notes: got %q, want %q", got.Notes, "important note")
	}
	if got.Metadata["key"] != "val" {
		t.Errorf("Metadata[key]: got %q, want %q", got.Metadata["key"], "val")
	}
	if len(got.DependsOn) != 1 || got.DependsOn[0] != dep.ID {
		t.Errorf("DependsOn: got %v, want [%s]", got.DependsOn, dep.ID)
	}
	if s2.Count() != 2 {
		t.Fatalf("Count after reload: got %d, want 2", s2.Count())
	}
}

// TestPersistence_ClosedAtSurvivesReload checks that the ClosedAt pointer is
// correctly marshalled and unmarshalled.
func TestPersistence_ClosedAtSurvivesReload(t *testing.T) {
	dir := t.TempDir()

	s1, _ := NewStore(dir)
	b, _ := s1.Create("close me", TypeTask, PriorityLow, "actor", "")
	s1.Close(b.ID)

	closedBead, _ := s1.Get(b.ID)
	wantClosedAt := *closedBead.ClosedAt

	s2, _ := NewStore(dir)
	got, _ := s2.Get(b.ID)

	if got.ClosedAt == nil {
		t.Fatal("ClosedAt nil after reload")
	}
	if !got.ClosedAt.Equal(wantClosedAt.Time) {
		t.Errorf("ClosedAt mismatch: got %v, want %v", *got.ClosedAt, wantClosedAt)
	}
}

// TestList_CombinedFilter verifies that multiple filter fields are AND-ed
// together (status + actor).
func TestList_CombinedFilter(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewStore(dir)

	s.Create("alice open", TypeTask, PriorityLow, "alice", "")
	b, _ := s.Create("alice claimed", TypeTask, PriorityLow, "alice", "")
	s.Create("bob open", TypeTask, PriorityLow, "bob", "")
	s.Claim(b.ID)

	result := s.List(ListFilter{
		Status: statusPtr(StatusOpen),
		Actor:  strPtr("alice"),
	})
	if len(result) != 1 {
		t.Fatalf("combined filter: expected 1, got %d", len(result))
	}
	if result[0].Actor != "alice" || result[0].Status != StatusOpen {
		t.Errorf("unexpected bead: %+v", result[0])
	}
}

func TestNewStore_MkdirError(t *testing.T) {
	// Try to create a store in a path that can't be created
	_, err := NewStore("/dev/null/impossible")
	if err == nil {
		t.Error("expected error for invalid directory path")
	}
}

func TestNewStore_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	// Write a corrupt beads.json
	os.WriteFile(dir+"/beads.json", []byte(`{not json`), 0o644)
	_, err := NewStore(dir)
	if err == nil {
		t.Error("expected error for corrupt beads.json")
	}
}

func TestNewStore_ReadError(t *testing.T) {
	dir := t.TempDir()
	// Create beads.json as a directory to cause read error
	os.Mkdir(dir+"/beads.json", 0o755)
	_, err := NewStore(dir)
	if err == nil {
		t.Error("expected error when beads.json is a directory")
	}
}

// TestReload_PicksUpExternalWrites simulates the bug where agents write beads
// to disk via the bd CLI but the in-memory store in the hive process is stale.
// Reload must re-read the file and reflect the new beads.
func TestReload_PicksUpExternalWrites(t *testing.T) {
	dir := t.TempDir()

	// Store 1: the "hive process" store — starts empty
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (hive): %v", err)
	}
	if s1.Count() != 0 {
		t.Fatalf("initial count: got %d, want 0", s1.Count())
	}

	// Store 2: simulates the bd CLI writing beads to the same directory
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore (bd): %v", err)
	}
	if _, err := s2.Create("external finding", TypeAdvisory, PriorityHigh, "scanner", ""); err != nil {
		t.Fatalf("Create via bd CLI: %v", err)
	}

	// Before Reload, s1 is stale
	if s1.Count() != 0 {
		t.Fatalf("s1 should still be 0 before Reload, got %d", s1.Count())
	}

	// After Reload, s1 picks up the external write
	if err := s1.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if s1.Count() != 1 {
		t.Fatalf("after Reload: got %d, want 1", s1.Count())
	}

	beads := s1.List(ListFilter{})
	if beads[0].Title != "external finding" {
		t.Errorf("Title: got %q, want %q", beads[0].Title, "external finding")
	}
}

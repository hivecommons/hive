package beads

import (
	"testing"
)

// ---------------------------------------------------------------------------
// SetHiveID
// ---------------------------------------------------------------------------

func TestSetHiveID(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	s.SetHiveID("test-hive-42")

	// Create a bead and verify it has the hive ID in metadata
	b, err := s.Create("test bead", TypeTask, PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if b.Meta(hiveIDMetadataKey) != "test-hive-42" {
		t.Errorf("hive_id metadata = %q, want test-hive-42", b.Meta(hiveIDMetadataKey))
	}
}

func TestSetHiveID_Empty(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// No SetHiveID call — metadata should not contain hive_id
	b, err := s.Create("test bead", TypeTask, PriorityMedium, "scanner", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if b.Meta(hiveIDMetadataKey) != "" {
		t.Error("hive_id should not be set when SetHiveID was not called")
	}
}

package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCloseAll(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	b1, _ := store.Create("bug1", TypeBug, PriorityHigh, "scanner", "")
	b2, _ := store.Create("bug2", TypeBug, PriorityLow, "scanner", "")
	b3, _ := store.Create("bug3", TypeTask, PriorityMedium, "scanner", "")

	store.CloseAll("test cleanup")

	for _, id := range []string{b1.ID, b2.ID, b3.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != StatusClosed {
			t.Errorf("bead %s status = %s, want closed", id, b.Status)
		}
		if b.ClosedAt == nil {
			t.Errorf("bead %s ClosedAt should be set", id)
		}
	}
}

func TestEvictOldClosed(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 10; i++ {
		b, _ := store.Create("bead", TypeTask, PriorityLow, "scanner", "")
		_ = store.Close(b.ID)
	}

	store.mu.Lock()
	initialCount := len(store.beads)
	store.evictOldClosed()
	afterCount := len(store.beads)
	store.mu.Unlock()

	if afterCount > initialCount {
		t.Errorf("eviction should not increase count: %d -> %d", initialCount, afterCount)
	}
}

func TestFlexTimeUnmarshalFormats(t *testing.T) {
	tests := []struct {
		input string
	}{
		{`"2024-01-15T10:30:00Z"`},
		{`"2024-01-15T10:30:00.000Z"`},
		{`"2024-01-15T10:30:00+00:00"`},
	}
	for _, tt := range tests {
		var ft flexTime
		if err := json.Unmarshal([]byte(tt.input), &ft); err != nil {
			t.Errorf("UnmarshalJSON(%s) error: %v", tt.input, err)
		}
		if ft.IsZero() {
			t.Errorf("UnmarshalJSON(%s) produced zero time", tt.input)
		}
	}
}

func TestFlexTimeUnmarshalEmpty(t *testing.T) {
	var ft flexTime
	if err := json.Unmarshal([]byte(`""`), &ft); err != nil {
		t.Errorf("empty string should not error: %v", err)
	}
	if !ft.IsZero() {
		t.Error("empty string should produce zero time")
	}
}

func TestFlexTimeUnmarshalNotString(t *testing.T) {
	var ft flexTime
	if err := json.Unmarshal([]byte(`123`), &ft); err == nil {
		t.Error("non-string should error")
	}
}

func TestFlexTimeUnmarshalInvalid(t *testing.T) {
	var ft flexTime
	err := json.Unmarshal([]byte(`"not-a-date"`), &ft)
	if err == nil {
		t.Error("expected error for invalid date")
	}
}

func TestFlexTimeMarshal(t *testing.T) {
	var ft flexTime
	_ = json.Unmarshal([]byte(`"2024-01-15T10:30:00Z"`), &ft)
	data, err := ft.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" {
		t.Error("MarshalJSON returned empty")
	}
}

func TestMetaHelper(t *testing.T) {
	b := &Bead{
		Metadata: map[string]interface{}{
			"key1": "value1",
			"key2": 42,
		},
	}
	if got := b.Meta("key1"); got != "value1" {
		t.Errorf("Meta(key1) = %q, want %q", got, "value1")
	}
	if got := b.Meta("key2"); got != "42" {
		t.Errorf("Meta(key2) = %q, want %q", got, "42")
	}
	if got := b.Meta("missing"); got != "" {
		t.Errorf("Meta(missing) = %q, want empty", got)
	}
	b2 := &Bead{}
	if got := b2.Meta("any"); got != "" {
		t.Errorf("Meta on nil metadata = %q, want empty", got)
	}
}

func TestSetMetadataCreate(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := store.Create("test", TypeBug, PriorityLow, "scanner", "")
	if err := store.SetMetadata(b.ID, "severity", "high"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(b.ID)
	if got.Meta("severity") != "high" {
		t.Errorf("metadata not set")
	}
}

func TestSetMetadataNotFound(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata("nonexistent", "key", "val"); err == nil {
		t.Error("expected error for missing bead")
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := store.Create("persist-test", TypeBug, PriorityHigh, "scanner", "ref.go")
	_ = store.SetMetadata(b.ID, "detail", "test detail")

	store2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := store2.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Title != "persist-test" {
		t.Errorf("Title = %q after reload", reloaded.Title)
	}
	if reloaded.Meta("detail") != "test detail" {
		t.Errorf("metadata lost after reload")
	}
}

func TestLoadCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "beads.json"), []byte("not json"), 0o644)
	_, err := NewStore(dir)
	if err == nil {
		t.Error("expected error loading corrupted beads file")
	}
}

package integrated

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteDurableStateFileAtomicallyReplacesExistingState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeDurableStateFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new\n" {
		t.Fatalf("durable replacement = %q, want %q", data, "new\n")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("durable replacement left temporary file %q", entry.Name())
		}
	}
}

func TestDurableRemoveStateFileDeletesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeDurableStateFile(path, []byte("state\n")); err != nil {
		t.Fatal(err)
	}
	if err := durableRemoveStateFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("durably removed state still exists: %v", err)
	}
	if _, err := os.Stat(path + ".deleted"); !os.IsNotExist(err) {
		t.Fatalf("durable removal left a tombstone: %v", err)
	}
	if err := durableRemoveStateFile(path); err != nil {
		t.Fatalf("idempotent durable removal: %v", err)
	}
}

func TestStoreDurablyDeletesMergeAuthorityAndIsIdempotent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	approval := exactTestMergeApproval(strings.Repeat("c", 64))
	intent := exactTestMergeIntent(&approval)
	if err := store.SaveMergeApproval(approval); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMergeIntent(intent); err != nil {
		t.Fatal(err)
	}

	for _, remove := range []struct {
		name string
		path string
		call func() error
	}{
		{name: "approval", path: filepath.Join(store.Dir(), "merge-approval.json"), call: store.DeleteMergeApproval},
		{name: "intent", path: filepath.Join(store.Dir(), "merge-intent.json"), call: store.DeleteMergeIntent},
	} {
		t.Run(remove.name, func(t *testing.T) {
			if err := remove.call(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(remove.path); !os.IsNotExist(err) {
				t.Fatalf("durably deleted %s still exists: %v", remove.name, err)
			}
			if _, err := os.Stat(remove.path + ".deleted"); !os.IsNotExist(err) {
				t.Fatalf("durably deleted %s left a tombstone: %v", remove.name, err)
			}
			if err := remove.call(); err != nil {
				t.Fatalf("idempotent %s deletion: %v", remove.name, err)
			}
		})
	}
}

func TestStoreSaveDurablyReplacesConfig(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	first := Config{Repository: "owner/first"}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := Config{Repository: "owner/second"}
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Repository != second.Repository {
		t.Fatalf("loaded repository = %q, want %q", loaded.Repository, second.Repository)
	}
}

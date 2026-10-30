package automerge

import (
	"os"
	"testing"
)

func TestBasePreparationStoreFileModes(t *testing.T) {
	dir := t.TempDir()
	store := NewBasePreparationStore(dir)

	// Perform a change to create the directory and files.
	err := store.change("owner/repo:main", func(st *basePreparation) error {
		st.Owner.Number = 42
		return nil
	})
	if err != nil {
		t.Fatalf("change failed: %v", err)
	}

	// Check directory mode is not group-writable.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat directory failed: %v", err)
	}
	dirMode := dirInfo.Mode()
	if (dirMode & 0o020) != 0 {
		t.Errorf("directory is group-writable: mode=%#o", dirMode)
	}

	// Check .json file mode is not group-writable.
	jsonPath := dir + "/sha256hex.json"
	// Find actual .json file since the name is sha256(key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir failed: %v", err)
	}
	var jsonFile os.FileInfo
	for _, e := range entries {
		if e.Name() != ".lock" {
			info, err := e.Info()
			if err != nil {
				t.Fatalf("info failed: %v", err)
			}
			jsonFile = info
			break
		}
	}
	if jsonFile != nil {
		jsonMode := jsonFile.Mode()
		if (jsonMode & 0o020) != 0 {
			t.Errorf(".json file is group-writable: mode=%#o", jsonMode)
		}
	}
}

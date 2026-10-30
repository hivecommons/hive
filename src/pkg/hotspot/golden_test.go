package hotspot

import (
	"os"
	"path/filepath"
	"testing"
)

// compareGolden compares got with the file at path; HIVE_UPDATE_GOLDEN=1
// rewrites it.
func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("HIVE_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (set HIVE_UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(want) != got {
		t.Errorf("golden mismatch for %s\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

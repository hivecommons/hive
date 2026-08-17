package advisory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadNewFindingsNonexistentDir(t *testing.T) {
	store := &Store{dir: "/tmp/nonexistent-advisory-dir-xyz", lastReadPos: make(map[string]int64)}
	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Errorf("should return nil error for nonexistent dir: %v", err)
	}
	if findings != nil {
		t.Error("should return nil findings for nonexistent dir")
	}
}

func TestReadNewFindingsWithAgentInData(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	// Write finding WITH agent field
	data := `{"title":"bug found","severity":"high","agent":"scanner"}` + "\n"
	os.WriteFile(filepath.Join(dir, "scanner.jsonl"), []byte(data), 0644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatalf("ReadNewFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Agent != "scanner" {
		t.Errorf("agent = %q, want 'scanner'", findings[0].Agent)
	}
}

func TestReadNewFindingsWithoutAgentField(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	// Write finding WITHOUT agent field — should be set from filename
	data := `{"title":"missing agent","severity":"medium"}` + "\n"
	os.WriteFile(filepath.Join(dir, "quality.jsonl"), []byte(data), 0644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatalf("ReadNewFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Agent != "quality" {
		t.Errorf("agent should be set from filename, got %q", findings[0].Agent)
	}
}

func TestReadNewFindingsSkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	os.MkdirAll(filepath.Join(dir, "subdir.jsonl"), 0755)
	data := `{"title":"real finding","severity":"low"}` + "\n"
	os.WriteFile(filepath.Join(dir, "scanner.jsonl"), []byte(data), 0644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatalf("ReadNewFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("expected 1 finding (skipping dir), got %d", len(findings))
	}
}

func TestReadNewFindingsSkipsNonJSONL(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("not jsonl"), 0644)
	data := `{"title":"valid","severity":"low"}` + "\n"
	os.WriteFile(filepath.Join(dir, "agent.jsonl"), []byte(data), 0644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatalf("ReadNewFindings: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("expected 1 finding (skipping .txt), got %d", len(findings))
	}
}

func TestReadNewFindingsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	data := "not valid json\n" + `{"title":"valid","severity":"low"}` + "\n"
	os.WriteFile(filepath.Join(dir, "scanner.jsonl"), []byte(data), 0644)

	findings, err := store.ReadNewFindings()
	if err != nil {
		t.Fatalf("ReadNewFindings: %v", err)
	}
	// Should skip invalid line and return valid one
	if len(findings) != 1 {
		t.Errorf("expected 1 valid finding, got %d", len(findings))
	}
}

func TestReadNewFindingsIncrementalRead(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, lastReadPos: make(map[string]int64)}

	path := filepath.Join(dir, "scanner.jsonl")
	os.WriteFile(path, []byte(`{"title":"first"}`+"\n"), 0644)

	f1, _ := store.ReadNewFindings()
	if len(f1) != 1 {
		t.Fatalf("first read: expected 1, got %d", len(f1))
	}

	// Append more data
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	f.WriteString(`{"title":"second"}` + "\n")
	f.Close()

	f2, _ := store.ReadNewFindings()
	if len(f2) != 1 {
		t.Errorf("incremental read: expected 1 new, got %d", len(f2))
	}
	if f2[0].Title != "second" {
		t.Errorf("incremental should return 'second', got %q", f2[0].Title)
	}
}

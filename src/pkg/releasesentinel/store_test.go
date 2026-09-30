package releasesentinel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileStore_MissingFileIsEmpty(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "nope.json"))
	recs, err := s.Load()
	if err != nil || recs == nil || len(recs) != 0 {
		t.Fatalf("Load = %v, %v; want empty non-nil map", recs, err)
	}
}

func TestFileStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	s := NewFileStore(path)
	if s.Path() != path {
		t.Fatalf("Path = %q", s.Path())
	}
	now := time.Date(2026, 9, 29, 1, 2, 3, 0, time.UTC)
	in := map[string]*Record{
		tag1: {Tag: tag1, Repo: "acme/widgets", SHA: shaA, State: StateFixing, Round: 2, RoundSHA: shaA, RoundStartedAt: now,
			BlockingRuns: []RunRef{{ID: 1, Name: "ci", Conclusion: "failure"}},
			History:      []Transition{{At: now, From: StateAwaitingCI, To: StateFixing, SHA: shaA, Round: 2, Reason: "r"}}},
	}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
	out, err := NewFileStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	got := out[tag1]
	if got == nil || got.Round != 2 || got.State != StateFixing || !got.RoundStartedAt.Equal(now) || len(got.History) != 1 || got.BlockingRuns[0].ID != 1 {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestFileStore_SaveNilWritesEmptyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := NewFileStore(path).Save(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("state file = %q, %v", data, err)
	}
}

func TestFileStore_LoadErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := NewFileStore(write("corrupt.json", "{not json")).Load(); err == nil {
		t.Fatal("corrupt state loaded without error; the round count would be forgotten")
	}
	if _, err := NewFileStore(write("future.json", `{"version": 99, "releases": {}}`)).Load(); err == nil {
		t.Fatal("future state version loaded without error")
	}
	recs, err := NewFileStore(write("nulls.json", `{"version": 1, "releases": {"v1.0.0": null, "v1.0.1": {"tag": "v1.0.1", "state": "green"}}}`)).Load()
	if err != nil || len(recs) != 1 || recs["v1.0.1"] == nil {
		t.Fatalf("null entries: %v, %v", recs, err)
	}
	// A directory where the file should be is an I/O error, not "empty".
	if err := os.Mkdir(filepath.Join(dir, "isdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(filepath.Join(dir, "isdir")).Load(); err == nil {
		t.Fatal("reading a directory succeeded")
	}
}

func TestFileStore_SaveErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Parent "directory" is a regular file: MkdirAll fails.
	if err := NewFileStore(filepath.Join(blocker, "state.json")).Save(nil); err == nil {
		t.Fatal("save under a regular file succeeded")
	}
	// Temp path is a directory: the write fails.
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewFileStore(target).Save(nil); err == nil {
		t.Fatal("save over a directory temp path succeeded")
	}
	// Target is a non-empty directory: the rename fails and the temp file is
	// cleaned up.
	target2 := filepath.Join(dir, "state2.json")
	if err := os.MkdirAll(filepath.Join(target2, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := NewFileStore(target2).Save(nil); err == nil {
		t.Fatal("rename over a non-empty directory succeeded")
	}
	if _, err := os.Stat(target2 + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file not cleaned up after a failed rename: %v", err)
	}
}

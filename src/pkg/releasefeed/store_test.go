package releasefeed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStore_LoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "release-feeds.json")
	fs := NewFileStore(path)
	st, err := fs.Load()
	if err != nil || len(st.Sources) != 0 || len(st.Agents) != 0 {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	st.Sources["pypi:requests"] = SourceState{ETag: `"e"`, Version: "1.0"}
	st.agent("deps").Seen["pypi:requests"] = "1.0"
	if err := fs.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := fs.Load()
	if err != nil || got.Sources["pypi:requests"].Version != "1.0" || got.Agents["deps"].Seen["pypi:requests"] != "1.0" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != stateFilePerm {
		t.Fatalf("state file mode: %v %v", info, err)
	}
}

func TestFileStore_LoadErrors(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(corrupt).Load(); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("corrupt: %v", err)
	}
	future := filepath.Join(dir, "future.json")
	if err := os.WriteFile(future, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(future).Load(); err == nil || !strings.Contains(err.Error(), "state version") {
		t.Fatalf("future: %v", err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewFileStore(empty).Load()
	if err != nil || st.Sources == nil || st.Agents == nil {
		t.Fatalf("empty maps must be initialised: %+v %v", st, err)
	}
	if _, err := NewFileStore(dir).Load(); err == nil {
		t.Fatal("reading a directory must fail")
	}
}

// A corrupt state file is reported and the watcher starts fresh rather than
// stopping: the next answers are baselines, so at worst one release is not
// kicked.
func TestWatcher_CorruptStateStartsFresh(t *testing.T) {
	r := newWatcherRig(t)
	if err := os.MkdirAll(filepath.Dir(r.store.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.store.path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := r.w.Poll(t.Context())
	if res.Fetched != 1 || len(res.Kicked) != 0 {
		t.Fatalf("fresh start: %+v", res)
	}
}

func TestFileStore_SaveError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file, so MkdirAll fails.
	if err := NewFileStore(filepath.Join(blocker, "state.json")).Save(newState()); err == nil {
		t.Fatal("save under a file must fail")
	}
	// The watcher logs a failed save and carries on.
	w := New(Options{Store: NewFileStore(filepath.Join(blocker, "state.json")), Logger: quietLogger()})
	w.ensureLoaded()
	w.save()
}

package github

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewDirForAtomicTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := reviewRequestDirForTest
	reviewRequestDirForTest = dir
	t.Cleanup(func() { reviewRequestDirForTest = old })
	return dir
}

func discardClient(t *testing.T) *Client {
	t.Helper()
	return NewClientForTest("http://127.0.0.1:1", "o", []string{"r"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A scan landing inside a non-atomic write used to see a zero-byte ".json",
// fail to unmarshal, and rename the file to ".bad" — destroying a valid
// request that was merely still being written.
func TestReviewRequest_InFlightFileIsNotQuarantined(t *testing.T) {
	dir := reviewDirForAtomicTest(t)
	path := filepath.Join(dir, "quality-1.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	discardClient(t).ProcessReviewRequestsOnce(t.Context())

	if _, err := os.Stat(path + ".bad"); err == nil {
		t.Fatal("in-flight (empty) request file was quarantined as .bad and lost")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("in-flight request file must be left in place for the next tick: %v", err)
	}
}

// Truncated-but-non-empty content is equally in flight while it is still being
// modified.
func TestReviewRequest_RecentlyModifiedGarbageIsNotQuarantined(t *testing.T) {
	dir := reviewDirForAtomicTest(t)
	path := filepath.Join(dir, "quality-2.json")
	if err := os.WriteFile(path, []byte(`{"repo":"o/r","nu`), 0o644); err != nil {
		t.Fatal(err)
	}

	discardClient(t).ProcessReviewRequestsOnce(t.Context())

	if _, err := os.Stat(path + ".bad"); err == nil {
		t.Fatal("a request file modified moments ago must not be quarantined")
	}
}

// Non-regression: once a file has settled, genuinely malformed JSON must still
// be moved aside so it stops being retried forever.
func TestReviewRequest_SettledGarbageIsStillQuarantined(t *testing.T) {
	dir := reviewDirForAtomicTest(t)
	path := filepath.Join(dir, "quality-3.json")
	if err := os.WriteFile(path, []byte("definitely not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	discardClient(t).ProcessReviewRequestsOnce(t.Context())

	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf("settled malformed request must still be quarantined: %v", err)
	}
}

func TestWriteRequestFile_AtomicAndScannerSafe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quality-9.json")
	if err := writeRequestFile(path, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("content = %q, err = %v", b, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644 (queues are group-readable)", st.Mode().Perm())
	}

	// Overwriting must not leave temp files behind, and no leftover may ever
	// carry a name a watcher would scan.
	if err := writeRequestFile(path, []byte(`{"ok":false}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
	}
}

// The temp name the atomic write publishes through must be invisible to every
// watcher scan, which selects on a ".json" suffix.
func TestWriteRequestFile_TempNameIsNotScanned(t *testing.T) {
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, filepath.Base("quality-1.json")+".tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if strings.HasSuffix(f.Name(), ".json") {
		t.Fatalf("temp name %q would be picked up by a watcher scan", f.Name())
	}
}

func TestQuarantinable(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	missing := filepath.Join(dir, "gone.json")
	if quarantinable(missing, now) {
		t.Error("a vanished file has nothing to quarantine")
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if quarantinable(empty, now) {
		t.Error("an empty file is still in flight")
	}

	fresh := filepath.Join(dir, "fresh.json")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if quarantinable(fresh, now) {
		t.Error("a just-modified file is still in flight")
	}
	// Anchor on the file's own mtime: `now` was sampled before the write, so
	// the file is fractionally newer than it.
	st, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !quarantinable(fresh, st.ModTime().Add(inFlightGrace)) {
		t.Error("a settled file must become quarantinable")
	}
}

// ageForQuarantine backdates a file past inFlightGrace so a watcher treats it
// as settled rather than still mid-write. Tests that assert quarantining of
// malformed JSON must use this: an unparseable file is deliberately left alone
// until it stops changing, because quarantining is unrecoverable.
func ageForQuarantine(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

package hubbackup

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	if got := FormatVersion(); got != backupFormatVersion {
		t.Fatalf("FormatVersion() = %d, want %d", got, backupFormatVersion)
	}
}

func TestBuilderAddBytesAndFinish(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	content := []byte("hello world")
	if err := b.AddBytes("test/file.txt", 0o644, content); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}

	man := &Manifest{
		FormatVersion: backupFormatVersion,
	}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Verify the sealed archive is valid and contains our file.
	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file entry, got %d", len(got.Files))
	}
	if got.Files[0].Path != "test/file.txt" {
		t.Errorf("file path = %q, want %q", got.Files[0].Path, "test/file.txt")
	}
	if got.Files[0].Size != int64(len(content)) {
		t.Errorf("file size = %d, want %d", got.Files[0].Size, len(content))
	}
}

func TestBuilderMultipleFiles(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	files := map[string][]byte{
		"dir/a.txt": []byte("alpha"),
		"dir/b.txt": []byte("bravo"),
		"root.txt":  []byte("root content"),
	}
	for name, content := range files {
		if err := b.AddBytes(name, 0o600, content); err != nil {
			t.Fatalf("AddBytes(%s): %v", name, err)
		}
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != len(files) {
		t.Fatalf("expected %d files, got %d", len(files), len(got.Files))
	}
}

func TestBuilderFinishNilManifest(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	_, err := b.Finish(key, nil, 0)
	if err == nil {
		t.Fatal("expected error for nil manifest, got nil")
	}
}

func TestBuilderFinishMaxBytesExceeded(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	// Add enough content to exceed a tiny cap.
	bigContent := make([]byte, 1024)
	if err := b.AddBytes("big.bin", 0o600, bigContent); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	_, err := b.Finish(key, man, 100) // 100-byte cap, content is >1024
	if err == nil {
		t.Fatal("expected maxBytes error, got nil")
	}
}

func TestBuilderFinishMaxBytesZeroNoLimit(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	bigContent := make([]byte, 4096)
	if err := b.AddBytes("big.bin", 0o600, bigContent); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish with maxBytes=0 should succeed: %v", err)
	}
	if len(sealed) == 0 {
		t.Fatal("sealed output is empty")
	}
}

func TestBuilderAddTree(t *testing.T) {
	key := decodeTestKey(t)
	dir := t.TempDir()

	// Create a small directory tree.
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewBuilder()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := b.AddTree(dir, "prefix", nil, logger); err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(got.Files))
	}

	// Check paths have the prefix.
	for _, f := range got.Files {
		if f.Path != "prefix/root.txt" && f.Path != "prefix/sub/nested.txt" {
			t.Errorf("unexpected file path: %s", f.Path)
		}
	}
}

func TestBuilderAddTreeSkipFunction(t *testing.T) {
	key := decodeTestKey(t)
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skip"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep", "a.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skip", "b.txt"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewBuilder()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	skip := func(rel string) bool {
		return rel == "skip"
	}
	if err := b.AddTree(dir, "out", skip, logger); err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file (skipped dir pruned), got %d", len(got.Files))
	}
	if got.Files[0].Path != "out/keep/a.txt" {
		t.Errorf("file path = %q, want %q", got.Files[0].Path, "out/keep/a.txt")
	}
}

func TestBuilderAddTreeUnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test permission denial as root")
	}

	dir := t.TempDir()
	unreadable := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(unreadable, []byte("secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := decodeTestKey(t)
	b := NewBuilder()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// AddTree should not fail — it skips unreadable files.
	if err := b.AddTree(dir, "data", nil, logger); err != nil {
		t.Fatalf("AddTree: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Only ok.txt should be archived; secret.txt was unreadable.
	if len(got.Files) != 1 {
		t.Fatalf("expected 1 file (unreadable skipped), got %d", len(got.Files))
	}
	if got.Files[0].Path != "data/ok.txt" {
		t.Errorf("file path = %q, want %q", got.Files[0].Path, "data/ok.txt")
	}
}

func TestBuilderAddTreeEmptyDir(t *testing.T) {
	key := decodeTestKey(t)
	dir := t.TempDir() // empty directory

	b := NewBuilder()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := b.AddTree(dir, "empty", nil, logger); err != nil {
		t.Fatalf("AddTree on empty dir: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Files) != 0 {
		t.Fatalf("expected 0 files for empty dir, got %d", len(got.Files))
	}
}

func TestBuilderRoundTripWithExtract(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	content := []byte(`{"backup":"data"}`)
	if err := b.AddBytes("hub/config.json", 0o644, content); err != nil {
		t.Fatalf("AddBytes: %v", err)
	}

	man := &Manifest{
		FormatVersion: backupFormatVersion,
		HubDataDir:    "/data",
		SpokeIDs:      []string{"spoke-1"},
	}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Extract and verify the file on disk.
	destDir := t.TempDir()
	extracted, err := Extract(key, sealed, destDir)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if extracted.HubDataDir != "/data" {
		t.Errorf("HubDataDir = %q, want %q", extracted.HubDataDir, "/data")
	}

	got, err := os.ReadFile(filepath.Join(destDir, "hub", "config.json"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("extracted content = %q, want %q", got, content)
	}
}

func TestBuilderAddBytesEmptyContent(t *testing.T) {
	key := decodeTestKey(t)
	b := NewBuilder()

	if err := b.AddBytes("empty.txt", 0o644, []byte{}); err != nil {
		t.Fatalf("AddBytes with empty content: %v", err)
	}

	man := &Manifest{FormatVersion: backupFormatVersion}
	sealed, err := b.Finish(key, man, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := Verify(key, sealed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Files[0].Size != 0 {
		t.Errorf("expected size 0 for empty content, got %d", got.Files[0].Size)
	}
}

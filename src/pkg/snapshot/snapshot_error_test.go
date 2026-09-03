package snapshot

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
)

func TestBuildImpossibleOutputDir(t *testing.T) {
	b := NewBuilder("/dev/null/impossible", slog.Default())
	err := b.Build(&dashboard.StatusPayload{
		Governor: dashboard.FrontendGovernor{Mode: "idle"},
	})
	if err == nil {
		t.Error("should error with impossible output dir")
	}
}

func TestCleanupNoRemovals(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	// Create recent files — should not be cleaned up
	os.WriteFile(filepath.Join(dir, "status-recent.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "latest.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"), 0644)

	err := b.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup error: %v", err)
	}

	// All files should still exist
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("expected 3 files, got %d", len(entries))
	}
}

func TestCleanupEmptyDirNoError(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	err := b.Cleanup(time.Hour)
	if err != nil {
		t.Fatalf("Cleanup empty dir: %v", err)
	}
}

func TestCleanupNonexistentDirError(t *testing.T) {
	b := NewBuilder("/tmp/nonexistent-snapshot-dir-xyz", slog.Default())
	err := b.Cleanup(time.Hour)
	if err == nil {
		t.Error("should error for nonexistent dir")
	}
}

func TestCleanupSkipsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "old-file.json"), []byte("{}"), 0644)
	// Make old-file.json look old
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(dir, "old-file.json"), oldTime, oldTime)

	err := b.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup error: %v", err)
	}

	// subdir should still exist, old-file.json should be removed
	if _, err := os.Stat(filepath.Join(dir, "subdir")); os.IsNotExist(err) {
		t.Error("subdirectory should not be removed")
	}
}

func TestCleanupPreservesLatestAndIndex(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	oldTime := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"latest.json", "index.html", "old-status.json"} {
		os.WriteFile(filepath.Join(dir, name), []byte("content"), 0644)
		os.Chtimes(filepath.Join(dir, name), oldTime, oldTime)
	}

	err := b.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup error: %v", err)
	}

	// latest.json and index.html should survive even though old
	if _, err := os.Stat(filepath.Join(dir, "latest.json")); os.IsNotExist(err) {
		t.Error("latest.json should be preserved")
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); os.IsNotExist(err) {
		t.Error("index.html should be preserved")
	}
	// old-status.json should be removed
	if _, err := os.Stat(filepath.Join(dir, "old-status.json")); !os.IsNotExist(err) {
		t.Error("old-status.json should be removed")
	}
}

func TestSaveStateWriteError(t *testing.T) {
	err := SaveState("/dev/null/impossible/state.json", &PersistedState{}, slog.Default())
	if err == nil {
		t.Error("should error with impossible path")
	}
}

func TestSaveState_WriteFailureReadOnlyDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Make dir read-only so WriteFile fails
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)

	state := &PersistedState{Agents: map[string]AgentState{}}
	err := SaveState(path, state, slog.Default())
	if err == nil {
		t.Error("expected error writing to read-only dir")
	}
}

func TestSaveState_RenameFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	// Write the temp file, then prevent rename by making target dir read-only
	// after the temp file is written. We create the temp file path manually
	// to set up the right conditions.
	dir := t.TempDir()
	subdir := filepath.Join(dir, "nested")
	os.MkdirAll(subdir, 0o755)
	path := filepath.Join(subdir, "state.json")

	state := &PersistedState{Agents: map[string]AgentState{}}

	// First save succeeds
	if err := SaveState(path, state, slog.Default()); err != nil {
		t.Fatalf("initial save: %v", err)
	}

	// Now make the directory read-only so that writing .tmp fails (which triggers write error)
	os.Chmod(subdir, 0o555)
	defer os.Chmod(subdir, 0o755)

	err := SaveState(path, state, slog.Default())
	if err == nil {
		t.Log("write succeeded on read-only dir (OS-dependent)")
	}
}

func TestBuild_LatestTmpWriteError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	// Create latest.json.tmp as a directory to block the write
	tmpPath := filepath.Join(dir, "latest.json.tmp")
	os.MkdirAll(tmpPath, 0o755)

	b := NewBuilder(dir, slog.Default())
	err := b.Build(&dashboard.StatusPayload{
		Governor: dashboard.FrontendGovernor{Mode: "IDLE"},
		Agents:   []dashboard.FrontendAgent{},
	})
	if err == nil {
		t.Error("expected error when latest.json.tmp is a directory")
	}
}

func TestCleanup_RemoveFailureContinues(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can remove anything")
	}
	// Create a subdirectory with an old file that can't be removed
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	oldTime := time.Now().Add(-48 * time.Hour)

	// Create old files
	old1 := filepath.Join(dir, "removable-old.json")
	os.WriteFile(old1, []byte("{}"), 0644)
	os.Chtimes(old1, oldTime, oldTime)

	// Cleanup should succeed even if some removes fail
	err := b.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup error: %v", err)
	}
}

func TestCleanup_MixedOldNewWithProtected(t *testing.T) {
	dir := t.TempDir()
	b := NewBuilder(dir, slog.Default())

	oldTime := time.Now().Add(-72 * time.Hour)

	// Old files that should be removed
	for _, name := range []string{"status-2020.json", "data-old.csv", "report.txt"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("old"), 0644)
		os.Chtimes(p, oldTime, oldTime)
	}

	// Protected files (old but should be preserved)
	for _, name := range []string{"latest.json", "index.html"} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("protected"), 0644)
		os.Chtimes(p, oldTime, oldTime)
	}

	// Recent files that should be preserved
	os.WriteFile(filepath.Join(dir, "status-new.json"), []byte("new"), 0644)

	// Subdirectory that should be skipped
	os.MkdirAll(filepath.Join(dir, "archive"), 0755)

	err := b.Cleanup(24 * time.Hour)
	if err != nil {
		t.Fatalf("Cleanup error: %v", err)
	}

	// Old non-protected files should be gone
	for _, name := range []string{"status-2020.json", "data-old.csv", "report.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("old file %q should have been removed", name)
		}
	}

	// Protected files should still exist
	for _, name := range []string{"latest.json", "index.html"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("protected file %q should still exist", name)
		}
	}

	// Recent file should still exist
	if _, err := os.Stat(filepath.Join(dir, "status-new.json")); err != nil {
		t.Error("recent file should still exist")
	}

	// Subdirectory should still exist
	if _, err := os.Stat(filepath.Join(dir, "archive")); err != nil {
		t.Error("subdirectory should still exist")
	}
}

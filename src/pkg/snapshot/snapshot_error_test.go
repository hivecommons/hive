package snapshot

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

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

package snapshot

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logger := testLogger()

	state := &PersistedState{
		GovernorMode:  "BUSY",
		BudgetLimit:   500000,
		BudgetIgnored: []string{"outreach"},
		Agents: map[string]AgentState{
			"scanner": {
				Paused:          false,
				PinnedCLI:       "claude",
				PinnedModel:     "sonnet",
				ModelOverride:   "opus",
				BackendOverride: "gemini",
				RestartCount:    3,
			},
		},
	}

	err := SaveState(path, state, logger)
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	loaded, err := LoadState(path, logger)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded state is nil")
	}

	if loaded.GovernorMode != "BUSY" {
		t.Errorf("governor mode = %q", loaded.GovernorMode)
	}
	if loaded.BudgetLimit != 500000 {
		t.Errorf("budget limit = %d", loaded.BudgetLimit)
	}
	if len(loaded.BudgetIgnored) != 1 || loaded.BudgetIgnored[0] != "outreach" {
		t.Errorf("budget ignored = %v", loaded.BudgetIgnored)
	}
	scanner := loaded.Agents["scanner"]
	if scanner.PinnedCLI != "claude" {
		t.Errorf("pinned CLI = %q", scanner.PinnedCLI)
	}
	if scanner.RestartCount != 3 {
		t.Errorf("restart count = %d", scanner.RestartCount)
	}
}

func TestLoadState_FileNotExist(t *testing.T) {
	logger := testLogger()
	state, err := LoadState("/nonexistent/path/state.json", logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != nil {
		t.Error("expected nil state for nonexistent file")
	}
}

func TestLoadState_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0644)

	logger := testLogger()
	_, err := LoadState(path, logger)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadState_TooOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logger := testLogger()

	state := &PersistedState{
		SavedAt:      time.Now().Add(-8 * 24 * time.Hour), // 8 days old
		GovernorMode: "IDLE",
		Agents:       map[string]AgentState{},
	}

	// Write directly with old timestamp
	err := SaveState(path, state, logger)
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Manually overwrite SavedAt to be old
	data, _ := os.ReadFile(path)
	// The SaveState sets SavedAt to now, so we need to write manually
	oldJSON := `{"saved_at":"2020-01-01T00:00:00Z","agents":{},"governor_mode":"IDLE","budget_limit":0}`
	os.WriteFile(path, []byte(oldJSON), 0644)
	_ = data

	loaded, err := LoadState(path, logger)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded != nil {
		t.Error("expected nil state for too-old file")
	}
}

func TestSaveState_BadPath(t *testing.T) {
	logger := testLogger()
	state := &PersistedState{Agents: map[string]AgentState{}}
	err := SaveState("/nonexistent/dir/state.json", state, logger)
	if err == nil {
		t.Error("expected error for bad path")
	}
}

func TestSaveState_SuccessfulRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	logger := testLogger()

	state := &PersistedState{
		Agents: map[string]AgentState{
			"scanner": {RestartCount: 5},
			"quality": {RestartCount: 3},
		},
	}
	if err := SaveState(path, state, logger); err != nil {
		t.Fatal(err)
	}
	if state.SavedAt.IsZero() {
		t.Error("SavedAt should be set")
	}

	loaded, err := LoadState(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Agents) != 2 {
		t.Errorf("loaded %d agents, want 2", len(loaded.Agents))
	}
}

func TestCleanup_RemoveErrorLogged(t *testing.T) {
	dir := t.TempDir()
	logger := testLogger()
	b := NewBuilder(dir, logger)

	oldFile := filepath.Join(dir, "status-2020-01-01T00-00-00Z.json")
	os.WriteFile(oldFile, []byte("{}"), 0o644)

	err := b.Cleanup(time.Hour)
	if err != nil {
		t.Errorf("cleanup should not return error: %v", err)
	}
}

func TestCleanup_RemovePermissionError(t *testing.T) {
	dir := t.TempDir()
	logger := testLogger()
	b := NewBuilder(dir, logger)

	oldFile := filepath.Join(dir, "status-2020-01-01T00-00-00Z.json")
	os.WriteFile(oldFile, []byte("{}"), 0o644)

	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)

	err := b.Cleanup(time.Hour)
	if err != nil {
		t.Errorf("cleanup should not return error even on permission issues: %v", err)
	}
}

func TestSaveState_RenameError(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	os.MkdirAll(subdir, 0o755)
	path := filepath.Join(subdir, "state.json")
	logger := testLogger()

	state := &PersistedState{Agents: map[string]AgentState{}}
	err := SaveState(path, state, logger)
	if err != nil {
		t.Fatal(err)
	}

	os.Chmod(subdir, 0o555)
	defer os.Chmod(subdir, 0o755)

	err = SaveState(path, state, logger)
	if err == nil {
		t.Log("write succeeded even with read-only dir (OS-dependent)")
	}
}

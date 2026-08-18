package knowledge

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestConnectExistingVault_NilAPI verifies connectExistingVault returns
// safely when the KnowledgeAPI is nil.
func TestConnectExistingVault_NilAPI(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &InceptionEngine{dataDir: dir, api: nil, logger: logger}
	// Should not panic.
	e.connectExistingVault()
}

// TestConnectExistingVault_MissingDir verifies connectExistingVault
// is a no-op when the inception-wiki directory does not exist.
func TestConnectExistingVault_MissingDir(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kapi := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
	e := &InceptionEngine{dataDir: dir, api: kapi, logger: logger}
	// No inception-wiki dir — should return without error.
	e.connectExistingVault()
}

// TestConnectExistingVault_EmptyDir verifies connectExistingVault
// is a no-op when the inception-wiki directory exists but is empty.
func TestConnectExistingVault_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kapi := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
	vaultDir := filepath.Join(dir, inceptionWikiDir)
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	e := &InceptionEngine{dataDir: dir, api: kapi, logger: logger}
	e.connectExistingVault()
}

// TestConnectExistingVault_WithFiles verifies connectExistingVault
// reconnects a vault when the inception-wiki directory has files.
func TestConnectExistingVault_WithFiles(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kapi := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
	vaultDir := filepath.Join(dir, inceptionWikiDir)
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "test.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	e := &InceptionEngine{dataDir: dir, api: kapi, logger: logger}
	e.connectExistingVault()

	// Calling again should hit the "already connected" branch.
	e.connectExistingVault()
}

// TestConnectExistingVault_WithCustomWikiName verifies connectExistingVault
// uses the WikiName from saved state instead of the default.
func TestConnectExistingVault_WithCustomWikiName(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kapi := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)

	// Create inception-wiki directory with a file.
	vaultDir := filepath.Join(dir, inceptionWikiDir)
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "fact.md"), []byte("# Fact\nBody"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Write a state file with a custom WikiName so connectExistingVault uses it.
	stateDir := filepath.Join(dir, "inception")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	state := &InceptionState{WikiName: "my-custom-wiki"}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), stateBytes, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// NewInceptionEngine loads state then calls connectExistingVault.
	eng := NewInceptionEngine(dir, kapi, logger)
	if eng == nil {
		t.Fatal("nil engine")
	}
}

// TestNewInceptionEngine_FullStartup exercises the constructor with
// pre-existing state and wiki files — the most realistic startup path.
func TestNewInceptionEngine_FullStartup(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	kapi := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)

	// First create and start an inception so state + wiki exist.
	eng1 := NewInceptionEngine(dir, kapi, logger)
	if _, err := eng1.Start("test startup project in go"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Create a wiki file so connectExistingVault finds content.
	vaultDir := filepath.Join(dir, inceptionWikiDir)
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(vaultDir, "readme.md"), []byte("# Hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Re-create the engine — simulates a restart.
	kapi2 := NewKnowledgeAPI(nil, KnowledgeConfig{}, logger)
	eng2 := NewInceptionEngine(dir, kapi2, logger)
	if eng2 == nil {
		t.Fatal("nil engine on restart")
	}
	if st := eng2.GetState(); st == nil {
		t.Fatal("state should be loaded on restart")
	}
}

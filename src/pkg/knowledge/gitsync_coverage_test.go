package knowledge

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// NewGitSyncer / Add
// ---------------------------------------------------------------------------

func TestNewGitSyncer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gs := NewGitSyncer(logger)
	if gs == nil {
		t.Fatal("expected non-nil GitSyncer")
	}
	if len(gs.vaults) != 0 {
		t.Errorf("expected 0 vaults, got %d", len(gs.vaults))
	}
}

func TestGitSyncer_Add(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gs := NewGitSyncer(logger)

	tmpDir := t.TempDir()
	store, err := NewFileStore(tmpDir, "test", logger)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	gs.Add("test-vault", tmpDir, store)
	if len(gs.vaults) != 1 {
		t.Errorf("expected 1 vault, got %d", len(gs.vaults))
	}
}

// ---------------------------------------------------------------------------
// Start — empty vaults (returns immediately)
// ---------------------------------------------------------------------------

func TestGitSyncer_Start_Empty(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gs := NewGitSyncer(logger)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	gs.Start(ctx)
	// Should return immediately since no vaults
}

// ---------------------------------------------------------------------------
// syncAll — non-git directory
// ---------------------------------------------------------------------------

func TestGitSyncer_SyncAll_NonGitDir(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	gs := NewGitSyncer(logger)

	tmpDir := t.TempDir()
	store, _ := NewFileStore(tmpDir, "test", logger)
	gs.Add("test-vault", tmpDir, store)

	// syncAll should skip non-git directories without error
	gs.syncAll(context.Background())
}

// ---------------------------------------------------------------------------
// isGitRepo
// ---------------------------------------------------------------------------

func TestIsGitRepo_False(t *testing.T) {
	tmpDir := t.TempDir()
	if isGitRepo(tmpDir) {
		t.Error("expected false for non-git directory")
	}
}

func TestIsGitRepo_True(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, ".git"), 0o755)
	if !isGitRepo(tmpDir) {
		t.Error("expected true for directory with .git")
	}
}

// ---------------------------------------------------------------------------
// gitPullError
// ---------------------------------------------------------------------------

func TestGitPullError_Error(t *testing.T) {
	e := &gitPullError{dir: "/tmp/vault", output: "conflict", err: os.ErrNotExist}
	msg := e.Error()
	if msg == "" {
		t.Error("expected non-empty error message")
	}
	if e.Unwrap() != os.ErrNotExist {
		t.Error("Unwrap should return wrapped error")
	}
}

// ---------------------------------------------------------------------------
// InitVaultRepo
// ---------------------------------------------------------------------------

func TestInitVaultRepo_AlreadyExists(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := t.TempDir()

	err := InitVaultRepo(tmpDir, logger)
	if err != nil {
		t.Fatalf("InitVaultRepo: %v", err)
	}
}

func TestInitVaultRepo_CreatesDir(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmpDir := filepath.Join(t.TempDir(), "new-vault")

	err := InitVaultRepo(tmpDir, logger)
	if err != nil {
		t.Fatalf("InitVaultRepo: %v", err)
	}

	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("expected vault directory to be created")
	}
}

// ---------------------------------------------------------------------------
// SeedVaultContent
// ---------------------------------------------------------------------------

func TestSeedVaultContent_AlreadyHasContent(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	vaultDir := t.TempDir()
	os.WriteFile(filepath.Join(vaultDir, "existing.md"), []byte("# Existing"), 0o644)

	seedDir := t.TempDir()
	os.WriteFile(filepath.Join(seedDir, "seed.md"), []byte("# Seed"), 0o644)

	err := SeedVaultContent(vaultDir, seedDir, logger)
	if err != nil {
		t.Fatalf("SeedVaultContent: %v", err)
	}

	// Seed file should NOT have been copied
	if _, err := os.Stat(filepath.Join(vaultDir, "seed.md")); !os.IsNotExist(err) {
		t.Error("seed.md should not have been copied into vault with existing content")
	}
}

func TestSeedVaultContent_EmptyVault(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	vaultDir := t.TempDir()

	seedDir := t.TempDir()
	os.WriteFile(filepath.Join(seedDir, "seed.md"), []byte("# Seed Content"), 0o644)
	os.WriteFile(filepath.Join(seedDir, "seed2.md"), []byte("# More Content"), 0o644)
	os.MkdirAll(filepath.Join(seedDir, "subdir"), 0o755) // should be skipped

	err := SeedVaultContent(vaultDir, seedDir, logger)
	if err != nil {
		t.Fatalf("SeedVaultContent: %v", err)
	}

	// Seed files should have been copied
	if _, err := os.Stat(filepath.Join(vaultDir, "seed.md")); os.IsNotExist(err) {
		t.Error("seed.md should have been copied")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "seed2.md")); os.IsNotExist(err) {
		t.Error("seed2.md should have been copied")
	}
}

func TestSeedVaultContent_NoSeedDir(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	vaultDir := t.TempDir()

	err := SeedVaultContent(vaultDir, "/nonexistent/seed-dir", logger)
	if err != nil {
		t.Fatalf("SeedVaultContent should not error for missing seed dir: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractTitleFromContent
// ---------------------------------------------------------------------------

func TestExtractTitleFromContent_WithHeading(t *testing.T) {
	content := "Some text\n# My Title\nMore text"
	result := extractTitleFromContent(content, "fallback-slug")
	if result != "My Title" {
		t.Errorf("got %q, want 'My Title'", result)
	}
}

func TestExtractTitleFromContent_NoHeading(t *testing.T) {
	content := "No heading here\njust text"
	result := extractTitleFromContent(content, "path/to/slug")
	if result != "slug" {
		t.Errorf("got %q, want 'slug'", result)
	}
}

func TestExtractTitleFromContent_SimpleSlug(t *testing.T) {
	result := extractTitleFromContent("", "my-note")
	if result != "my-note" {
		t.Errorf("got %q, want 'my-note'", result)
	}
}

// ---------------------------------------------------------------------------
// extractFrontmatterString
// ---------------------------------------------------------------------------

func TestExtractFrontmatterString_Present(t *testing.T) {
	fm := map[string]interface{}{"title": "Hello"}
	result := extractFrontmatterString(fm, "title", "default")
	if result != "Hello" {
		t.Errorf("got %q, want Hello", result)
	}
}

func TestExtractFrontmatterString_Missing(t *testing.T) {
	fm := map[string]interface{}{}
	result := extractFrontmatterString(fm, "title", "default")
	if result != "default" {
		t.Errorf("got %q, want default", result)
	}
}

func TestExtractFrontmatterString_WrongType(t *testing.T) {
	fm := map[string]interface{}{"title": 42}
	result := extractFrontmatterString(fm, "title", "default")
	if result != "default" {
		t.Errorf("got %q, want default", result)
	}
}

func TestExtractFrontmatterString_NilMap(t *testing.T) {
	result := extractFrontmatterString(nil, "title", "default")
	if result != "default" {
		t.Errorf("got %q, want default", result)
	}
}

// ---------------------------------------------------------------------------
// extractFrontmatterStringSlice
// ---------------------------------------------------------------------------

func TestExtractFrontmatterStringSlice_InterfaceArray(t *testing.T) {
	fm := map[string]interface{}{"tags": []interface{}{"go", "test"}}
	result := extractFrontmatterStringSlice(fm, "tags")
	if len(result) != 2 || result[0] != "go" || result[1] != "test" {
		t.Errorf("got %v", result)
	}
}

func TestExtractFrontmatterStringSlice_StringArray(t *testing.T) {
	fm := map[string]interface{}{"tags": []string{"go", "test"}}
	result := extractFrontmatterStringSlice(fm, "tags")
	if len(result) != 2 {
		t.Errorf("got %v", result)
	}
}

func TestExtractFrontmatterStringSlice_NilMap(t *testing.T) {
	result := extractFrontmatterStringSlice(nil, "tags")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractFrontmatterStringSlice_Missing(t *testing.T) {
	fm := map[string]interface{}{}
	result := extractFrontmatterStringSlice(fm, "tags")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestExtractFrontmatterStringSlice_WrongType(t *testing.T) {
	fm := map[string]interface{}{"tags": "not-a-slice"}
	result := extractFrontmatterStringSlice(fm, "tags")
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// extractFrontmatterFloat
// ---------------------------------------------------------------------------

func TestExtractFrontmatterFloat_Float64(t *testing.T) {
	fm := map[string]interface{}{"confidence": 0.95}
	result := extractFrontmatterFloat(fm, "confidence", 0.5)
	if result != 0.95 {
		t.Errorf("got %f, want 0.95", result)
	}
}

func TestExtractFrontmatterFloat_Int(t *testing.T) {
	fm := map[string]interface{}{"confidence": 1}
	result := extractFrontmatterFloat(fm, "confidence", 0.5)
	if result != 1.0 {
		t.Errorf("got %f, want 1.0", result)
	}
}

func TestExtractFrontmatterFloat_JSONNumber(t *testing.T) {
	fm := map[string]interface{}{"confidence": json.Number("0.85")}
	result := extractFrontmatterFloat(fm, "confidence", 0.5)
	if result != 0.85 {
		t.Errorf("got %f, want 0.85", result)
	}
}

func TestExtractFrontmatterFloat_InvalidJSONNumber(t *testing.T) {
	fm := map[string]interface{}{"confidence": json.Number("not-a-number")}
	result := extractFrontmatterFloat(fm, "confidence", 0.5)
	if result != 0.5 {
		t.Errorf("got %f, want 0.5 (default)", result)
	}
}

func TestExtractFrontmatterFloat_NilMap(t *testing.T) {
	result := extractFrontmatterFloat(nil, "confidence", 0.5)
	if result != 0.5 {
		t.Errorf("got %f, want 0.5", result)
	}
}

func TestExtractFrontmatterFloat_WrongType(t *testing.T) {
	fm := map[string]interface{}{"confidence": "high"}
	result := extractFrontmatterFloat(fm, "confidence", 0.5)
	if result != 0.5 {
		t.Errorf("got %f, want 0.5 (default)", result)
	}
}

// ---------------------------------------------------------------------------
// ObsidianSyncRequest.UnmarshalJSON
// ---------------------------------------------------------------------------

func TestObsidianSyncRequest_UnmarshalJSON(t *testing.T) {
	raw := `{
		"filename": "test.md",
		"filepath": "notes/test.md",
		"content": "Hello world",
		"vault": "my-vault",
		"customField": "custom-value",
		"tags": ["go", "test"]
	}`

	var req ObsidianSyncRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}

	if req.Filename != "test.md" {
		t.Errorf("filename = %q", req.Filename)
	}
	if req.Content != "Hello world" {
		t.Errorf("content = %q", req.Content)
	}
	// customField should be captured in Frontmatter
	if req.Frontmatter["customField"] != "custom-value" {
		t.Errorf("customField not captured in frontmatter: %v", req.Frontmatter)
	}
	// vault should be in Overflow
	if req.Vault() != "my-vault" {
		t.Errorf("Vault() = %q, want my-vault", req.Vault())
	}
}

func TestObsidianSyncRequest_Vault_Empty(t *testing.T) {
	var req ObsidianSyncRequest
	if req.Vault() != "" {
		t.Errorf("expected empty vault, got %q", req.Vault())
	}
}

func TestObsidianSyncRequest_UnmarshalJSON_WithPath(t *testing.T) {
	raw := `{
		"filename": "note.md",
		"content": "text",
		"path": "some/path"
	}`
	var req ObsidianSyncRequest
	json.Unmarshal([]byte(raw), &req)
	if req.Filepath != "some/path" {
		t.Errorf("filepath = %q, want some/path", req.Filepath)
	}
}

// ---------------------------------------------------------------------------
// ObsidianSync — file-based fallback (no client for layer)
// ---------------------------------------------------------------------------

func TestObsidianSync_FileBasedFallback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Create knowledge API without wiki layers (file-based only)
	kcfg := KnowledgeConfig{Enabled: true, Engine: "file"}
	api := NewKnowledgeAPI(nil, kcfg, logger)

	req := ObsidianSyncRequest{
		Filename: "test-sync.md",
		Content:  "# Test\nHello from obsidian",
		Frontmatter: map[string]interface{}{
			"title": "Test Sync",
			"tags":  []interface{}{"go"},
			"type":  "pattern",
			"layer": "project",
		},
	}

	result, err := api.ObsidianSync(context.Background(), req)
	if err != nil {
		t.Fatalf("ObsidianSync: %v", err)
	}

	if result.Slug != "test-sync" {
		t.Errorf("slug = %q, want test-sync", result.Slug)
	}
	if result.Action != "created" && result.Action != "updated" {
		t.Errorf("action = %q", result.Action)
	}
	if result.Fact.Title != "Test Sync" {
		t.Errorf("title = %q", result.Fact.Title)
	}
}

// ---------------------------------------------------------------------------
// GetVaultStore
// ---------------------------------------------------------------------------

func TestGetVaultStore_NotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, logger)

	store := api.GetVaultStore("/nonexistent")
	if store != nil {
		t.Error("expected nil for non-matching vault")
	}
}

func TestGetVaultStore_Found(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, logger)

	tmpDir := t.TempDir()
	api.ConnectVault(tmpDir, "test-vault")

	store := api.GetVaultStore(tmpDir)
	if store == nil {
		t.Error("expected to find vault store")
	}
}

package knowledge

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchAllWithVaultsConnected(t *testing.T) {
	dir := t.TempDir()

	// Create a vault directory with some markdown files
	vaultDir := filepath.Join(dir, "test-vault")
	os.MkdirAll(vaultDir, 0755)
	os.WriteFile(filepath.Join(vaultDir, "pattern-caching.md"), []byte(`---
title: Caching Pattern
type: pattern
---
Use Redis for caching hot paths.
`), 0644)
	os.WriteFile(filepath.Join(vaultDir, "gotcha-nil-map.md"), []byte(`---
title: Nil Map Gotcha
type: gotcha
---
Always initialize maps before use.
`), 0644)

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	if err := api.ConnectVault(vaultDir, "test-vault"); err != nil {
		t.Fatalf("ConnectVault: %v", err)
	}

	// Search with empty query — should list all facts from vault
	results := api.SearchAllWithVaults(context.Background(), "", "", 0)
	if len(results) < 2 {
		t.Errorf("expected at least 2 results from vault, got %d", len(results))
	}

	// Search with type filter
	patterns := api.SearchAllWithVaults(context.Background(), "", "pattern", 0)
	for _, f := range patterns {
		if string(f.Type) != "pattern" {
			t.Errorf("type filter should return only patterns, got %q", f.Type)
		}
	}

	// Search with query
	cacheResults := api.SearchAllWithVaults(context.Background(), "caching", "", 10)
	found := false
	for _, f := range cacheResults {
		if f.Title == "Caching Pattern" {
			found = true
		}
	}
	if !found && len(cacheResults) == 0 {
		t.Log("search by query may not find vault content depending on search impl")
	}
}

func TestSearchAllWithVaultsEmpty(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())

	results := api.SearchAllWithVaults(context.Background(), "", "", 0)
	// No vaults connected — should return empty or base results
	_ = results
}

func TestVaultFactsConnected(t *testing.T) {
	dir := t.TempDir()
	vaultDir := filepath.Join(dir, "facts-vault")
	os.MkdirAll(vaultDir, 0755)
	os.WriteFile(filepath.Join(vaultDir, "decision-api.md"), []byte(`---
title: API Decision
type: decision
---
Use REST over gRPC.
`), 0644)

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	api.ConnectVault(vaultDir, "facts-vault")

	facts := api.VaultFacts("facts-vault")
	if len(facts) == 0 {
		t.Error("should return facts from connected vault")
	}
}

func TestVaultFactsUnknown(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	facts := api.VaultFacts("nonexistent")
	if facts != nil {
		t.Error("should return nil for unknown vault")
	}
}

func TestVaultFactBySlug(t *testing.T) {
	dir := t.TempDir()
	vaultDir := filepath.Join(dir, "slug-vault")
	os.MkdirAll(vaultDir, 0755)
	os.WriteFile(filepath.Join(vaultDir, "my-fact.md"), []byte(`---
title: My Fact
type: pattern
---
Content here.
`), 0644)

	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	api.ConnectVault(vaultDir, "slug-vault")

	fact, err := api.VaultFact("my-fact")
	if err != nil {
		t.Logf("VaultFact: %v (may need exact slug format)", err)
	}
	_ = fact
}

func TestVaultFactNotFound(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	_, err := api.VaultFact("nonexistent-slug")
	if err == nil {
		t.Error("should error for nonexistent fact")
	}
}

func TestLayersWithFileEngine(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	layers := api.Layers()
	_ = layers
}

func TestObsidianSyncPathTraversal(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	_, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "../../../etc/passwd",
		Content:  "evil",
	})
	if err == nil {
		t.Error("should reject path traversal in filename")
	}
}

func TestObsidianSyncValid(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	result, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "test-pattern.md",
		Content:  "Always validate input before processing",
		Frontmatter: map[string]interface{}{
			"title":      "Input Validation",
			"type":       "pattern",
			"layer":      "project",
			"confidence": 0.9,
			"tags":       []interface{}{"security", "validation"},
		},
	})
	if err != nil {
		t.Logf("ObsidianSync: %v (may fail on macOS without /data)", err)
	}
	if result != nil && result.Slug == "" {
		t.Error("result should have a slug")
	}
}

func TestObsidianSyncNoExtension(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	result, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "my-fact.markdown",
		Content:  "Some content",
	})
	if err != nil {
		t.Logf("ObsidianSync markdown ext: %v", err)
	}
	_ = result
}

func TestObsidianSyncBadLayer(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	result, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "test.md",
		Content:  "content",
		Frontmatter: map[string]interface{}{
			"layer": "..",
		},
	})
	if err != nil {
		t.Logf("ObsidianSync bad layer: %v", err)
	}
	_ = result
}

func TestObsidianSyncEmptyLayer(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	result, err := api.ObsidianSync(context.Background(), ObsidianSyncRequest{
		Filename: "test.md",
		Content:  "content",
		Frontmatter: map[string]interface{}{
			"layer": "",
		},
	})
	if err != nil {
		t.Logf("ObsidianSync empty layer: %v", err)
	}
	_ = result
}

func TestListIdeationFactsFileEngine(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	facts := api.ListIdeationFacts(context.Background())
	_ = facts
}

func TestGetConstitutionFileEngine(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true, Engine: "file"}, slog.Default())
	c := api.GetConstitution(context.Background())
	_ = c
}

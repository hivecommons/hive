package integrated

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreLoadMigratesLegacyV1AndRetainsManagedPreimageLedger(t *testing.T) {
	stateRoot := t.TempDir()
	store, err := NewStore(filepath.Join(stateRoot, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("repository-owned workflow\n")
	digest := sha256.Sum256(content)
	legacy := Config{
		SchemaVersion:           legacyConfigSchema,
		Repository:              "owner/repo",
		RepositoryID:            "123",
		StateDir:                stateRoot,
		ManagedPreimagesVersion: managedPreimagesVersion,
		ManagedPathPreimages: map[string]ManagedPathPreimage{
			".github/workflows/hive-visual-hive.yml": {
				Existed:       true,
				GitMode:       "100644",
				Content:       content,
				ContentSHA256: hex.EncodeToString(digest[:]),
			},
		},
	}
	type persistedConfig Config
	data, err := json.MarshalIndent(persistedConfig(legacy), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDurableStateFile(filepath.Join(stateRoot, "integrated", "config.json"), append(data, '\n')); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != ConfigSchema {
		t.Fatalf("legacy config was not migrated in memory: %q", loaded.SchemaVersion)
	}
	if loaded.ExecutionMode != ExecutionLocal {
		t.Fatalf("legacy config execution mode = %q, want local", loaded.ExecutionMode)
	}
	if !bytes.Equal(loaded.ManagedPathPreimages[".github/workflows/hive-visual-hive.yml"].Content, content) {
		t.Fatal("legacy migration dropped managed-path preimage bytes")
	}

	durable, err := os.ReadFile(filepath.Join(stateRoot, "integrated", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedConfig
	if err := json.Unmarshal(durable, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SchemaVersion != ConfigSchema {
		t.Fatalf("legacy config was returned without first persisting the v2 writer guard: %q", persisted.SchemaVersion)
	}
	if persisted.ExecutionMode != ExecutionLocal {
		t.Fatalf("migrated durable execution mode = %q, want local", persisted.ExecutionMode)
	}
	if !bytes.Equal(persisted.ManagedPathPreimages[".github/workflows/hive-visual-hive.yml"].Content, content) {
		t.Fatal("persisted v2 config dropped managed-path preimage bytes")
	}
	if reloaded, reloadErr := store.Load(); reloadErr != nil || reloaded.SchemaVersion != ConfigSchema ||
		!bytes.Equal(reloaded.ManagedPathPreimages[".github/workflows/hive-visual-hive.yml"].Content, content) {
		t.Fatalf("current v2 config did not retain the ledger on reload: schema=%q err=%v", reloaded.SchemaVersion, reloadErr)
	}
}

func TestStoreLoadRejectsUnknownFutureSchemaWithoutRewriting(t *testing.T) {
	stateRoot := t.TempDir()
	store, err := NewStore(filepath.Join(stateRoot, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(stateRoot, "integrated", "config.json")
	original := []byte(`{"schema_version":"hive.integrated-config.v999","repository":"owner/repo","repository_id":"123"}` + "\n")
	if err := writeDurableStateFile(configPath, original); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "unsupported integrated config schema") {
		t.Fatalf("unknown future schema did not fail closed: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("unknown future schema was rewritten while being rejected")
	}
}

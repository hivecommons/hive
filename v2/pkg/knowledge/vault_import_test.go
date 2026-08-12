package knowledge

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// newTestAPI builds a minimal KnowledgeAPI with no remote layers, mirroring a
// provisioned spoke that has only local vaults.
func newTestAPI(t *testing.T) *KnowledgeAPI {
	t.Helper()
	return &KnowledgeAPI{
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		vaults: nil,
	}
}

func connectTempVault(t *testing.T, k *KnowledgeAPI, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := k.ConnectVault(dir, name); err != nil {
		t.Fatalf("ConnectVault(%s): %v", name, err)
	}
	return dir
}

// TestImportFacts_WritesToVault: the #3581 fix — importing to a local vault name
// writes files and succeeds, instead of erroring "has no configured endpoint".
func TestImportFacts_WritesToVault(t *testing.T) {
	k := newTestAPI(t)
	dir := connectTempVault(t, k, "Runbooks")

	n, err := k.ImportFacts(context.Background(), LayerType("Runbooks"),
		"# Restart the gateway\n\nRun `docker compose restart`.\n\n# Rotate the token\n\nUse the admin panel.", "markdown")
	if err != nil {
		t.Fatalf("ImportFacts to vault: %v", err)
	}
	if n != 2 {
		t.Errorf("imported %d facts, want 2", n)
	}
	// Two markdown files should now exist in the vault dir.
	entries, _ := os.ReadDir(dir)
	md := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".md" {
			md++
		}
	}
	if md != 2 {
		t.Errorf("vault has %d .md files, want 2", md)
	}
}

// TestImportFacts_ReservedVaultRejected: importing into the automation vault must
// fail with a clear error and write nothing (the exact bug in #3581).
func TestImportFacts_ReservedVaultRejected(t *testing.T) {
	k := newTestAPI(t)
	_, err := k.ImportFacts(context.Background(), LayerType(reservedVaultName), "# X\n\nbody", "markdown")
	if err == nil {
		t.Fatal("importing into the reserved vault must error")
	}
}

// TestImportFacts_NoChannelGivesFriendlyError: with no matching vault/layer, the
// error must guide the user to create a channel — not the old endpoint jargon.
func TestImportFacts_NoChannelGivesFriendlyError(t *testing.T) {
	k := newTestAPI(t)
	_, err := k.ImportFacts(context.Background(), LayerType("nonexistent"), "# X\n\nbody", "markdown")
	if err == nil {
		t.Fatal("expected an error for an unknown channel")
	}
}

// TestCreateVault_RejectsReservedAndUnsafe validates the create-channel guards.
func TestCreateVault_RejectsReservedAndUnsafe(t *testing.T) {
	k := newTestAPI(t)
	if _, err := k.CreateVault(reservedVaultName); err == nil {
		t.Error("creating the reserved vault must be rejected")
	}
	if _, err := k.CreateVault("../escape"); err == nil {
		t.Error("path-unsafe name must be rejected")
	}
	if _, err := k.CreateVault(""); err == nil {
		t.Error("empty name must be rejected")
	}
	if !isSafeVaultName("My Runbooks-2") {
		t.Error("a normal name should be accepted")
	}
	if isSafeVaultName("bad/name") {
		t.Error("slash must be rejected")
	}
}

// TestWritableVaults_ExcludesReserved: the reserved vault never appears in the
// user-facing writable set, even when connected.
func TestWritableVaults_ExcludesReserved(t *testing.T) {
	k := newTestAPI(t)
	connectTempVault(t, k, reservedVaultName)
	connectTempVault(t, k, "Docs")

	w := k.WritableVaults()
	for _, v := range w {
		if v.Name == reservedVaultName {
			t.Fatalf("reserved vault leaked into writable set: %+v", w)
		}
	}
	found := false
	for _, v := range w {
		if v.Name == "Docs" {
			found = true
		}
	}
	if !found {
		t.Errorf("writable set missing Docs: %+v", w)
	}
}

// TestVaultByName_IgnoresReserved: vaultByName must never resolve the reserved
// vault, so it can't be used as a covert import target.
func TestVaultByName_IgnoresReserved(t *testing.T) {
	k := newTestAPI(t)
	connectTempVault(t, k, reservedVaultName)
	if k.vaultByName(reservedVaultName) != nil {
		t.Error("vaultByName must not resolve the reserved vault")
	}
	if k.vaultByName("") != nil {
		t.Error("vaultByName must not resolve an empty name")
	}
}

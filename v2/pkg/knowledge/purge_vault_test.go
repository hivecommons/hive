package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPurgeVault_RefusesOutsideBase: a purge path outside vaultsBaseDir (or the
// base itself) is refused — the hard guard against deleting arbitrary dirs.
func TestPurgeVault_RefusesOutsideBase(t *testing.T) {
	k := newTestAPI(t)
	if err := k.PurgeVault("/etc"); err == nil {
		t.Error("purge outside vaults base must be refused")
	}
	if err := k.PurgeVault(vaultsBaseDir); err == nil {
		t.Error("purge of the base dir itself must be refused")
	}
	if err := k.PurgeVault(filepath.Join(vaultsBaseDir, reservedVaultName)); err == nil {
		t.Error("purge of the reserved vault must be refused")
	}
}

// TestPurgeVault_DeletesFiles: a valid channel under a (test-overridden) base is
// disconnected AND its files removed.
func TestPurgeVault_DeletesFiles(t *testing.T) {
	// Point vaultsBaseDir at a temp dir so the guard passes for a real path.
	orig := vaultsBaseDir
	tmp := t.TempDir()
	vaultsBaseDir = tmp
	t.Cleanup(func() { vaultsBaseDir = orig })

	dir := filepath.Join(tmp, "runbooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# x\n\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	k := newTestAPI(t)
	_ = k.ConnectVault(dir, "runbooks")

	if err := k.PurgeVault(dir); err != nil {
		t.Fatalf("PurgeVault: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("channel dir should be gone, stat err=%v", err)
	}
	if k.vaultByName("runbooks") != nil {
		t.Error("channel should be disconnected after purge")
	}
}

package hub

// Error-path pins for storeAppKeyAtPath, the single write path both the
// primary and secondary App key stores share. The happy path (0600 file,
// atomic rename) is pinned elsewhere; these tests pin what happens when the
// filesystem says no: every failure must surface as an error naming the
// failed step, and a failed store must never leave a stray temp file that a
// later rename could publish as key material.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoreAppKeyAtPathMkdirFailure: when the key directory cannot be
// created (a regular file squats on the path), the store must fail with the
// mkdir step named and must not invent a file anywhere.
func TestStoreAppKeyAtPathMkdirFailure(t *testing.T) {
	orig := clusterAppKeyDir
	t.Cleanup(func() { clusterAppKeyDir = orig })

	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	clusterAppKeyDir = filepath.Join(blocker, "app-keys")

	pem := testAppKeyPEM(t)
	err := storeAppKeyAtPath(filepath.Join(clusterAppKeyDir, "c1.pem"), pem, "cluster c1")
	if err == nil {
		t.Fatal("expected error when key dir cannot be created")
	}
	if !strings.Contains(err.Error(), "create app key dir") {
		t.Fatalf("error should name the mkdir step, got: %v", err)
	}
	if strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("error must never carry key material: %v", err)
	}
}

// TestStoreAppKeyAtPathCreateTempFailure: an unwritable key directory must
// fail at the temp-file step, before any byte of the key touches disk.
func TestStoreAppKeyAtPathCreateTempFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	orig := clusterAppKeyDir
	t.Cleanup(func() { clusterAppKeyDir = orig })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod key dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	clusterAppKeyDir = dir

	pem := testAppKeyPEM(t)
	err := storeAppKeyAtPath(filepath.Join(dir, "c1.pem"), pem, "cluster c1")
	if err == nil {
		t.Fatal("expected error when key dir is unwritable")
	}
	if !strings.Contains(err.Error(), "create temp app key file") {
		t.Fatalf("error should name the temp-file step, got: %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read key dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed store must leave no files behind, found %d", len(entries))
	}
}

// TestStoreAppKeyAtPathRenameFailureRemovesTemp: when the final rename fails
// (target path in a directory that does not exist), the deferred cleanup must
// remove the fully written temp file — a leftover 0600 temp full of key
// material in the shared key dir is still a leaked key waiting for a reader.
func TestStoreAppKeyAtPathRenameFailureRemovesTemp(t *testing.T) {
	orig := clusterAppKeyDir
	t.Cleanup(func() { clusterAppKeyDir = orig })

	dir := t.TempDir()
	clusterAppKeyDir = dir

	pem := testAppKeyPEM(t)
	target := filepath.Join(dir, "no-such-subdir", "c1.pem")
	err := storeAppKeyAtPath(target, pem, "cluster c1")
	if err == nil {
		t.Fatal("expected error when rename target directory is missing")
	}
	if !strings.Contains(err.Error(), "rename app key into place") {
		t.Fatalf("error should name the rename step, got: %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read key dir: %v", readErr)
	}
	for _, e := range entries {
		t.Fatalf("temp key file left behind after failed rename: %s", e.Name())
	}
}

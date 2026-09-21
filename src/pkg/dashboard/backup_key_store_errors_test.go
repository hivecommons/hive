package dashboard

// Error-path and mode pins for storeBackupKey, the function that writes the
// backup encryption key VALUE to the PVC. The handlers around it are pinned
// in backup_key_test.go; these tests pin the write itself: failures must
// surface (a swallowed error here means "backups enabled" with no key on
// disk), and a pre-existing loose-mode file must be tightened to 0600 rather
// than trusted.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoreBackupKeyMkdirFailure: a regular file squatting on the secrets
// directory path must fail the store with the directory step named.
func TestStoreBackupKeyMkdirFailure(t *testing.T) {
	orig := writableBackupKeyFile
	t.Cleanup(func() { writableBackupKeyFile = orig })

	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	writableBackupKeyFile = filepath.Join(blocker, "secrets", "backup_encryption_key")

	path, err := storeBackupKey(strings.Repeat("a", backupKeyHexLen))
	if err == nil {
		t.Fatalf("expected error when secrets dir cannot be created, got path %q", path)
	}
	if !strings.Contains(err.Error(), "create secrets directory") {
		t.Fatalf("error should name the mkdir step, got: %v", err)
	}
}

// TestStoreBackupKeyWriteFailure: an unwritable secrets directory must fail
// the store with the write step named, not report success with no key file.
func TestStoreBackupKeyWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	orig := writableBackupKeyFile
	t.Cleanup(func() { writableBackupKeyFile = orig })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod secrets dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	writableBackupKeyFile = filepath.Join(dir, "backup_encryption_key")

	_, err := storeBackupKey(strings.Repeat("a", backupKeyHexLen))
	if err == nil {
		t.Fatal("expected error when secrets dir is unwritable")
	}
	if !strings.Contains(err.Error(), "write key file") {
		t.Fatalf("error should name the write step, got: %v", err)
	}
}

// TestStoreBackupKeyTightensPreexistingLooseMode: WriteFile keeps an existing
// file's mode, so storeBackupKey chmods explicitly. Pin that a key file left
// world-readable by an older image comes out 0600 with the new value.
func TestStoreBackupKeyTightensPreexistingLooseMode(t *testing.T) {
	orig := writableBackupKeyFile
	t.Cleanup(func() { writableBackupKeyFile = orig })

	dir := t.TempDir()
	p := filepath.Join(dir, "backup_encryption_key")
	if err := os.WriteFile(p, []byte("old-key\n"), 0o666); err != nil {
		t.Fatalf("seed loose-mode key file: %v", err)
	}
	writableBackupKeyFile = p

	key := strings.Repeat("b", backupKeyHexLen)
	got, err := storeBackupKey(key)
	if err != nil {
		t.Fatalf("storeBackupKey: %v", err)
	}
	if got != p {
		t.Fatalf("returned path %q, want %q", got, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if fi.Mode().Perm() != backupKeyFileMode {
		t.Fatalf("pre-existing key file kept mode %v, want %v", fi.Mode().Perm(), os.FileMode(backupKeyFileMode))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if string(data) != key+"\n" {
		t.Fatalf("key file content = %q, want new key + newline", string(data))
	}
}

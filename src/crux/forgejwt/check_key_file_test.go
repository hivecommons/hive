package forgejwt

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/crux/keystore"
)

// TestCheckKeyFile pins hivecommons/hive#19919: `hive validate` runs the
// mint's key checks, so a key_file that is missing, too open, not PEM or too
// small fails there and not at the first clone or push.
func TestCheckKeyFile(t *testing.T) {
	good := pemBytes(t, testKeys()[0])
	if err := CheckKeyFile(writeFile(t, good, 0o600)); err != nil {
		t.Errorf("a valid 0600 key: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "nope.pem")
	if err := CheckKeyFile(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: err = %v, want os.ErrNotExist", err)
	}
	if err := CheckKeyFile(writeFile(t, good, 0o644)); !errors.Is(err, ErrKeyFileMode) {
		t.Errorf("mode 0644: err = %v, want ErrKeyFileMode", err)
	}
	if err := CheckKeyFile(writeFile(t, []byte("not a key"), 0o600)); err == nil {
		t.Error("a file that is not PEM passed")
	}
}

// A key the boot keystore sweep sealed in place opens when a keystore is
// active, and is not refused when this process has none, because only the
// booted hive can open it.
func TestCheckKeyFileSealedKey(t *testing.T) {
	const material = "forgejwt-check-keystore-material-0123456789"
	dir := t.TempDir()
	t.Cleanup(keystore.SetRewriteRootsForTest(dir))
	t.Cleanup(keystore.ResetForTest)
	keystore.ResetForTest()
	p, err := keystore.NewEnvKeyProvider([]byte(material))
	if err != nil {
		t.Fatal(err)
	}
	keystore.SetActive(p)

	path := filepath.Join(dir, "forgejo-jwt.pem")
	if err := os.WriteFile(path, pemBytes(t, testKeys()[0]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := keystore.MigrateDir(t.Context(), dir); err != nil {
		t.Fatalf("MigrateDir: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !keystore.IsEnvelope(raw) {
		t.Fatal("the key was not sealed in place")
	}
	if err := CheckKeyFile(path); err != nil {
		t.Errorf("sealed key with the keystore active: %v", err)
	}

	keystore.ResetForTest()
	if err := CheckKeyFile(path); err != nil {
		t.Errorf("sealed key with no keystore in this process: %v", err)
	}
}

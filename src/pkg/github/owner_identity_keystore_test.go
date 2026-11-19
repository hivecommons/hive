package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/keystore"
)

// hivecommons/hive#18214: with a keystore provider active the identity file
// holds an envelope, never the token, Load opens it, and a file written in
// legacy plaintext is re-sealed on its first read.
func TestOwnerIdentityStore_SealedAtRest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	defer keystore.SetRewriteRootsForTest(dir)()
	prov, err := keystore.NewEnvKeyProvider([]byte(strings.Repeat("k", 64)))
	if err != nil {
		t.Fatal(err)
	}
	keystore.ResetForTest()
	keystore.SetActive(prov)
	t.Cleanup(keystore.ResetForTest)

	path := filepath.Join(dir, "github-owner-token")
	store := NewOwnerIdentityStore(path)
	if err := store.Save(OwnerIdentity{Login: "octocat", Token: "gho_secret", Scope: OwnerIdentityScope}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "gho_secret") || !keystore.IsEnvelope(raw) {
		t.Fatalf("identity file is not sealed: %q", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	id, ok, err := store.Load()
	if err != nil || !ok || id.Token != "gho_secret" || id.Login != "octocat" {
		t.Fatalf("Load = %+v ok=%v err=%v", id, ok, err)
	}
	if tok, ok := store.Token(); !ok || tok != "gho_secret" {
		t.Fatalf("Token = %q ok=%v", tok, ok)
	}

	// Legacy plaintext file from before the keystore: readable, then sealed.
	if err := os.WriteFile(path, []byte(`{"login":"octocat","token":"gho_old","connected_at":"2026-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	id, ok, err = store.Load()
	if err != nil || !ok || id.Token != "gho_old" {
		t.Fatalf("legacy Load = %+v ok=%v err=%v", id, ok, err)
	}
	raw, _ = os.ReadFile(path)
	if !keystore.IsEnvelope(raw) {
		t.Fatalf("legacy identity file was not re-sealed on read: %q", raw)
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
}

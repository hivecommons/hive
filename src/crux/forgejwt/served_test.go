package forgejwt

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyListFromFilesPublishesCurrentAndPrevious(t *testing.T) {
	keys := testKeys()
	cur := writeFile(t, pemBytes(t, keys[0]), 0o600)
	old := writeFile(t, pemBytes(t, keys[1]), 0o600)
	missing := filepath.Join(t.TempDir(), "gone.pem")

	// The current key repeated in previous_key_files is listed once; an
	// unreadable previous key is skipped and reported, not fatal.
	discovery, jwks, skipped, err := KeyListFromFiles(testIssuer, cur, []string{old, cur, missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Error(), missing) {
		t.Fatalf("skipped = %v, want the missing previous key", skipped)
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 2 || set.Keys[0]["kid"] != keyID(&keys[0].PublicKey) || set.Keys[1]["kid"] != keyID(&keys[1].PublicKey) {
		t.Fatalf("key list = %s, want the current key then the previous key", jwks)
	}
	// Only public members are rendered: no private exponent or primes.
	for _, k := range set.Keys {
		for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
			if _, ok := k[private]; ok {
				t.Fatalf("key list carries private member %q: %s", private, jwks)
			}
		}
	}
	if bytes.Contains(jwks, []byte(cur)) || bytes.Contains(discovery, []byte(cur)) || bytes.Contains(jwks, []byte("PRIVATE")) {
		t.Fatalf("rendered documents name a key path or private key: %s %s", discovery, jwks)
	}
	var disc map[string]any
	if err := json.Unmarshal(discovery, &disc); err != nil {
		t.Fatal(err)
	}
	if disc["issuer"] != string(testIssuer) || disc["jwks_uri"] != string(testIssuer)+"/"+KeyListPath {
		t.Fatalf("discovery = %s", discovery)
	}

	// A token minted with either key verifies against the served list: the
	// rotation overlap the setting exists for.
	for _, path := range []string{cur, old} {
		tok, err := Signer{Issuer: testIssuer, KeyFile: path}.Mint("aud", "hive:contributor")
		if err != nil {
			t.Fatal(err)
		}
		if err := verify(t, tok, jwks); err != nil {
			t.Fatalf("token from %s does not verify: %v", path, err)
		}
	}
}

func TestKeyListFromFilesFailsWithoutCurrentKey(t *testing.T) {
	old := writeFile(t, pemBytes(t, testKeys()[1]), 0o600)
	if _, _, _, err := KeyListFromFiles(testIssuer, filepath.Join(t.TempDir(), "gone.pem"), []string{old}); err == nil {
		t.Fatal("missing current key: want an error, not a list of the previous key alone")
	}
	open := writeFile(t, pemBytes(t, testKeys()[0]), 0o644)
	if _, _, _, err := KeyListFromFiles(testIssuer, open, nil); !errors.Is(err, ErrKeyFileMode) {
		t.Fatalf("world-readable current key: err = %v, want ErrKeyFileMode", err)
	}
	if _, _, _, err := KeyListFromFiles("https://keys.example.org/path", old, nil); err == nil {
		t.Fatal("issuer with a path: want an error")
	}
}

func TestKeyListFromFilesRefusesOversizedList(t *testing.T) {
	keys := testKeys()
	cur := writeFile(t, pemBytes(t, keys[0]), 0o600)
	old := writeFile(t, pemBytes(t, keys[1]), 0o600)
	_, jwks, _, err := KeyListFromFiles(testIssuer, cur, []string{old})
	if err != nil {
		t.Fatal(err)
	}
	orig := documentLimit
	t.Cleanup(func() { documentLimit = orig })
	documentLimit = len(jwks) - 1
	if _, _, _, err := KeyListFromFiles(testIssuer, cur, []string{old}); !errors.Is(err, ErrKeyListTooLarge) {
		t.Fatalf("err = %v, want ErrKeyListTooLarge", err)
	}
	if MaxDocumentBytes != 16<<10 {
		t.Fatalf("MaxDocumentBytes = %d, want Forgejo's 16 KiB read limit", MaxDocumentBytes)
	}
}

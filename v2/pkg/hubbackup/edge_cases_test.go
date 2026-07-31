package hubbackup

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- LoadKey / Seal / Open edges ----

// A key of the right length but wrong hex must be rejected, and the error must
// tell the operator what shape is expected.
func TestLoadKeyRejectsNonHex(t *testing.T) {
	t.Setenv(EnvBackupKey, strings.Repeat("z", aesKeySize*2))
	_, err := LoadKey()
	if err == nil {
		t.Fatal("non-hex key must be rejected")
	}
	if !strings.Contains(err.Error(), "hex") {
		t.Errorf("error should mention hex encoding, got %v", err)
	}
}

// Surrounding whitespace (a trailing newline from a Secret mount) must not
// break key loading.
func TestLoadKeyToleratesSurroundingWhitespace(t *testing.T) {
	t.Setenv(EnvBackupKey, "  "+testKey+"\n")
	key, err := LoadKey()
	if err != nil {
		t.Fatalf("a key with surrounding whitespace must load: %v", err)
	}
	if len(key) != aesKeySize {
		t.Fatalf("key length = %d, want %d", len(key), aesKeySize)
	}
}

// Seal must reject a key that is not AES-256, rather than silently using a
// weaker cipher.
func TestSealRejectsWrongKeySize(t *testing.T) {
	if _, err := Seal([]byte("tooshort"), []byte("data")); err == nil {
		t.Fatal("a short key must be rejected by Seal")
	}
}

func TestOpenRejectsWrongKeySize(t *testing.T) {
	if _, err := Open([]byte("tooshort"), []byte("data")); err == nil {
		t.Fatal("a short key must be rejected by Open")
	}
}

// Two seals of identical plaintext must differ: a reused nonce would leak
// information across archives.
func TestSealUsesFreshNoncePerCall(t *testing.T) {
	key := decodeTestKey(t)
	plaintext := []byte("identical plaintext")
	a, err := Seal(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("two seals of the same plaintext are byte-identical — the nonce is reused")
	}
	// Both must still decrypt to the original.
	for _, sealed := range [][]byte{a, b} {
		got, err := Open(key, sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(got) != string(plaintext) {
			t.Fatalf("round trip corrupted: %q", got)
		}
	}
}

// Data shorter than a GCM nonce must be rejected with a clear message rather
// than panicking on a slice bound.
func TestOpenRejectsUndersizedCiphertext(t *testing.T) {
	_, err := Open(decodeTestKey(t), []byte{1, 2, 3})
	if err == nil {
		t.Fatal("ciphertext shorter than the nonce must be rejected")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("error should say the ciphertext is too short, got %v", err)
	}
}

// ---- decodeBase64 ----

// Shell `base64` wraps output; the decoder must tolerate embedded whitespace.
func TestDecodeBase64ToleratesWrappedOutput(t *testing.T) {
	original := []byte("some spoke configuration content that wraps")
	encoded := base64.StdEncoding.EncodeToString(original)
	wrapped := encoded[:10] + "\n" + encoded[10:20] + "\r\n  " + encoded[20:]

	got, err := decodeBase64(wrapped)
	if err != nil {
		t.Fatalf("wrapped base64 must decode: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("decoded content differs: %q", got)
	}
}

func TestDecodeBase64RejectsGarbage(t *testing.T) {
	if _, err := decodeBase64("!!!!not base64!!!!"); err == nil {
		t.Fatal("invalid base64 must be rejected")
	}
}

// ---- parseSpokeStream ----

// A stream with no markers yields no files rather than one anonymous blob.
func TestParseSpokeStreamIgnoresUnmarkedOutput(t *testing.T) {
	files, err := parseSpokeStream([]byte("some stray stderr\nmore noise\n"))
	if err != nil {
		t.Fatalf("unmarked output must not error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("unmarked output must produce no files, got %v", files)
	}
}

// Multiple files in one stream must all be recovered — a spoke costs one exec,
// so a parser that stops after the first file silently loses the rest.
func TestParseSpokeStreamRecoversEveryFile(t *testing.T) {
	stream := spokeStream(map[string]string{
		"hive.yaml.runtime": "config-a",
		"agents.json":   "config-b",
		"app-key.pem":   "config-c",
	})
	files, err := parseSpokeStream([]byte(stream))
	if err != nil {
		t.Fatalf("parseSpokeStream: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 files, got %d: %v", len(files), keysOfBytes(files))
	}
	if string(files["hive.yaml.runtime"]) != "config-a" {
		t.Errorf("hive.yaml.runtime = %q", files["hive.yaml.runtime"])
	}
	if string(files["app-key.pem"]) != "config-c" {
		t.Errorf("app-key.pem = %q", files["app-key.pem"])
	}
}

// ---- buildSpokeReadScript ----

// The script must read hive.yaml.runtime (authoritative) — a live spoke has both
// hive.yaml and hive.yaml.runtime with DIFFERENT content, so reading the wrong one
// silently backs up stale config.
func TestSpokeReadScriptPrefersAuthoritativeConfig(t *testing.T) {
	script := buildSpokeReadScript()
	if !strings.Contains(script, "/data/hive.yaml.runtime") {
		t.Fatal("the read script must capture /data/hive.yaml.runtime")
	}
	// It must also glob App keys, whose filenames vary per cluster.
	if !strings.Contains(script, spokeKeyGlob) {
		t.Errorf("the read script must glob GitHub App keys (%s)", spokeKeyGlob)
	}
	// Every wanted file must be guarded by an existence test so a missing file
	// does not abort the whole exec.
	if !strings.Contains(script, "if [ -f") {
		t.Error("the script must test for file existence before reading")
	}
}

// ---- Extract edges ----

// An absolute path in the archive must be refused before anything is written.
func TestExtractRejectsAbsolutePaths(t *testing.T) {
	key := decodeTestKey(t)
	sealed := buildArchiveFixture(t, key, "/etc/passwd", []byte("root:x:0:0"), nil)
	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err == nil {
		t.Fatal("an absolute archive path must be refused")
	}
}

// Extract must create intermediate directories rather than failing on a deep
// path.
func TestExtractCreatesNestedDirectories(t *testing.T) {
	key := decodeTestKey(t)
	sealed := buildArchiveFixture(t, key, "hub/saas/app-keys/deep/nested.pem",
		[]byte("keymaterial"), nil)
	dest := t.TempDir()
	if _, err := Extract(key, sealed, dest); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "hub", "saas", "app-keys", "deep", "nested.pem"))
	if err != nil {
		t.Fatalf("nested file not restored: %v", err)
	}
	if string(got) != "keymaterial" {
		t.Fatalf("content = %q", got)
	}
}

// ---- ObjectStore edges ----

// A malformed list payload must error rather than silently reporting an empty
// bucket, which would make Prune a no-op and VerifyLatest claim "no archives".
func TestObjectStoreListRejectsMalformedPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not json"))
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	if _, err := store.List(); err == nil {
		t.Fatal("a malformed list payload must be an error, not an empty bucket")
	}
}

// A malformed namespace payload must error.
func TestResolveNamespaceRejectsMalformedPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{not a json string"))
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	if _, err := store.resolveNamespace(); err == nil {
		t.Fatal("a malformed namespace payload must be an error")
	}
}

// A delete failure during prune must be logged and skipped, not abort the run:
// pruning is best-effort cleanup, and failing it must not fail the backup.
func TestPruneContinuesPastDeleteFailure(t *testing.T) {
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			w.Write([]byte(`{"objects":[
				{"name":"hive-hub-backup-20260101T000000Z.tar.gz.enc"},
				{"name":"hive-hub-backup-20260102T000000Z.tar.gz.enc"},
				{"name":"hive-hub-backup-20260731T000000Z.tar.gz.enc"}]}`))
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	if err := store.Prune(1, quietLogger()); err != nil {
		t.Fatalf("a failed delete must not fail the prune: %v", err)
	}
	// Both over-retention archives must have been attempted.
	if deletes != 2 {
		t.Fatalf("want 2 delete attempts despite failures, got %d", deletes)
	}
}

// A List failure must propagate out of Prune — pruning against an unknown
// object set could otherwise delete the wrong things.
func TestPruneFailsWhenListFails(t *testing.T) {
	srv := newStatusServer(t, 500)
	store := newTestStore(t, srv.URL)
	if err := store.Prune(1, quietLogger()); err == nil {
		t.Fatal("Prune must fail when the bucket cannot be listed")
	}
}

// Get on a missing object must be an error, not empty bytes that would then
// "verify" as a corrupt archive with a confusing message.
func TestObjectStoreGetMissingObject(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	store := newTestStore(t, srv.URL)
	if _, err := store.Get("does-not-exist"); err == nil {
		t.Fatal("Get on a missing object must error")
	}
}

package hubbackup

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the reachable error branches of the sealed-archive
// pipeline that the happy-path suites leave unexercised: Build/Finish with a
// bad key, Verify on malformed plaintext, Extract filesystem failures,
// builder tree-walk edge cases, ObjectStore construction/transport errors,
// and the VerifyLatest/Run store-error paths.

// ---- Build / Builder.Finish sealing errors ----

// Build must surface a Seal failure (here: a key that is not a valid AES-256
// key) instead of returning a corrupt archive.
func TestBuildFailsWhenKeyCannotSeal(t *testing.T) {
	t.Setenv(EnvDataDir, t.TempDir())

	_, _, err := Build([]byte("short"), nil, nil, quietLogger())
	if err == nil {
		t.Fatal("Build must fail when the key cannot seal the archive")
	}
	if !strings.Contains(err.Error(), "seal archive") {
		t.Errorf("error should identify the sealing step, got %v", err)
	}
}

// Finish must surface the same Seal failure on the exported builder.
func TestBuilderFinishFailsWhenKeyCannotSeal(t *testing.T) {
	x := NewBuilder()
	if err := x.AddBytes("hub/file", 0o600, []byte("data")); err != nil {
		t.Fatal(err)
	}
	man := &Manifest{FormatVersion: FormatVersion()}
	if _, err := x.Finish([]byte("short"), man, 0); err == nil {
		t.Fatal("Finish must fail when the key cannot seal the archive")
	}
}

// A second Finish writes into a closed tar stream and must fail loudly, not
// silently emit a second, different archive.
func TestBuilderFinishFailsWhenCalledTwice(t *testing.T) {
	x := NewBuilder()
	if err := x.AddBytes("hub/file", 0o600, []byte("data")); err != nil {
		t.Fatal(err)
	}
	key := decodeTestKey(t)
	man := &Manifest{FormatVersion: FormatVersion()}
	if _, err := x.Finish(key, man, 0); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	if _, err := x.Finish(key, man, 0); err == nil {
		t.Fatal("a second Finish on a sealed builder must fail")
	}
}

// AddBytes after Finish targets a closed tar writer and must return the
// writer's error rather than pretending the file was archived.
func TestBuilderAddBytesFailsAfterFinish(t *testing.T) {
	x := NewBuilder()
	if _, err := x.Finish(decodeTestKey(t), &Manifest{FormatVersion: FormatVersion()}, 0); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := x.AddBytes("hub/late", 0o600, []byte("too late")); err == nil {
		t.Fatal("AddBytes on a finished builder must fail")
	}
}

// ---- builder tree walking ----

// A missing source root is logged and skipped: WalkDir hands the callback an
// error, which must not abort the archive.
func TestBuilderAddTreeToleratesMissingRoot(t *testing.T) {
	x := NewBuilder()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if err := x.b.addTree(missing, "hub/x", false, quietLogger()); err != nil {
		t.Fatalf("a missing tree root must be skipped, not fatal: %v", err)
	}
	if len(x.b.files) != 0 {
		t.Fatalf("nothing should have been archived, got %d files", len(x.b.files))
	}
}

// With skipExcluded set, the regenerable scratch directories must be pruned
// as whole subtrees.
func TestBuilderAddTreePrunesExcludedDirs(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "nous", "scratch.bin"), "regenerable")
	mustWrite(t, filepath.Join(dir, "keep.json"), `{"ok":1}`)

	x := NewBuilder()
	if err := x.b.addTree(dir, "hub", true, quietLogger()); err != nil {
		t.Fatalf("addTree: %v", err)
	}
	if len(x.b.files) != 1 || x.b.files[0].Path != "hub/keep.json" {
		t.Fatalf("excluded dir must be pruned; archived %+v", x.b.files)
	}
}

// The exported AddTree must honour a skip callback for individual files, not
// just directories.
func TestExportedAddTreeSkipCallbackOmitsFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "keep.json"), `{"ok":1}`)
	mustWrite(t, filepath.Join(dir, "omit.log"), "noise")

	x := NewBuilder()
	skip := func(rel string) bool { return strings.HasSuffix(rel, ".log") }
	if err := x.AddTree(dir, "hive", skip, quietLogger()); err != nil {
		t.Fatalf("AddTree: %v", err)
	}
	if len(x.b.files) != 1 || x.b.files[0].Path != "hive/keep.json" {
		t.Fatalf("skip callback must omit the file; archived %+v", x.b.files)
	}
}

// ---- Verify on malformed (but correctly sealed) plaintext ----

// A sealed blob whose plaintext is not gzip must fail at the gunzip step, not
// panic or pass.
func TestVerifyRejectsNonGzipPlaintext(t *testing.T) {
	key := decodeTestKey(t)
	sealed, err := Seal(key, []byte("this is not a gzip stream"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(key, sealed)
	if err == nil {
		t.Fatal("non-gzip plaintext must fail verification")
	}
	if !strings.Contains(err.Error(), "gunzip") {
		t.Errorf("error should identify the gunzip step, got %v", err)
	}
}

// Valid gzip wrapping a non-tar stream must fail at the tar-read step.
func TestVerifyRejectsNonTarPlaintext(t *testing.T) {
	key := decodeTestKey(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("gzip yes, tar no")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	_, err = Verify(key, sealed)
	if err == nil {
		t.Fatal("a non-tar stream must fail verification")
	}
	if !strings.Contains(err.Error(), "read tar") {
		t.Errorf("error should identify the tar step, got %v", err)
	}
}

// ---- Extract filesystem failures ----

// Extract must fail when a member's parent path is occupied by a regular
// file, instead of silently dropping the member.
func TestExtractFailsWhenParentPathIsAFile(t *testing.T) {
	key := decodeTestKey(t)
	sealed := buildArchiveFixture(t, key, "hub/nested/file.json", []byte(`{"ok":1}`), nil)

	dest := t.TempDir()
	// Occupy "hub" with a file so MkdirAll("hub/nested") must fail.
	if err := os.WriteFile(filepath.Join(dest, "hub"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(key, sealed, dest); err == nil {
		t.Fatal("Extract must fail when it cannot create the member's directory")
	}
}

// Extract must fail when the member's own path is occupied by a directory.
func TestExtractFailsWhenTargetIsADirectory(t *testing.T) {
	key := decodeTestKey(t)
	sealed := buildArchiveFixture(t, key, "hub/file.json", []byte(`{"ok":1}`), nil)

	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "hub", "file.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(key, sealed, dest); err == nil {
		t.Fatal("Extract must fail when the member path is a directory")
	}
}

// ---- parseSpokeStream flush on the NEXT marker ----

// A corrupt base64 body must fail when the next @@FILE@@ marker forces the
// flush, not only at end of stream.
func TestParseSpokeStreamFlushFailsOnNextMarker(t *testing.T) {
	raw := spokeStreamPrefix + "first" + spokeStreamDelim + "\n" +
		"!!!not-base64!!!\n" +
		spokeStreamPrefix + "second" + spokeStreamDelim + "\n" +
		"aGVsbG8=\n"
	if _, err := parseSpokeStream([]byte(raw)); err == nil {
		t.Fatal("corrupt base64 must fail when the next marker flushes it")
	}
}

// ---- ObjectStore construction and transport errors ----

// A PEM block whose bytes parse as neither PKCS1 nor PKCS8 must be rejected
// with the PKCS8 parse error, not accepted as a key.
func TestNewObjectStoreRejectsUnparseableKeyBytes(t *testing.T) {
	setOCIEnv(t)
	junk := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER at all")})
	t.Setenv(envOCIPrivateKey, string(junk))

	_, err := NewObjectStore("backups")
	if err == nil {
		t.Fatal("an unparseable private key must be rejected")
	}
	if !strings.Contains(err.Error(), "parse OCI private key") {
		t.Errorf("error should identify the key parse step, got %v", err)
	}
}

// With no region in the environment, the constructor must fall back to the
// documented default rather than building host "objectstorage..oraclecloud.com".
func TestNewObjectStoreDefaultsRegion(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	setOCIEnv(t)
	t.Setenv(envOCIRegion, "")
	t.Setenv(envOCIEndpoint, srv.URL)

	store, err := NewObjectStore("backups")
	if err != nil {
		t.Fatalf("NewObjectStore: %v", err)
	}
	if store.region != defaultOCIRegion {
		t.Fatalf("region should default to %q, got %q", defaultOCIRegion, store.region)
	}
}

// An invalid HTTP method must fail at request construction.
func TestObjectStoreDoRejectsInvalidMethod(t *testing.T) {
	srv, _ := ociMock(t, map[string][]byte{})
	store := newTestStore(t, srv.URL)
	if _, err := store.do("BAD METHOD", "/n/", nil); err == nil {
		t.Fatal("an invalid method must fail request construction")
	}
}

// A dead endpoint must surface the transport error, not hang or succeed.
func TestObjectStoreDoFailsWhenEndpointUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens here any more

	store := newTestStore(t, url)
	if _, err := store.do(http.MethodGet, "/n/", nil); err == nil {
		t.Fatal("a connection failure must be reported")
	}
}

// ---- VerifyLatest store-error paths ----

// Missing OCI credentials must fail VerifyLatest after the key loads.
func TestVerifyLatestFailsWithoutObjectStoreCredentials(t *testing.T) {
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(envOCITenancy, "")
	t.Setenv(EnvBucket, "backups")

	if _, err := VerifyLatest(quietLogger()); err == nil {
		t.Fatal("VerifyLatest must fail without object storage credentials")
	}
}

// A failing List must be surfaced, not reported as an empty bucket.
func TestVerifyLatestFailsWhenListFails(t *testing.T) {
	srv := newNamespaceThenStatusServer(t, 500)
	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvBucket, "backups")

	if _, err := VerifyLatest(quietLogger()); err == nil {
		t.Fatal("a failing List must fail VerifyLatest")
	}
}

// A bucket that lists an archive which then cannot be fetched must fail.
func TestVerifyLatestFailsWhenGetFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ociNamespacePath {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			var lr listResponse
			lr.Objects = append(lr.Objects, struct {
				Name string `json:"name"`
			}{Name: "hive-hub-backup-20260101T000000Z.tar.gz.enc"})
			json.NewEncoder(w).Encode(lr)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvBucket, "backups")

	if _, err := VerifyLatest(quietLogger()); err == nil {
		t.Fatal("a failing Get must fail VerifyLatest")
	}
}

// ---- Run: prune failure is a warning, not a failed backup ----

// A verified upload followed by a failed prune must still count as a good
// backup: retention trimming is best-effort by design.
func TestRunSucceedsWhenPruneFails(t *testing.T) {
	dir := seedHubData(t)
	t.Setenv(EnvDataDir, dir)
	t.Setenv(EnvBackupKey, testKey)
	t.Setenv(EnvRetention, "1")

	objects := map[string][]byte{
		"hive-hub-backup-20260101T000000Z.tar.gz.enc": []byte("old1"),
		"hive-hub-backup-20260102T000000Z.tar.gz.enc": []byte("old2"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ociNamespacePath {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			// Run only lists during Prune, so a broken list breaks exactly
			// the prune step while upload and read-back stay healthy.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		idx := strings.Index(r.URL.Path, "/o/")
		if idx == -1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name := r.URL.Path[idx+3:]
		switch r.Method {
		case http.MethodPut:
			buf := make([]byte, r.ContentLength)
			if r.ContentLength > 0 {
				if _, err := r.Body.Read(buf); err != nil && err.Error() != "EOF" {
					t.Errorf("read body: %v", err)
				}
			}
			objects[name] = buf
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(data)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	setOCIEnv(t)
	t.Setenv(envOCIEndpoint, srv.URL)
	t.Setenv(EnvBucket, "backups")

	man, name, err := Run(Options{SkipSpokes: true, SkipSecrets: true}, quietLogger())
	if err != nil {
		t.Fatalf("a failed prune must not fail the backup: %v", err)
	}
	if man == nil || name == "" {
		t.Fatal("Run must return the manifest and object name")
	}
	if _, ok := objects[name]; !ok {
		t.Fatalf("archive %q was not uploaded; bucket has %v", name, keysOf(objects))
	}
}

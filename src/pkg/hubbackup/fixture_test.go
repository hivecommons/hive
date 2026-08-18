package hubbackup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// slogLogger aliases *slog.Logger so the fixed collectors can implement the
// package's collector interfaces without importing slog at every use site.
type slogLogger = slog.Logger

// errFake is a sentinel used where the specific error value is irrelevant.
var errFake = errors.New("simulated failure")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func fixedZone(t *testing.T, offsetSeconds int) *time.Location {
	t.Helper()
	return time.FixedZone("TEST", offsetSeconds)
}

// buildArchiveFixture assembles a sealed archive containing exactly one file
// plus a manifest, applying mutate to the manifest before it is written. This
// lets a test desynchronise the manifest from the actual bytes — the shape of
// the bug where Verify passed while checking nothing.
func buildArchiveFixture(t *testing.T, key []byte, path string, content []byte,
	mutate func(*Manifest)) []byte {
	t.Helper()

	var raw strings.Builder
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)

	writeFile := func(name string, body []byte) {
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(path, content)

	man := &Manifest{
		FormatVersion: backupFormatVersion,
		CreatedAt:     time.Now().UTC(),
		Files: []FileEntry{{
			Path: path, Size: int64(len(content)), SHA256: sha256Hex(content),
		}},
	}
	if mutate != nil {
		mutate(man)
	}
	manJSON, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(manifestName, manJSON)

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, []byte(raw.String()))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// buildArchiveFixtureNoManifest produces a well-formed archive that omits
// MANIFEST.json entirely.
func buildArchiveFixtureNoManifest(t *testing.T, key []byte) []byte {
	t.Helper()
	var raw strings.Builder
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)

	body := []byte("orphan")
	hdr := &tar.Header{Name: "hub/orphan", Mode: 0o600, Size: int64(len(body)), ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(key, []byte(raw.String()))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// testPKCS8KeyPEM generates a throwaway RSA key in PKCS8 PEM form. Real
// oci-api-key Secrets carry either PKCS1 or PKCS8, so both must parse.
func testPKCS8KeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// testED25519KeyPEM produces a valid PKCS8 key that is NOT RSA, to prove the
// non-RSA rejection path is real rather than a parse failure.
func testED25519KeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// newStatusServer always answers with the given status code.
func newStatusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newNamespaceThenStatusServer resolves the namespace successfully, then fails
// every subsequent request — the shape of "credentials work, upload denied".
func newNamespaceThenStatusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ociNamespacePath {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chmodUnreadable removes all permissions from path. Returns an error when the
// platform or a root test runner makes that impossible.
func chmodUnreadable(path string) error {
	if err := os.Chmod(path, 0o000); err != nil {
		return err
	}
	if f, err := os.Open(path); err == nil {
		f.Close()
		return errors.New("file is still readable (running as root?)")
	}
	return nil
}

// keysOf returns a map's keys, for readable failure messages.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// keysOfBytes returns a file map's keys, for readable failure messages.
func keysOfBytes(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

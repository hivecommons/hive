package hubbackup

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testRSAKeyPEM generates a throwaway RSA key in PKCS1 PEM form. Generated per
// run so no key material is committed.
func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

// setOCIEnv installs a complete set of fake OCI credentials.
func setOCIEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envOCITenancy, "ocid1.tenancy.oc1..test")
	t.Setenv(envOCIUser, "ocid1.user.oc1..test")
	t.Setenv(envOCIFingerprint, "aa:bb:cc")
	t.Setenv(envOCIPrivateKey, testRSAKeyPEM(t))
	t.Setenv(envOCIRegion, "us-ashburn-1")
}

// ociMock serves the Object Storage endpoints against an in-memory bucket.
func ociMock(t *testing.T, objects map[string][]byte) (*httptest.Server, *[]string) {
	t.Helper()
	var methods []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)

		// Namespace resolution: GET /n/
		if r.URL.Path == ociNamespacePath {
			json.NewEncoder(w).Encode("testnamespace")
			return
		}
		// List: GET /n/<ns>/b/<bucket>/o
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
			var lr listResponse
			for name := range objects {
				lr.Objects = append(lr.Objects, struct {
					Name string `json:"name"`
				}{Name: name})
			}
			json.NewEncoder(w).Encode(lr)
			return
		}
		// Single object operations: /n/<ns>/b/<bucket>/o/<name>
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
		case http.MethodDelete:
			delete(objects, name)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &methods
}

// newTestStore builds an ObjectStore wired to the mock, bypassing
// NewObjectStore's namespace round trip.
func newTestStore(t *testing.T, endpoint string) *ObjectStore {
	t.Helper()
	setOCIEnv(t)
	block, _ := pem.Decode([]byte(testRSAKeyPEM(t)))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return &ObjectStore{
		tenancy: "t", user: "u", fingerprint: "f",
		privateKey: key, region: "us-ashburn-1",
		namespace: "testnamespace", bucket: "backups",
		client:           &http.Client{},
		endpointOverride: endpoint,
	}
}

// ---- NewObjectStore credential validation ----

func TestNewObjectStoreRequiresAllCredentials(t *testing.T) {
	cases := []struct{ name, unset string }{
		{"tenancy", envOCITenancy},
		{"user", envOCIUser},
		{"fingerprint", envOCIFingerprint},
		{"private key", envOCIPrivateKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setOCIEnv(t)
			t.Setenv(tc.unset, "")
			if _, err := NewObjectStore("bucket"); err == nil {
				t.Fatalf("missing %s must be rejected, not silently defaulted", tc.unset)
			}
		})
	}
}

func TestNewObjectStoreRequiresBucket(t *testing.T) {
	setOCIEnv(t)
	_, err := NewObjectStore("")
	if err == nil {
		t.Fatal("empty bucket must be rejected: there is no backup destination")
	}
	if !strings.Contains(err.Error(), EnvBucket) {
		t.Errorf("error should name %s, got %v", EnvBucket, err)
	}
}

func TestNewObjectStoreRejectsBadPEM(t *testing.T) {
	setOCIEnv(t)
	t.Setenv(envOCIPrivateKey, "not a pem block")
	if _, err := NewObjectStore("bucket"); err == nil {
		t.Fatal("invalid PEM must be rejected")
	}
}

// ---- Put / Get / List / Delete / Prune ----

func TestObjectStorePutGetRoundTrip(t *testing.T) {
	objects := map[string][]byte{}
	srv, _ := ociMock(t, objects)
	store := newTestStore(t, srv.URL)

	payload := []byte("sealed-archive-bytes")
	if err := store.Put("hive-hub-backup-20260731T000000Z.tar.gz.enc", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get("hive-hub-backup-20260731T000000Z.tar.gz.enc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round trip corrupted the archive: got %q want %q", got, payload)
	}
}

// List must return archives newest-first and must ignore unrelated objects,
// because VerifyLatest and Prune both depend on names[0] being the newest.
func TestObjectStoreListIsNewestFirstAndFiltered(t *testing.T) {
	objects := map[string][]byte{
		"hive-hub-backup-20260701T000000Z.tar.gz.enc": {1},
		"hive-hub-backup-20260731T000000Z.tar.gz.enc": {2},
		"hive-hub-backup-20260715T000000Z.tar.gz.enc": {3},
		"unrelated-object.txt":                        {4},
	}
	srv, _ := ociMock(t, objects)
	store := newTestStore(t, srv.URL)

	names, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("want 3 archives (unrelated object filtered out), got %v", names)
	}
	if names[0] != "hive-hub-backup-20260731T000000Z.tar.gz.enc" {
		t.Fatalf("List must be newest-first, got %v", names)
	}
	for _, n := range names {
		if n == "unrelated-object.txt" {
			t.Fatal("non-archive objects must not be listed")
		}
	}
}

// Prune keeps the N newest and deletes the rest. Deleting the wrong end would
// destroy the most recent backup.
func TestObjectStorePruneDeletesOldestOnly(t *testing.T) {
	objects := map[string][]byte{
		"hive-hub-backup-20260701T000000Z.tar.gz.enc": {1},
		"hive-hub-backup-20260715T000000Z.tar.gz.enc": {2},
		"hive-hub-backup-20260731T000000Z.tar.gz.enc": {3},
	}
	srv, _ := ociMock(t, objects)
	store := newTestStore(t, srv.URL)

	if err := store.Prune(2, quietLogger()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("want 2 archives retained, got %d: %v", len(objects), objects)
	}
	if _, ok := objects["hive-hub-backup-20260731T000000Z.tar.gz.enc"]; !ok {
		t.Fatal("Prune deleted the NEWEST archive — retention is inverted")
	}
	if _, ok := objects["hive-hub-backup-20260701T000000Z.tar.gz.enc"]; ok {
		t.Fatal("Prune kept the oldest archive")
	}
}

func TestObjectStorePruneNoopWhenUnderRetention(t *testing.T) {
	objects := map[string][]byte{
		"hive-hub-backup-20260731T000000Z.tar.gz.enc": {1},
	}
	srv, _ := ociMock(t, objects)
	store := newTestStore(t, srv.URL)

	if err := store.Prune(5, quietLogger()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("nothing should be deleted below the retention count, got %v", objects)
	}
}

func TestObjectStoreDelete(t *testing.T) {
	objects := map[string][]byte{"obj": {1}}
	srv, _ := ociMock(t, objects)
	store := newTestStore(t, srv.URL)

	if err := store.Delete("obj"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := objects["obj"]; ok {
		t.Fatal("object still present after Delete")
	}
}

// A non-2xx response must surface as an error rather than being treated as a
// successful upload — otherwise a failed backup is reported as good.
func TestObjectStoreHTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "NotAuthenticated")
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	err := store.Put("obj", []byte("x"))
	if err == nil {
		t.Fatal("HTTP 403 must be reported as an error, not a successful upload")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should include the status code, got %v", err)
	}
}

// Every request must carry an OCI Signature Authorization header; an unsigned
// request would be rejected by the real service.
func TestObjectStoreSignsRequests(t *testing.T) {
	var auth, xContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		xContent = r.Header.Get("x-content-sha256")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	if err := store.Put("obj", []byte("payload")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasPrefix(auth, "Signature ") {
		t.Fatalf("request was not signed: Authorization=%q", auth)
	}
	for _, want := range []string{`algorithm="rsa-sha256"`, `version="1"`, "keyId="} {
		if !strings.Contains(auth, want) {
			t.Errorf("signature header missing %s: %q", want, auth)
		}
	}
	// PUT must additionally sign the body digest.
	if xContent == "" {
		t.Error("PUT must set x-content-sha256 so the body is covered by the signature")
	}
}

// The signed Host must remain the real OCI host even when the endpoint is
// redirected, since the signature covers the host header.
func TestObjectStoreSignsRealHostNotOverride(t *testing.T) {
	var gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	store := newTestStore(t, srv.URL)

	if err := store.Put("obj", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotHost != store.host() {
		t.Fatalf("signed Host should be the OCI host %q, got %q", store.host(), gotHost)
	}
}

func TestObjectStoreBaseURLDefaultsToRealHost(t *testing.T) {
	store := newTestStore(t, "")
	want := "https://" + store.host()
	if got := store.baseURL(); got != want {
		t.Fatalf("without an override the store must target %q, got %q", want, got)
	}
}

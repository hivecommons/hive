package hub

import (
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// ============================================================
// oci_fss.go
// ============================================================

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa key: %v", err)
	}
	return k
}

// injectOCIConfig forces loadOCIConfig() to return cfg by pre-populating the
// once-guarded package state, and restores it afterwards.
func injectOCIConfig(t *testing.T, cfg *ociConfig) {
	t.Helper()
	oldCfg, oldErr := ociCfg, ociCfgErr
	ociCfg = cfg
	ociCfgErr = nil
	ociCfgOnce = sync.Once{}
	ociCfgOnce.Do(func() {}) // mark done so loadOCIConfig returns the injected cfg
	t.Cleanup(func() {
		ociCfg = oldCfg
		ociCfgErr = oldErr
		ociCfgOnce = sync.Once{}
	})
}

// pointOCIEndpointAt rewrites ociFSSEndpointFmt so FSS calls hit srv.
func pointOCIEndpointAt(t *testing.T, srv *httptest.Server) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	old := ociFSSEndpointFmt
	// %s is the region; we ignore it and always return the test server origin.
	ociFSSEndpointFmt = u.Scheme + "://" + u.Host + "%.0s"
	t.Cleanup(func() { ociFSSEndpointFmt = old })
}

func TestGetEnvOrDefault(t *testing.T) {
	t.Setenv("OCI_TEST_KEY", "value")
	if got := getEnvOrDefault("OCI_TEST_KEY", "fallback"); got != "value" {
		t.Errorf("set: got %q, want value", got)
	}
	if got := getEnvOrDefault("OCI_MISSING_KEY_XYZ", "fallback"); got != "fallback" {
		t.Errorf("unset: got %q, want fallback", got)
	}
}

func TestOCIKeyID(t *testing.T) {
	cfg := &ociConfig{tenancy: "t", user: "u", fingerprint: "f"}
	if got := ociKeyID(cfg); got != "t/u/f" {
		t.Errorf("ociKeyID = %q, want t/u/f", got)
	}
}

func TestOCISignRequestGET(t *testing.T) {
	cfg := &ociConfig{tenancy: "t", user: "u", fingerprint: "f", privateKey: testRSAKey(t)}
	req, _ := http.NewRequest(http.MethodGet, "https://filestorage.us/x", nil)
	req.Host = req.URL.Host
	if err := ociSignRequest(req, nil, cfg); err != nil {
		t.Fatalf("sign GET: %v", err)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, `algorithm="rsa-sha256"`) || !strings.Contains(auth, `keyId="t/u/f"`) {
		t.Errorf("unexpected Authorization header: %q", auth)
	}
	if req.Header.Get("date") == "" {
		t.Error("date header not set")
	}
}

func TestOCISignRequestPOST(t *testing.T) {
	cfg := &ociConfig{tenancy: "t", user: "u", fingerprint: "f", privateKey: testRSAKey(t)}
	body := []byte(`{"a":1}`)
	req, _ := http.NewRequest(http.MethodPost, "https://filestorage.us/x", strings.NewReader(string(body)))
	req.Host = req.URL.Host
	if err := ociSignRequest(req, body, cfg); err != nil {
		t.Fatalf("sign POST: %v", err)
	}
	if req.Header.Get("x-content-sha256") == "" {
		t.Error("x-content-sha256 not set for POST")
	}
	if req.Header.Get("content-type") != ociContentType {
		t.Errorf("content-type = %q", req.Header.Get("content-type"))
	}
}

func testOCIConfig(t *testing.T) *ociConfig {
	return &ociConfig{
		tenancy: "t", user: "u", fingerprint: "f", privateKey: testRSAKey(t),
		region: "us-test", compartmentID: "c", availDomain: "ad",
		mountTargetID: "mt", exportSetID: "es",
	}
}

func TestCreateOCIFileSystemSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"ocid1.filesystem.test"}`))
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	id, err := createOCIFileSystem("my-fs", slog.Default())
	if err != nil {
		t.Fatalf("createOCIFileSystem: %v", err)
	}
	if id != "ocid1.filesystem.test" {
		t.Errorf("id = %q", id)
	}
}

func TestCreateOCIFileSystemAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"denied"}`))
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if _, err := createOCIFileSystem("my-fs", slog.Default()); err == nil {
		t.Error("expected error on non-2xx")
	}
}

func TestCreateOCIExportSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"ocid1.export.test"}`))
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	id, err := createOCIExport("fs-id", "/exp", slog.Default())
	if err != nil || id != "ocid1.export.test" {
		t.Fatalf("createOCIExport: id=%q err=%v", id, err)
	}
}

func TestCreateOCIExportMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`)) // no id
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if _, err := createOCIExport("fs-id", "/exp", slog.Default()); err == nil {
		t.Error("expected error when response missing export ID")
	}
}

func TestDeleteOCIExport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if err := deleteOCIExport("exp-id", slog.Default()); err != nil {
		t.Fatalf("deleteOCIExport: %v", err)
	}
}

func TestDeleteOCIExportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if err := deleteOCIExport("exp-id", slog.Default()); err == nil {
		t.Error("expected error on non-2xx delete")
	}
}

func TestDeleteOCIFileSystem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if err := deleteOCIFileSystem("fs-id", slog.Default()); err != nil {
		t.Fatalf("deleteOCIFileSystem: %v", err)
	}
}

func TestDeleteOCIFileSystemError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	injectOCIConfig(t, testOCIConfig(t))
	pointOCIEndpointAt(t, srv)

	if err := deleteOCIFileSystem("fs-id", slog.Default()); err == nil {
		t.Error("expected error on non-2xx delete")
	}
}

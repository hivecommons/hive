package dashboard

// Tests pinning the storeLiteLLMAPIKey PVC-failure fallback semantics
// (litellm_key_store.go). The happy path (PVC write succeeds) is covered
// elsewhere; these tests pin the three uncovered outcomes when the PVC
// write fails:
//
//  1. Secret patch succeeds  → key IS persisted; api_key_file must be the
//     Secret mount path (config.DefaultLiteLLMAPIKeyFile), not the PVC path.
//  2. Not in cluster         → nothing persisted; the PVC error surfaces.
//  3. Secret patch also fails → both stores failed; the PVC error surfaces.
//
// They also pin the secrecy invariant documented in the file header: the
// key value must never appear in returned errors, and the Secret patch
// must send the key only to the in-cluster API (base64 in the PATCH body).

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func fallbackTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// breakPVCKeyFile points writableLiteLLMKeyFile at a path whose parent is a
// regular file, so writeLiteLLMKeyFile fails (ENOTDIR from MkdirAll) no
// matter what uid the tests run as. Restores the original on cleanup.
func breakPVCKeyFile(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	orig := writableLiteLLMKeyFile
	writableLiteLLMKeyFile = filepath.Join(blocker, "secrets", "litellm_api_key")
	t.Cleanup(func() { writableLiteLLMKeyFile = orig })
}

// fakeKubeAPI stands up a TLS server impersonating the in-cluster API and
// wires the serviceaccount dir + KUBERNETES_SERVICE_* env at it. status is
// what the Secret PATCH responds with. Returns a pointer to the last
// request body seen (nil until a PATCH arrives).
func fakeKubeAPI(t *testing.T, status int) (gotBody *[]byte, gotReq *http.Request) {
	t.Helper()
	var body []byte
	var req http.Request
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = b
		req = *r
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	saDir := t.TempDir()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if !x509.NewCertPool().AppendCertsFromPEM(certPEM) {
		t.Fatalf("test server certificate did not encode to a usable PEM")
	}
	for name, content := range map[string]string{
		"token":     "sa-token-value\n",
		"namespace": "hive-test-ns\n",
		"ca.crt":    string(certPEM),
	} {
		if err := os.WriteFile(filepath.Join(saDir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("writing serviceaccount %s: %v", name, err)
		}
	}
	origSA := serviceAccountDir
	serviceAccountDir = saDir
	t.Cleanup(func() { serviceAccountDir = origSA })

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", u.Hostname())
	t.Setenv("KUBERNETES_SERVICE_PORT", u.Port())
	return &body, &req
}

// PVC write fails but the hive-secrets Secret patch succeeds: the save must
// succeed and record the Secret mount path, and the PATCH must carry the
// base64 key to the namespace-scoped hive-secrets resource with the SA token.
func TestStoreLiteLLMAPIKey_PVCFailsSecretFallbackSucceeds(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakPVCKeyFile(t)
	body, req := fakeKubeAPI(t, http.StatusOK)

	const key = "sk-fallback-secret-value"
	path, err := s.storeLiteLLMAPIKey(key)
	if err != nil {
		t.Fatalf("storeLiteLLMAPIKey should succeed via the Secret fallback, got %v", err)
	}
	if path != config.DefaultLiteLLMAPIKeyFile {
		t.Errorf("api_key_file = %q, want Secret mount path %q", path, config.DefaultLiteLLMAPIKeyFile)
	}

	if req.Method != http.MethodPatch {
		t.Errorf("Secret patch method = %q, want PATCH", req.Method)
	}
	wantPath := "/api/v1/namespaces/hive-test-ns/secrets/hive-secrets"
	if req.URL.Path != wantPath {
		t.Errorf("Secret patch path = %q, want %q", req.URL.Path, wantPath)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sa-token-value" {
		t.Errorf("Authorization = %q, want the trimmed SA token", got)
	}

	var patch struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(*body, &patch); err != nil {
		t.Fatalf("patch body is not JSON: %v (%q)", err, *body)
	}
	if got := patch.Data["litellm_api_key"]; got != base64.StdEncoding.EncodeToString([]byte(key)) {
		t.Errorf("patch data[litellm_api_key] = %q, want base64 of the key", got)
	}
}

// PVC write fails and we are not in a cluster: nothing persisted the key, so
// the save must fail with the PVC error — and never leak the key value.
func TestStoreLiteLLMAPIKey_PVCFailsNotInCluster(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakPVCKeyFile(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	const key = "sk-must-not-leak-anywhere"
	path, err := s.storeLiteLLMAPIKey(key)
	if err == nil {
		t.Fatalf("storeLiteLLMAPIKey should fail when no store persisted the key (path=%q)", path)
	}
	if path != "" {
		t.Errorf("api_key_file = %q on failure, want empty", path)
	}
	if !errors.Is(err, fs.ErrPermission) && !strings.Contains(err.Error(), filepath.Dir(writableLiteLLMKeyFile)) {
		t.Errorf("error should surface the PVC store path %q, got %v", filepath.Dir(writableLiteLLMKeyFile), err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error leaks the key value: %v", err)
	}
}

// PVC write fails AND the Secret patch is rejected (e.g. missing the
// hive-secrets-writer Role): both stores failed, the PVC error surfaces,
// and neither the key value nor the API error body leaks.
func TestStoreLiteLLMAPIKey_BothStoresFail(t *testing.T) {
	s := NewServer(0, fallbackTestLogger())
	breakPVCKeyFile(t)
	fakeKubeAPI(t, http.StatusForbidden)

	const key = "sk-both-stores-fail"
	path, err := s.storeLiteLLMAPIKey(key)
	if err == nil {
		t.Fatalf("storeLiteLLMAPIKey should fail when both stores fail (path=%q)", path)
	}
	if path != "" {
		t.Errorf("api_key_file = %q on failure, want empty", path)
	}
	if !strings.Contains(err.Error(), filepath.Dir(writableLiteLLMKeyFile)) {
		t.Errorf("error should surface the PVC (primary store) path, got %v", err)
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error leaks the key value: %v", err)
	}
}

// A rejected patch must produce a status-only error (API bodies can echo
// patch contents), and a 2xx patch reports success.
func TestPatchKeyIntoHiveSecrets_StatusOnlyErrors(t *testing.T) {
	fakeKubeAPI(t, http.StatusForbidden)
	err := patchKeyIntoHiveSecrets(litellmSecretDataKey, "sk-status-only")
	if err == nil {
		t.Fatal("expected an error for a 403 patch response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "hive-secrets-writer") {
		t.Errorf("expected a status-only actionable error, got %v", err)
	}
	if strings.Contains(err.Error(), "sk-status-only") ||
		strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("sk-status-only"))) {
		t.Errorf("patch error leaks the key value: %v", err)
	}
}

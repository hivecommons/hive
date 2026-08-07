package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyInstallationForOrg(t *testing.T) {
	tests := []struct {
		name       string
		account    string
		id         int64
		wantErrSub string
	}{
		{name: "matches org case insensitively", account: "MyOrg", id: 4242},
		{name: "rejects mismatched org", account: "OtherOrg", id: 4242, wantErrSub: "no installation"},
		{name: "rejects nonpositive id", account: "MyOrg", id: 0, wantErrSub: "positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/app/installations/4242" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 4242,
					"account": map[string]any{
						"login": tt.account,
						"type":  "Organization",
					},
				})
			}))
			defer api.Close()

			auth := newVerifyTestAppAuth(t, 1234, 0, api.URL)
			err := auth.VerifyInstallationForOrg(context.Background(), tt.id, "myorg")
			if tt.wantErrSub == "" {
				if err != nil {
					t.Fatalf("VerifyInstallationForOrg: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
				t.Fatalf("VerifyInstallationForOrg error = %v, want substring %q", err, tt.wantErrSub)
			}
		})
	}
}

func TestVerifyInstallationForOrgRejectsInvalidInputs(t *testing.T) {
	if err := (*AppAuth)(nil).VerifyInstallationForOrg(context.Background(), 1, "myorg"); err == nil || !strings.Contains(err.Error(), "no app auth") {
		t.Fatalf("nil auth error = %v, want no app auth", err)
	}
	auth := &AppAuth{}
	if err := auth.VerifyInstallationForOrg(context.Background(), 1, "myorg"); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("missing key error = %v, want private key", err)
	}
	auth = newVerifyTestAppAuth(t, 1234, 0, "https://api.github.com")
	if err := auth.VerifyInstallationForOrg(context.Background(), 1, " "); err == nil || !strings.Contains(err.Error(), "target org") {
		t.Fatalf("empty org error = %v, want target org", err)
	}
}

func TestVerifyInstallationForOrgRejectsMissingInstallation(t *testing.T) {
	api := httptest.NewServer(http.NotFoundHandler())
	defer api.Close()

	auth := newVerifyTestAppAuth(t, 1234, 0, api.URL)
	err := auth.VerifyInstallationForOrg(context.Background(), 9999, "myorg")
	if err == nil || !strings.Contains(err.Error(), "getting app installation 9999") {
		t.Fatalf("missing installation error = %v, want lookup error", err)
	}
}

func newVerifyTestAppAuth(t *testing.T, appID, installationID int64, apiURL string) *AppAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "gh-app-key.pem")
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("creating key: %v", err)
	}
	if err := pem.Encode(f, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		_ = f.Close()
		t.Fatalf("writing key: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing key: %v", err)
	}
	auth, err := NewAppAuth(appID, installationID, keyPath, slog.New(slog.NewTextHandler(os.Stderr, nil)), apiURL)
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	return auth
}

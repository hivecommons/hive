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
	"strings"
	"testing"
)

func testAppPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestNewAppAuthFromPEMAndAccessors(t *testing.T) {
	auth, err := NewAppAuthFromPEM(1234, 5678, testAppPrivateKeyPEM(t), slog.Default(), "https://github.ibm.com/api/v3")
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	if auth.AppID() != 1234 {
		t.Fatalf("AppID = %d, want 1234", auth.AppID())
	}
	if auth.InstallationID() != 5678 {
		t.Fatalf("InstallationID = %d, want 5678", auth.InstallationID())
	}
	if auth.APIURL() != "https://github.ibm.com/api/v3" {
		t.Fatalf("APIURL = %q", auth.APIURL())
	}
	if !auth.HasKey() {
		t.Fatal("HasKey = false, want true")
	}
}

func TestNewAppAuthFromPEMRejectsInvalidKey(t *testing.T) {
	if _, err := NewAppAuthFromPEM(1, 2, "not pem", slog.Default(), ""); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("NewAppAuthFromPEM invalid key error = %v, want PEM error", err)
	}
}

func TestInstallationAccountLoginRejectsInvalidInputs(t *testing.T) {
	if _, err := (*AppAuth)(nil).InstallationAccountLogin(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "no app auth") {
		t.Fatalf("nil auth error = %v", err)
	}
	if _, err := (&AppAuth{}).InstallationAccountLogin(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("missing key error = %v", err)
	}
	auth, err := NewAppAuthFromPEM(1, 0, testAppPrivateKeyPEM(t), slog.Default(), "")
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	if _, err := auth.InstallationAccountLogin(context.Background(), 0); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("bad installation ID error = %v", err)
	}
}

func TestInstallationAccountLoginReturnsAccount(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations/42" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42,
			"account": map[string]any{
				"login": "Lee-Cooper",
				"type":  "Organization",
			},
		})
	}))
	defer api.Close()
	auth, err := NewAppAuthFromPEM(1, 0, testAppPrivateKeyPEM(t), slog.Default(), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	login, err := auth.InstallationAccountLogin(context.Background(), 42)
	if err != nil {
		t.Fatalf("InstallationAccountLogin: %v", err)
	}
	if login != "Lee-Cooper" {
		t.Fatalf("login = %q, want Lee-Cooper", login)
	}
}

func TestForgetInstallationDiscoveryRemovesCachedEntry(t *testing.T) {
	ResetInstallationDiscoveryCache()
	auth := &AppAuth{appID: 99, apiURL: "https://github.ibm.com/api/v3"}
	key := discoveryCacheKey(auth.apiURL, auth.appID, "Lee-Cooper")
	discoveryCacheMu.Lock()
	discoveryCache[key] = discoveryCacheEntry{id: 42}
	discoveryCacheMu.Unlock()

	auth.ForgetInstallationDiscovery("Lee-Cooper")

	discoveryCacheMu.Lock()
	_, ok := discoveryCache[key]
	discoveryCacheMu.Unlock()
	if ok {
		t.Fatalf("cache entry %q still present after ForgetInstallationDiscovery", key)
	}
	(*AppAuth)(nil).ForgetInstallationDiscovery("Lee-Cooper")
}

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestNewAppAuth_ValidKey(t *testing.T) {
	// Generate a test RSA key
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	// Write PKCS1 PEM to temp file
	keyBytes := x509.MarshalPKCS1PrivateKey(key)
	pemBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes}

	tmpFile, err := os.CreateTemp("", "app-test-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if err := pem.Encode(tmpFile, pemBlock); err != nil {
		t.Fatalf("writing PEM: %v", err)
	}
	tmpFile.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := NewAppAuth(12345, 67890, tmpFile.Name(), logger, "")
	if err != nil {
		t.Fatalf("NewAppAuth: %v", err)
	}
	if auth == nil {
		t.Fatal("auth is nil")
	}
	if auth.appID != 12345 {
		t.Errorf("appID = %d", auth.appID)
	}
	if auth.installationID != 67890 {
		t.Errorf("installationID = %d", auth.installationID)
	}
}

func TestNewAppAuth_PKCS8Key(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling PKCS8: %v", err)
	}
	pemBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}

	tmpFile, err := os.CreateTemp("", "app-test-pkcs8-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if err := pem.Encode(tmpFile, pemBlock); err != nil {
		t.Fatalf("writing PEM: %v", err)
	}
	tmpFile.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := NewAppAuth(1, 2, tmpFile.Name(), logger, "")
	if err != nil {
		t.Fatalf("NewAppAuth PKCS8: %v", err)
	}
	if auth == nil {
		t.Fatal("auth nil")
	}
}

func TestNewAppAuth_FileNotFound(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err := NewAppAuth(1, 2, "/nonexistent/key.pem", logger, "")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestNewAppAuth_NoPEMBlock(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "app-test-bad-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	tmpFile.WriteString("not a pem block")
	tmpFile.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err = NewAppAuth(1, 2, tmpFile.Name(), logger, "")
	if err == nil {
		t.Error("expected error for invalid PEM")
	}
}

func TestNewAppAuth_InvalidKeyBytes(t *testing.T) {
	pemBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("garbage")}

	tmpFile, err := os.CreateTemp("", "app-test-garbage-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	pem.Encode(tmpFile, pemBlock)
	tmpFile.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	_, err = NewAppAuth(1, 2, tmpFile.Name(), logger, "")
	if err == nil {
		t.Error("expected error for garbage key bytes")
	}
}

func TestGenerateJWT(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          12345,
		installationID: 67890,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	token, err := auth.generateJWT()
	if err != nil {
		t.Fatalf("generateJWT: %v", err)
	}
	if token == "" {
		t.Error("empty JWT")
	}
}

func TestNewClientFromApp(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	client := NewClientFromApp(auth, "myorg", []string{"repo1"}, logger)
	if client == nil {
		t.Fatal("nil client")
	}
	if client.org != "myorg" {
		t.Errorf("org = %q", client.org)
	}
}

func TestToken_CachedValid(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		cachedToken:    "cached-token-value",
		tokenExpiry:    time.Now().Add(time.Hour), // still valid
	}

	token, err := auth.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "cached-token-value" {
		t.Errorf("token = %q, want cached-token-value", token)
	}
}

func TestToken_ExpiredCacheHitsAPI(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		cachedToken:    "old-token",
		tokenExpiry:    time.Now().Add(-time.Hour), // expired
	}

	// Token with expired cache will try to call GitHub API, which will fail
	// since we don't have a real installation. We just verify it goes through the path.
	_, err := auth.Token(context.Background())
	if err == nil {
		t.Error("expected error for expired token without real API")
	}
}

func TestRoundTrip_WithCachedToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		cachedToken:    "test-token-for-roundtrip",
		tokenExpiry:    time.Now().Add(time.Hour),
	}

	var capturedAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	transport := &appTransport{
		auth: auth,
		base: http.DefaultTransport,
	}

	req := httptest.NewRequest("GET", backend.URL+"/test", nil)
	// httptest.NewRequest sets body but not the URL scheme for actual transport
	req.RequestURI = ""
	req.URL.Scheme = "http"
	req.URL.Host = backend.Listener.Addr().String()

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()

	if capturedAuth != "Bearer test-token-for-roundtrip" {
		t.Errorf("auth = %q", capturedAuth)
	}
}

func TestRoundTrip_TokenError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		cachedToken:    "expired",
		tokenExpiry:    time.Now().Add(-time.Hour),
	}

	transport := &appTransport{
		auth: auth,
		base: http.DefaultTransport,
	}

	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	req.RequestURI = ""
	_, err := transport.RoundTrip(req)
	if err == nil {
		t.Error("expected error from RoundTrip when token fails")
	}
}

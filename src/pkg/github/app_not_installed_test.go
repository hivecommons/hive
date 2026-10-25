package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

// installation_id 0 must fail before any request leaves: GitHub's 404 for
// POST /app/installations/0/access_tokens otherwise reads as a repo-scope
// failure (#14002).
func TestScopedTokenForRepos_InstallationZeroIsAppNotInstalled(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := NewAppAuthFromPEM(1, 0, pemBytes, logger, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	_, err = auth.ScopedTokenForRepos(context.Background(), "contributor", []string{"docling-pipelines"})
	if !errors.Is(err, ErrAppNotInstalled) {
		t.Fatalf("want ErrAppNotInstalled, got %v", err)
	}
	if IsRepoScopeMintError(err) {
		t.Fatal("a missing installation must not classify as a repo-scope mint error")
	}
	if requests.Load() != 0 {
		t.Fatalf("expected no GitHub request, got %d", requests.Load())
	}
}

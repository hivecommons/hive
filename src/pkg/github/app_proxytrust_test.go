package github

import (
	"context"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewClientFromAppUsesReloadingProxyTrustForAPICalls(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	withProxyCAPath(t, caPath)

	// Simulate a fresh PVC: the process/client exists before the proxy has
	// generated /data/proxy-ca.pem, so any transport captured now has only
	// system roots and will never trust the proxy cert.
	_ = sharedProxyTrust.sharedTransport()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer cached-installation-token"; got != want {
			t.Fatalf("Authorization header = %q, want %q", got, want)
		}
		if r.URL.Path != "/repos/owner/repo" {
			t.Fatalf("path = %q, want /repos/owner/repo", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"repo","full_name":"owner/repo","has_issues":true}`)
	}))
	srv.StartTLS()
	defer srv.Close()

	auth := &AppAuth{
		cachedToken:    "cached-installation-token",
		tokenExpiry:    time.Now().Add(2 * time.Hour),
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		installationID: 123,
		apiURL:         srv.URL + "/",
	}
	client := NewClientFromApp(auth, "owner", []string{"repo"}, auth.logger)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o644); err != nil {
		t.Fatalf("write proxy CA: %v", err)
	}
	sharedProxyTrust.mu.Lock()
	sharedProxyTrust.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	sharedProxyTrust.mu.Unlock()

	repo, _, err := client.GoGitHub().Repositories.Get(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("Repositories.Get should trust proxy CA written after client construction: %v", err)
	}
	if repo.GetFullName() != "owner/repo" {
		t.Fatalf("repo full_name = %q, want owner/repo", repo.GetFullName())
	}
}

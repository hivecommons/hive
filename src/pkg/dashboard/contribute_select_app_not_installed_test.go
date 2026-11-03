package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// newUninstalledAppAuth is an AppAuth with installation_id 0 — the state a
// hive is in when its GitHub App is not installed on the org (#14002). The
// server records every request so the test can prove none was made.
func newUninstalledAppAuth(t *testing.T) (*ghpkg.AppAuth, *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyFile := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth, err := ghpkg.NewAppAuthWithCache(1, 0, keyFile, filepath.Join(t.TempDir(), "token.cache"), logger, srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthWithCache: %v", err)
	}
	return auth, &requests
}

// An App with no installation used to surface as repo_unmintable (GitHub 404s
// installation 0 and the repo-scope classifier takes any 404), which told the
// contributor to fix the repo list. It must now say app_not_installed, carry
// a message naming the org and install link, exclude no repo, and never call
// GitHub.
func TestSelectTask_AppNotInstalledIsNamedNotRepoUnmintable(t *testing.T) {
	hub, s := covK2Hub(t)
	oneActionableIssue(s)
	s.deps.Config.Project.Org = "docling-project"
	auth, requests := newUninstalledAppAuth(t)
	s.deps.GHAppAuth = auth

	msg := hub.selectTask(lockScopeConn(hub, "dave", "c-dave"))
	if msg == nil || msg.Type != "task_unavailable" || msg.Reason != taskUnavailableAppNotInstalled {
		t.Fatalf("expected task_unavailable/%s, got %+v", taskUnavailableAppNotInstalled, msg)
	}
	for _, want := range []string{"docling-project", "not installed", "installations/new", "Nothing is wrong on your side"} {
		if !strings.Contains(msg.Message, want) {
			t.Errorf("message %q lacks %q", msg.Message, want)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("no GitHub request may be made for installation 0, got %d", requests.Load())
	}
	if hub.repoUnmintable("myorg/repo1", time.Now()) {
		t.Fatal("a missing installation is not a repo-scoped failure and must not exclude the repo")
	}
}

func TestAppNotInstalledMessage_WithoutOrgOrURL(t *testing.T) {
	got := appNotInstalledMessage("", "")
	if !strings.Contains(got, "its org") || strings.Contains(got, " at ") {
		t.Fatalf("unexpected message without org/url: %q", got)
	}
}

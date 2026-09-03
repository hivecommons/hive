package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// healTestAppAuthPEM returns a throwaway RSA private key PEM good enough to
// construct an *github.AppAuth via NewAppAuthFromPEM; it never signs anything
// verified against a real GitHub App.
func healTestAppAuthPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

// healGitHubAppInstallation must be a pure no-op — no panic, no API call —
// for every combination of nil/keyless/orgless inputs. Passing a nil logger
// where the real caller always supplies one would still be safe, but every
// call site does supply one, so these use restoreTestLogger() throughout and
// rely on a nil appAuth/cfg/org short-circuiting before the logger is ever
// touched.
func TestHealGitHubAppInstallationGuardsNoOp(t *testing.T) {
	logger := restoreTestLogger()
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme"}}

	t.Run("nil appAuth", func(t *testing.T) {
		healGitHubAppInstallation(context.Background(), nil, cfg, logger)
	})

	t.Run("keyless appAuth", func(t *testing.T) {
		// NewAppAuth with no key file resolves no key, so HasKey() is false
		// and the function must return before touching cfg at all — nil cfg
		// proves it never dereferences cfg.Project.Org on this path.
		auth, err := github.NewAppAuth(1, 2, "/nonexistent/key.pem", logger, "")
		if err == nil && auth.HasKey() {
			t.Fatal("test setup: expected a keyless AppAuth")
		}
		healGitHubAppInstallation(context.Background(), auth, nil, logger)
	})

	t.Run("nil cfg with keyed appAuth", func(t *testing.T) {
		auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), logger, "")
		if err != nil {
			t.Fatalf("NewAppAuthFromPEM: %v", err)
		}
		healGitHubAppInstallation(context.Background(), auth, nil, logger)
	})

	t.Run("empty org", func(t *testing.T) {
		auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), logger, "")
		if err != nil {
			t.Fatalf("NewAppAuthFromPEM: %v", err)
		}
		emptyOrgCfg := &config.Config{Project: config.ProjectConfig{Org: ""}}
		healGitHubAppInstallation(context.Background(), auth, emptyOrgCfg, logger)
	})
}

// A VerifyInstallation failure (unreachable/erroring API) must be swallowed:
// healGitHubAppInstallation logs and returns rather than propagating, since
// the self-heal tick runs unattended on every heartbeat and a transient API
// error must never be treated as fatal.
func TestHealGitHubAppInstallationVerifyErrorIsSwallowed(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()

	auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), slog.Default(), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme"}}

	// Must return normally (no panic) even though every API call 500s.
	healGitHubAppInstallation(context.Background(), auth, cfg, restoreTestLogger())
}

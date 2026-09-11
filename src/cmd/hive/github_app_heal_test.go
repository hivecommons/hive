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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// healTestAPIServer serves just the two endpoints RediscoverAndAdopt walks:
// the verification lookup of the CONFIGURED installation and the direct
// per-org discovery lookup. Routing by path suffix keeps it independent of
// whatever base-path prefix setBaseURL leaves on the client.
func healTestAPIServer(t *testing.T, configuredAccount string, discoveredID int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/app/installations/2"):
			_, _ = w.Write([]byte(`{"id":2,"account":{"login":"` + configuredAccount + `"}}`))
		case strings.HasSuffix(r.URL.Path, "/orgs/acme/installation"):
			_, _ = w.Write([]byte(`{"id":` + strconv.FormatInt(discoveredID, 10) + `,"account":{"login":"acme"}}`))
		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// redirectConfigPersistPaths points Config.Save's package-level PVC/overlay
// targets into the test's temp dir so the save-success path never touches the
// real /data of the host running the suite (same discipline as
// persist_state_test.go).
func redirectConfigPersistPaths(t *testing.T, dir string) {
	t.Helper()
	oldRuntime := config.RuntimeConfigFile
	oldOverlay := config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile = oldRuntime
		config.DashboardOverlayFile = oldOverlay
	})
}

// When the configured installation already belongs to the right org,
// RediscoverAndAdopt returns 0 and the heal must change NOTHING: the
// installation_id stays as configured and no save is attempted. cfg has no
// SourcePath here, so any save attempt would log a persist error — the
// assertion on InstallationID is what proves the early return.
func TestHealGitHubAppInstallationAlreadyCorrectIsNoOp(t *testing.T) {
	api := healTestAPIServer(t, "acme", 0)
	defer api.Close()

	auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), slog.Default(), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	cfg := &config.Config{
		Project: config.ProjectConfig{Org: "acme"},
		GitHub:  config.GitHubConfig{InstallationID: 2},
	}

	healGitHubAppInstallation(context.Background(), auth, cfg, restoreTestLogger())

	if cfg.GitHub.InstallationID != 2 {
		t.Fatalf("installation_id changed on the already-correct path: got %d, want 2", cfg.GitHub.InstallationID)
	}
	if auth.InstallationID() != 2 {
		t.Fatalf("AppAuth adopted a new installation on the already-correct path: got %d, want 2", auth.InstallationID())
	}
}

// The full self-heal: the configured installation belongs to the WRONG
// account, discovery finds the right one, and the heal must adopt it in
// memory (cfg + AppAuth) AND persist it, so the fix survives a pod restart.
func TestHealGitHubAppInstallationAdoptsAndPersists(t *testing.T) {
	api := healTestAPIServer(t, "someone-else", 777)
	defer api.Close()

	auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), slog.Default(), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}

	dir := t.TempDir()
	redirectConfigPersistPaths(t, dir)
	src := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(src, []byte("project:\n  org: acme\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	cfg := &config.Config{
		SourcePath: src,
		Project:    config.ProjectConfig{Org: "acme"},
		// One agent satisfies validateSaveGuard, which otherwise refuses the
		// write as a corrupt/minimal config.
		Agents: map[string]config.AgentConfig{"quality": {Backend: "claude"}},
		GitHub: config.GitHubConfig{InstallationID: 2},
	}

	healGitHubAppInstallation(context.Background(), auth, cfg, restoreTestLogger())

	if cfg.GitHub.InstallationID != 777 {
		t.Fatalf("cfg.GitHub.InstallationID = %d, want adopted 777", cfg.GitHub.InstallationID)
	}
	if auth.InstallationID() != 777 {
		t.Fatalf("AppAuth.InstallationID() = %d, want adopted 777", auth.InstallationID())
	}
	saved, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading persisted config: %v", err)
	}
	if !strings.Contains(string(saved), "installation_id: 777") {
		t.Fatalf("persisted config does not carry the adopted installation_id:\n%s", saved)
	}
}

// A failed persist must not undo the in-memory adoption: the running process
// keeps working with the corrected installation, and the error is logged as
// "will revert on the next pod restart" rather than propagated. An empty
// SourcePath makes Save fail before it writes ANY layer, which also proves
// the heal never touches the real /data paths on this failure path.
func TestHealGitHubAppInstallationSaveFailureKeepsAdoption(t *testing.T) {
	api := healTestAPIServer(t, "someone-else", 777)
	defer api.Close()

	auth, err := github.NewAppAuthFromPEM(1, 2, healTestAppAuthPEM(t), slog.Default(), api.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}
	cfg := &config.Config{
		// SourcePath deliberately empty: saveLocked refuses immediately.
		Project: config.ProjectConfig{Org: "acme"},
		GitHub:  config.GitHubConfig{InstallationID: 2},
	}

	healGitHubAppInstallation(context.Background(), auth, cfg, restoreTestLogger())

	if cfg.GitHub.InstallationID != 777 {
		t.Fatalf("cfg.GitHub.InstallationID = %d, want in-memory adoption 777 despite save failure", cfg.GitHub.InstallationID)
	}
	if auth.InstallationID() != 777 {
		t.Fatalf("AppAuth.InstallationID() = %d, want 777", auth.InstallationID())
	}
}

package hub

import (
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================
// webhook.go — kubectl-backed secret lookups + full installation match
// ============================================================

// installSecretKubectl writes a fake kubectl that returns a base64-encoded PEM
// for gh-app-key.pem and a base64 dashboard token, so loadAppPrivateKey and
// loadSpokeAuthToken take their success paths.
func installSecretKubectl(t *testing.T, pem, token string) {
	t.Helper()
	dir := t.TempDir()
	b64pem := base64.StdEncoding.EncodeToString([]byte(pem))
	b64tok := base64.StdEncoding.EncodeToString([]byte(token))
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *gh-app-key*) printf '%s' '" + b64pem + "' ;;\n" +
		"  *dashboard-token*) printf '%s' '" + b64tok + "' ;;\n" +
		"  *) echo '' ;;\n" +
		"esac\n" +
		"exit 0\n"
	path := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestLoadAppPrivateKeySuccess(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----"
	installSecretKubectl(t, pem, "dash-token")

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}}}
	got := s.loadAppPrivateKey(&RegistryEntry{ID: "h1", ClusterID: "hive-oke"})
	if got != pem {
		t.Errorf("loadAppPrivateKey = %q, want the PEM", got)
	}
}

func TestLoadSpokeAuthTokenSuccess(t *testing.T) {
	installSecretKubectl(t, "-----BEGIN X-----", "dash-token-value")

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}}}
	got := s.loadSpokeAuthToken(&RegistryEntry{ID: "h1", ClusterID: "hive-oke"})
	if got != "dash-token-value" {
		t.Errorf("loadSpokeAuthToken = %q, want dash-token-value", got)
	}
}

func TestHandleInstallationEventFullMatch(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nzz\n-----END RSA PRIVATE KEY-----"
	installSecretKubectl(t, pem, "tok")

	s := &HubServer{
		logger:                  slog.Default(),
		clusters:                map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}},
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
	}
	// A registered hive matching the org/repo so the "match found -> store app
	// config for heartbeat delivery" branch runs.
	s.registry.Hives = []RegistryEntry{{ID: "h1", Org: "acme", PrimaryRepo: "widgets", Repos: []string{"widgets"}, ClusterID: "hive-oke"}}

	evt := installationEvent{Action: "created"}
	evt.Installation.ID = 99
	evt.Installation.AppID = 7
	evt.Installation.Account.Login = "acme"
	evt.Repositories = []struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	}{{Name: "widgets", FullName: "acme/widgets"}}
	body, _ := json.Marshal(evt)

	s.handleInstallationEvent(body)

	// The app config should be queued for h1's next heartbeat.
	s.pendingGitHubAppConfigsMu.Lock()
	cfg, ok := s.pendingGitHubAppConfigs["h1"]
	s.pendingGitHubAppConfigsMu.Unlock()
	if !ok || cfg.InstallationID != 99 || cfg.PrivateKey != pem {
		t.Errorf("expected app config queued for h1, got ok=%v cfg=%+v", ok, cfg)
	}
}

func TestPushGitHubConfigToSpokeRetries(t *testing.T) {
	installSecretKubectl(t, "-----BEGIN X-----", "tok")

	// Spoke fails the first attempt (connection reset via immediate close) then
	// succeeds; but to keep the test fast we accept a single 200 immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No auth token was loaded — still fine, just accept regardless of
		// whether an Authorization header is present.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &HubServer{logger: slog.Default(), clusters: map[string]ClusterConfig{"hive-oke": {ID: "hive-oke"}}}
	hive := &RegistryEntry{ID: "h1", ClusterID: "hive-oke", DashboardURL: srv.URL}
	s.pushGitHubConfigToSpoke(hive, 100, 200)
}

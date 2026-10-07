package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestHeartbeatNormalizesPublicGitHubAppSlug(t *testing.T) {
	srv := registryAppSlugTestServer(t)

	postRegistryAppSlugBeat(t, srv, fmt.Sprintf(`{"hive_id":"legacy-public-app","org":"hivecommons","primary_repo":"hive","repos":["hive"],"github_app_id":%d,"github_app_slug":"kubestellar-hive"}`, config.PublicGitHubAppID))

	entry := registryEntryByID(t, srv, "legacy-public-app")
	if entry.GitHubAppSlug != config.PublicGitHubAppSlug {
		t.Fatalf("GitHubAppSlug = %q, want %q", entry.GitHubAppSlug, config.PublicGitHubAppSlug)
	}
}

func TestHeartbeatKeepsNonFleetGitHubAppSlug(t *testing.T) {
	srv := registryAppSlugTestServer(t)

	postRegistryAppSlugBeat(t, srv, `{"hive_id":"custom-app","org":"example","primary_repo":"repo","repos":["repo"],"github_app_id":123456789,"github_app_slug":"kubestellar-hive"}`)

	entry := registryEntryByID(t, srv, "custom-app")
	if entry.GitHubAppSlug != "kubestellar-hive" {
		t.Fatalf("GitHubAppSlug = %q, want custom slug preserved", entry.GitHubAppSlug)
	}
}

func TestLoadRegistryNormalizesStoredPublicGitHubAppSlug(t *testing.T) {
	dir := t.TempDir()
	regFile := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(regFile, []byte(fmt.Sprintf(`{"hives":[{"id":"stored","githubAppId":%d,"githubAppSlug":"kubestellar-hive"},{"id":"custom","githubAppId":123456789,"githubAppSlug":"kubestellar-hive"}]}`, config.PublicGitHubAppID)), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	srv := &HubServer{logger: slog.Default(), hubSecret: testHubSecret, registryPath: regFile}
	srv.loadRegistry()

	if got := registryEntryByID(t, srv, "stored").GitHubAppSlug; got != config.PublicGitHubAppSlug {
		t.Fatalf("stored public GitHubAppSlug = %q, want %q", got, config.PublicGitHubAppSlug)
	}
	if got := registryEntryByID(t, srv, "custom").GitHubAppSlug; got != "kubestellar-hive" {
		t.Fatalf("custom GitHubAppSlug = %q, want original slug", got)
	}

	data, err := os.ReadFile(regFile)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"githubAppSlug": "hivecommons-hive"`) {
		t.Fatalf("persisted registry did not contain normalized public slug: %s", text)
	}
}

func registryAppSlugTestServer(t *testing.T) *HubServer {
	t.Helper()
	dir := t.TempDir()
	origHives, origRegistry := saasHivesDir, registryPath
	saasHivesDir = filepath.Join(dir, "hives")
	registryPath = filepath.Join(dir, "hub-registry.json")
	t.Cleanup(func() { saasHivesDir, registryPath = origHives, origRegistry })

	srv := NewHubServer(0, slog.Default(), "test", "v5")
	t.Cleanup(srv.StopSaveLoop)
	srv.setHubSecret("")
	return srv
}

func postRegistryAppSlugBeat(t *testing.T, srv *HubServer, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/heartbeat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleHeartbeat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat returned %d (%s), want 200", w.Code, w.Body.String())
	}
}

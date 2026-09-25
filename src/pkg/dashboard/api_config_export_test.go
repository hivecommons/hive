package dashboard

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func configExportFixture(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	agentsDir := filepath.Join(dir, "agent-configs")
	policiesDir := filepath.Join(dir, "policies")
	dataAgentsDir := filepath.Join(dir, "agents")
	restrictionsPath := filepath.Join(dir, "restrictions.conf")
	sidebarPath := filepath.Join(dir, "sidebar.json")
	envPath := filepath.Join(dir, "config.env")

	for _, path := range []string{agentsDir, policiesDir, filepath.Join(dataAgentsDir, "scanner")} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}
	mustWrite := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	mustWrite(seedPath, "project:\n  org: acme\n  name: road-runner\nagents:\n  scanner: {}\ngithub:\n  token: ghp_seededsecret\n")
	mustWrite(overlayPath, "project:\n  org: acme\n  name: road-runner\nagents:\n  scanner:\n    backend: copilot\n")
	mustWrite(filepath.Join(agentsDir, "scanner.yaml"), "backend: claude\nmodel: sonnet\n")
	mustWrite(envPath, "HIVE_TOKEN=rotate-me\nVISIBLE=value\n")
	mustWrite(filepath.Join(policiesDir, "scanner.md"), "template body\n")
	mustWrite(restrictionsPath, "vendor/|global reason\n")
	mustWrite(filepath.Join(dataAgentsDir, "scanner", "restrictions.conf"), "agent-only/|agent reason\n")
	mustWrite(sidebarPath, `{"order":["scanner"]}`)

	t.Setenv("HIVE_CONFIG", seedPath)
	oldOverlay := config.DashboardOverlayFile
	oldPromptDir := promptTemplateSaveDir
	oldRestrictions := configExportRestrictionsFile
	oldAgentsData := configExportAgentsDataDir
	oldSidebar := sidebarFile
	oldNow := configExportNow
	config.DashboardOverlayFile = overlayPath
	promptTemplateSaveDir = policiesDir
	configExportRestrictionsFile = restrictionsPath
	configExportAgentsDataDir = dataAgentsDir
	sidebarFile = sidebarPath
	configExportNow = func() time.Time { return time.Date(2026, 9, 25, 18, 3, 0, 0, time.UTC) }
	t.Cleanup(func() {
		config.DashboardOverlayFile = oldOverlay
		promptTemplateSaveDir = oldPromptDir
		configExportRestrictionsFile = oldRestrictions
		configExportAgentsDataDir = oldAgentsData
		sidebarFile = oldSidebar
		configExportNow = oldNow
	})

	cfg := &config.Config{
		HiveID: "hive/one",
		Project: config.ProjectConfig{
			Org:  "acme",
			Name: "road-runner",
		},
		GitHub: config.GitHubConfig{Token: "ghp_seededsecret"},
		OTel:   config.OTelConfig{Headers: map[string]string{"authorization": "Bearer collector-secret"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet"},
		},
		Data: config.DataConfig{AgentsDir: agentsDir},
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Config: cfg, Logger: slog.Default()}
	return srv, dir
}

func decodeConfigExport(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode export: %v\n%s", err, rec.Body.String())
	}
	return body
}

func TestConfigExportRequiresOwner(t *testing.T) {
	srv, _ := configExportFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	req.Header.Set("X-Hive-Role", "viewer")
	rec := httptest.NewRecorder()
	srv.handleConfigExport(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestConfigExportRedactsSeededToken(t *testing.T) {
	srv, _ := configExportFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	srv.handleConfigExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_seededsecret") || strings.Contains(rec.Body.String(), "rotate-me") || strings.Contains(rec.Body.String(), "collector-secret") {
		t.Fatalf("export leaked a secret:\n%s", rec.Body.String())
	}
	body := decodeConfigExport(t, rec)
	effective := body["effective"].(map[string]any)
	github := effective["github"].(map[string]any)
	token := github["token"].(map[string]any)
	if token["redacted"] != true || len(token["sha256"].(string)) != 12 {
		t.Fatalf("token redaction marker = %#v, want redacted sha12", token)
	}
}

func TestConfigExportDeterministicOrdering(t *testing.T) {
	srv, _ := configExportFixture(t)
	first, err := srv.buildConfigExport()
	if err != nil {
		t.Fatal(err)
	}
	second, err := srv.buildConfigExport()
	if err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	json.NewEncoder(&a).Encode(first)
	json.NewEncoder(&b).Encode(second)
	if a.String() != b.String() {
		t.Fatalf("exports differ:\n%s\n---\n%s", a.String(), b.String())
	}
}

func TestConfigExportIncludesLayersAndMissingReasons(t *testing.T) {
	srv, dir := configExportFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	srv.handleConfigExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeConfigExport(t, rec)
	layers := body["layers"].(map[string]any)
	if !layers["seed"].(map[string]any)["present"].(bool) {
		t.Fatal("seed layer not present")
	}
	if !layers["dashboard_overlay"].(map[string]any)["present"].(bool) {
		t.Fatal("dashboard overlay layer not present")
	}
	agents := layers["agent_overlays"].(map[string]any)
	if !agents["scanner"].(map[string]any)["present"].(bool) {
		t.Fatal("scanner agent overlay not present")
	}
	if !layers["config_env"].(map[string]any)["present"].(bool) {
		t.Fatal("config.env layer not present")
	}
	side := body["side_files"].(map[string]any)
	if !side["global_restrictions"].(map[string]any)["present"].(bool) {
		t.Fatal("global restrictions not present")
	}
	if !side["sidebar"].(map[string]any)["present"].(bool) {
		t.Fatal("sidebar not present")
	}

	configExportRestrictionsFile = filepath.Join(dir, "missing-restrictions.conf")
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	markOwnerRequest(req)
	srv.handleConfigExport(rec, req)
	body = decodeConfigExport(t, rec)
	missing := body["side_files"].(map[string]any)["global_restrictions"].(map[string]any)
	if missing["present"].(bool) || missing["reason"].(string) == "" {
		t.Fatalf("missing restrictions snapshot = %#v, want omitted with reason", missing)
	}
}

func TestConfigExportFilenameHeader(t *testing.T) {
	srv, _ := configExportFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config/export", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	srv.handleConfigExport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get("Content-Disposition")
	want := `attachment; filename="hive-config-hive-one-20260925-1803.json"`
	if got != want {
		t.Fatalf("Content-Disposition = %q, want %q", got, want)
	}
}

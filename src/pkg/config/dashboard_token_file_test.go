package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setDashboardAuthTokenFileForTest(t *testing.T, path string) {
	t.Helper()
	orig := dashboardAuthTokenFile
	dashboardAuthTokenFile = path
	t.Cleanup(func() { dashboardAuthTokenFile = orig })
}

func writeDashboardTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dashboard-token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("writing dashboard token file: %v", err)
	}
	return path
}

func TestLoadDashboardAuthTokenFromFile(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_TOKEN", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	t.Setenv("DASHBOARD_AUTH_TOKEN_FILE", "")
	tokenPath := writeDashboardTokenFile(t, "  file-token\n")
	setDashboardAuthTokenFileForTest(t, tokenPath)

	cfg, err := Load(writeTempConfig(t, minimalValidYAML("acme", "ghp_test")))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Dashboard.AuthToken != "file-token" {
		t.Fatalf("Dashboard.AuthToken = %q, want trimmed file token", cfg.Dashboard.AuthToken)
	}
}

func TestDashboardAuthTokenEnvBeatsFile(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_TOKEN", "env-token")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	t.Setenv("DASHBOARD_AUTH_TOKEN_FILE", "")
	tokenPath := writeDashboardTokenFile(t, "file-token\n")
	setDashboardAuthTokenFileForTest(t, tokenPath)

	cfg, err := Load(writeTempConfig(t, minimalValidYAML("acme", "ghp_test")))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Dashboard.AuthToken != "env-token" {
		t.Fatalf("Dashboard.AuthToken = %q, want env token", cfg.Dashboard.AuthToken)
	}
}

func TestDashboardOverlayBytesScrubsFileSourcedAuthToken(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_TOKEN", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	t.Setenv("DASHBOARD_AUTH_TOKEN_FILE", "")
	tokenPath := writeDashboardTokenFile(t, "file-token\n")
	setDashboardAuthTokenFileForTest(t, tokenPath)

	cfg := &Config{
		Project:   ProjectConfig{Org: "acme"},
		Agents:    map[string]AgentConfig{"scanner": {Role: "scanner"}},
		Dashboard: DashboardConfig{AuthToken: "file-token"},
	}
	data, err := cfg.dashboardOverlayBytes()
	if err != nil {
		t.Fatalf("dashboardOverlayBytes: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "file-token") {
		t.Fatalf("overlay leaked file-sourced dashboard token: %s", body)
	}
	if cfg.Dashboard.AuthToken != "file-token" {
		t.Fatalf("live config token mutated to %q", cfg.Dashboard.AuthToken)
	}
}

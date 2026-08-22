package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new-config.yaml")

	level := 2
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Name: "test"},
		Agents: map[string]AgentConfig{
			"scanner": {Role: "scanner"},
		},
		ACMMLevel: &level,
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save to new file: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if len(data) == 0 {
		t.Error("saved file should not be empty")
	}
}

func TestSaveExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.yaml")
	os.WriteFile(path, []byte("old: content\n"), 0644)

	level := 1
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Name: "test"},
		Agents: map[string]AgentConfig{
			"scanner": {Role: "scanner"},
		},
		ACMMLevel: &level,
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save to existing file: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(data) == "old: content\n" {
		t.Error("file should be overwritten, not contain old content")
	}
}

func TestSaveNoSourcePath(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "testorg"},
		Agents:  map[string]AgentConfig{"scanner": {}},
	}
	err := cfg.Save()
	if err == nil {
		t.Error("should error with no source path")
	}
}

func TestSaveEmptyOrg(t *testing.T) {
	cfg := &Config{
		SourcePath: "/tmp/test.yaml",
		Project:    ProjectConfig{},
		Agents:     map[string]AgentConfig{"scanner": {}},
	}
	err := cfg.Save()
	if err == nil {
		t.Error("should error with empty org")
	}
}

func TestSaveNoAgents(t *testing.T) {
	cfg := &Config{
		SourcePath: "/tmp/test.yaml",
		Project:    ProjectConfig{Org: "testorg"},
		Agents:     map[string]AgentConfig{},
	}
	err := cfg.Save()
	if err == nil {
		t.Error("should error with no agents")
	}
}

func TestSaveReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	readOnlyPath := filepath.Join(dir, "readonly", "config.yaml")

	cfg := &Config{
		SourcePath: readOnlyPath,
		Project:    ProjectConfig{Org: "testorg"},
		Agents:     map[string]AgentConfig{"scanner": {}},
	}
	err := cfg.Save()
	if err == nil {
		t.Error("should error when directory doesn't exist")
	}
}

func TestRemoveAgentFileNonexistent(t *testing.T) {
	dir := t.TempDir()
	err := RemoveAgentFile(dir, "nonexistent-agent")
	// Returns nil — ignores IsNotExist
	if err != nil {
		t.Errorf("should not error for nonexistent file: %v", err)
	}
}

func TestRemoveAgentFileSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-agent.yaml")
	os.WriteFile(path, []byte("test: true\n"), 0644)

	err := RemoveAgentFile(dir, "test-agent")
	if err != nil {
		t.Fatalf("RemoveAgentFile: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("agent yaml file should be removed")
	}
}

// --- dashboard overlay (K8s persistence across pod restarts) ---

func TestSaveWritesDashboardOverlayInK8sMode(t *testing.T) {
	dir := t.TempDir()
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_secrettoken12345")

	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "testorg"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
		GitHub:     GitHubConfig{Token: "ghp_secrettoken12345"},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		t.Fatalf("overlay not written: %v", err)
	}
	content := string(data)
	// Secret-free: the env-derived token is collapsed back to its ${VAR}
	// form so the PVC overlay never holds the raw secret.
	if strings.Contains(content, "ghp_secrettoken12345") {
		t.Fatal("overlay contains the raw github token")
	}
	if !strings.Contains(content, "${HIVE_GITHUB_TOKEN}") {
		t.Error("overlay should reference the token via ${HIVE_GITHUB_TOKEN}")
	}
	if !strings.Contains(content, "testorg") {
		t.Error("overlay missing project org")
	}
	// The live config is untouched by the scrub.
	if cfg.GitHub.Token != "ghp_secrettoken12345" {
		t.Errorf("Save mutated the live config token: %q", cfg.GitHub.Token)
	}
}

func TestSaveSkipsDashboardOverlayOutsideK8s(t *testing.T) {
	dir := t.TempDir()
	origOverlay, origRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	// Keep Save()'s runtime-config write off the live /data too.
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = origOverlay, origRuntime })
	// No KUBERNETES_SERVICE_HOST and (on dev/CI machines) no
	// serviceaccount token file — Docker mode. On a live hive host the
	// SA token file DOES exist, so redirect the probe path too (#4595
	// hermeticity pattern) instead of relying on the host's filesystem.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	origSAToken := saTokenFile
	saTokenFile = filepath.Join(dir, "no-such-sa-token")
	t.Cleanup(func() { saTokenFile = origSAToken })

	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "testorg"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(DashboardOverlayFile); !os.IsNotExist(err) {
		t.Errorf("overlay should not be written outside Kubernetes (stat err: %v)", err)
	}
}

func TestDashboardOverlayBytes_RedactsOTelHeaders(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_TOKEN", "short7")
	cfg := &Config{
		OTel:    OTelConfig{Headers: map[string]string{"authorization": "Bearer short7"}},
		Tracing: OTelConfig{Headers: map[string]string{"x-api-key": "short7"}},
	}

	data, err := cfg.dashboardOverlayBytes()
	if err != nil {
		t.Fatalf("dashboardOverlayBytes() error = %v", err)
	}
	body := string(data)
	if strings.Contains(body, "short7") {
		t.Fatalf("overlay leaked OTLP header secret: %s", body)
	}
	if !strings.Contains(body, "Bearer ${OTEL_EXPORTER_TOKEN}") || !strings.Contains(body, "${OTEL_EXPORTER_TOKEN}") {
		t.Fatalf("overlay did not preserve env reference: %s", body)
	}
}

func TestRedactEnvExpandedValue_LongestMatchFirst(t *testing.T) {
	t.Setenv("OTEL_SHORT", "abc")
	t.Setenv("OTEL_LONG", "abcdef")

	got := redactEnvExpandedValue("Bearer abcdef")
	if got != "Bearer ${OTEL_LONG}" {
		t.Fatalf("redactEnvExpandedValue() = %q, want longest env match", got)
	}
}

func TestSave_RedactsOTelHeadersInSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	t.Setenv("OTEL_EXPORTER_TOKEN", "save-secret")
	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		GitHub:     GitHubConfig{Token: "ghp_test123456789"},
		Agents:     map[string]AgentConfig{"scanner": {Backend: "copilot", Enabled: true}},
		OTel:       OTelConfig{Enabled: true, Headers: map[string]string{"authorization": "Bearer save-secret"}},
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "save-secret") {
		t.Fatalf("saved config leaked OTLP header secret: %s", body)
	}
	if !strings.Contains(body, "Bearer ${OTEL_EXPORTER_TOKEN}") {
		t.Fatalf("saved config did not preserve OTLP env reference: %s", body)
	}
}

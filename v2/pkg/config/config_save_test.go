package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestSaveRestoresAllEnvironmentTemplatesAndWritesExactBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	origBackup := backupFile
	backupFile = filepath.Join(dir, "hive.yaml.bak")
	t.Cleanup(func() { backupFile = origBackup })
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_env_secret_12345")
	t.Setenv("DASHBOARD_AUTH_TOKEN", "dashboard_env_secret_12345")
	t.Setenv("SLACK_WEBHOOK", "https://hooks.example.test/slack-secret")
	t.Setenv("DISCORD_WEBHOOK", "https://hooks.example.test/discord-secret")
	t.Setenv("DISCORD_BOT_TOKEN", "discord-bot-secret")
	t.Setenv("NTFY_TOPIC", "ntfy-secret-topic")
	t.Setenv("DASHBOARD_PORT_TEMPLATE", "3456")

	input := `project:
  org: testorg
  repos: [repo]
github:
  token: ${HIVE_GITHUB_TOKEN}
notifications:
  ntfy:
    server: https://ntfy.sh
    topic: ${NTFY_TOPIC}
  slack:
    webhook: ${SLACK_WEBHOOK}
  discord:
    webhook: ${DISCORD_WEBHOOK}
    bot_token: ${DISCORD_BOT_TOKEN}
    channel_id: channel-1
dashboard:
  port: ${DASHBOARD_PORT_TEMPLATE}
  auth_token: ${DASHBOARD_AUTH_TOKEN}
agents:
  scanner:
    backend: claude
    enabled: true
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides: %v", err)
	}
	// Persistence is bound to load provenance, not the process environment's
	// value at save time.
	t.Setenv("HIVE_GITHUB_TOKEN", "rotated-after-load")
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var reference []byte
	for name, persistedPath := range map[string]string{
		"primary": path,
		"backup":  backupFile,
		"overlay": DashboardOverlayFile,
	} {
		data, err := os.ReadFile(persistedPath)
		if err != nil {
			t.Fatalf("read %s config: %v", name, err)
		}
		if reference == nil {
			reference = data
		} else if !bytes.Equal(data, reference) {
			t.Errorf("%s config differs from the exact primary persistence bytes", name)
		}
		content := string(data)
		for _, secret := range []string{
			"ghp_env_secret_12345",
			"dashboard_env_secret_12345",
			"https://hooks.example.test/slack-secret",
			"https://hooks.example.test/discord-secret",
			"discord-bot-secret",
			"ntfy-secret-topic",
		} {
			if strings.Contains(content, secret) {
				t.Errorf("%s config contains expanded value %q", name, secret)
			}
		}
		for _, template := range []string{
			"${HIVE_GITHUB_TOKEN}",
			"${DASHBOARD_AUTH_TOKEN}",
			"${SLACK_WEBHOOK}",
			"${DISCORD_WEBHOOK}",
			"${DISCORD_BOT_TOKEN}",
			"${NTFY_TOPIC}",
			"${DASHBOARD_PORT_TEMPLATE}",
		} {
			if !strings.Contains(content, template) {
				t.Errorf("%s config lost environment template %q", name, template)
			}
		}
	}

	if cfg.GitHub.Token != "ghp_env_secret_12345" {
		t.Errorf("Save mutated the live github token: %q", cfg.GitHub.Token)
	}
	if cfg.Dashboard.AuthToken != "dashboard_env_secret_12345" {
		t.Errorf("Save mutated the live dashboard token: %q", cfg.Dashboard.AuthToken)
	}
	if cfg.Dashboard.Port != 3456 {
		t.Errorf("Save mutated the live dashboard port: %d", cfg.Dashboard.Port)
	}
	if cfg.Notifications.Slack == nil || cfg.Notifications.Slack.Webhook != "https://hooks.example.test/slack-secret" {
		t.Errorf("Save mutated the live slack webhook: %#v", cfg.Notifications.Slack)
	}
	if cfg.Notifications.Discord == nil || cfg.Notifications.Discord.BotToken != "discord-bot-secret" {
		t.Errorf("Save mutated the live discord config: %#v", cfg.Notifications.Discord)
	}
}

func TestLoadRejectsEnvironmentReferencesInYAMLMergeSources(t *testing.T) {
	t.Setenv("SLACK_WEBHOOK", "expanded-merge-secret")
	cases := map[string]string{
		"direct alias": `slack_defaults: &slack_defaults
  webhook: ${SLACK_WEBHOOK}
project: {org: testorg, repos: [repo]}
github: {token: ghp_test}
notifications:
  slack:
    <<: *slack_defaults
agents:
  scanner: {backend: claude, enabled: true}
`,
		"merge sequence": `secret_defaults: &secret_defaults
  webhook: ${SLACK_WEBHOOK}
literal_defaults: &literal_defaults
  webhook: literal-fallback
project: {org: testorg, repos: [repo]}
github: {token: ghp_test}
notifications:
  slack:
    <<: [*secret_defaults, *literal_defaults]
agents:
  scanner: {backend: claude, enabled: true}
`,
		"nested merge alias": `secret_defaults: &secret_defaults
  webhook: ${SLACK_WEBHOOK}
nested_defaults: &nested_defaults
  <<: *secret_defaults
project: {org: testorg, repos: [repo]}
github: {token: ghp_test}
notifications:
  slack:
    <<: *nested_defaults
agents:
  scanner: {backend: claude, enabled: true}
`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "hive.yaml")
			if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWithOverrides(path, "-")
			if err == nil {
				saveErr := cfg.Save()
				persisted, readErr := os.ReadFile(path)
				t.Fatalf("LoadWithOverrides accepted an env-bearing YAML merge source (Save error=%v, read error=%v, leaked expanded secret=%t)", saveErr, readErr, bytes.Contains(persisted, []byte("expanded-merge-secret")))
			}
			if cfg != nil {
				t.Fatal("LoadWithOverrides returned a config with the merge-source error")
			}
			if !strings.Contains(err.Error(), "YAML merge") || !strings.Contains(err.Error(), "cannot be persisted safely") {
				t.Fatalf("unexpected merge-source error: %v", err)
			}
			persisted, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(persisted, []byte(input)) || bytes.Contains(persisted, []byte("expanded-merge-secret")) {
				t.Fatal("rejected LoadWithOverrides changed or expanded the source config")
			}
		})
	}
}

func TestLoadAllowsLiteralYAMLMergeSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	input := `slack_defaults: &slack_defaults
  webhook: literal-webhook
project: {org: testorg, repos: [repo]}
github: {token: ghp_test}
notifications:
  slack:
    <<: *slack_defaults
agents:
  scanner: {backend: claude, enabled: true}
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatalf("literal YAML merge source was rejected: %v", err)
	}
	data, err := cfg.persistenceBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("literal-webhook")) {
		t.Fatal("literal YAML merge value was not retained")
	}
}

func TestLoadRejectsYAMLAliasCycleWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	input := `cycle: &cycle
  <<: *cycle
project: {org: testorg, repos: [repo]}
github: {token: ghp_test}
agents:
  scanner: {backend: claude, enabled: true}
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	type loadResult struct {
		config *Config
		err    error
	}
	result := make(chan loadResult, 1)
	go func() {
		cfg, err := LoadWithOverrides(path, "-")
		result <- loadResult{config: cfg, err: err}
	}()
	select {
	case loaded := <-result:
		if loaded.config != nil {
			t.Fatal("LoadWithOverrides returned a config for a cyclic YAML alias")
		}
		if loaded.err == nil || !strings.Contains(loaded.err.Error(), "YAML alias cycle") || !strings.Contains(loaded.err.Error(), "cannot be persisted safely") {
			t.Fatalf("unexpected cyclic-alias error: %v", loaded.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("LoadWithOverrides did not return within two seconds for a cyclic YAML alias")
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(persisted, []byte(input)) {
		t.Fatal("rejected cyclic-alias load changed the source config")
	}
}

func TestPersistenceDoesNotRewriteLiteralEqualToEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	t.Setenv("HIVE_GITHUB_TOKEN", "same-token-value")
	input := minimalValidYAML("testorg", "same-token-value")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatal(err)
	}
	data, err := cfg.persistenceBytes()
	if err != nil {
		t.Fatalf("persistenceBytes: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "same-token-value") {
		t.Error("literal token equal to the environment was not preserved")
	}
	if strings.Contains(content, "${HIVE_GITHUB_TOKEN}") {
		t.Error("literal token was incorrectly rewritten from value equality")
	}
}

func TestPersistenceWritesMutatedPlaceholderFieldExplicitly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	t.Setenv("HIVE_GITHUB_TOKEN", "loaded-github-secret")
	t.Setenv("SLACK_WEBHOOK", "loaded-slack-secret")
	input := `project:
  org: testorg
  repos: [repo]
github:
  token: ${HIVE_GITHUB_TOKEN}
notifications:
  slack:
    webhook: ${SLACK_WEBHOOK}
agents:
  scanner:
    backend: claude
    enabled: true
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Notifications.Slack.Webhook = "explicit-new-webhook"
	data, err := cfg.persistenceBytes()
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "explicit-new-webhook") || strings.Contains(content, "${SLACK_WEBHOOK}") {
		t.Fatalf("mutated webhook was not persisted explicitly:\n%s", content)
	}
	if !strings.Contains(content, "${HIVE_GITHUB_TOKEN}") || strings.Contains(content, "loaded-github-secret") {
		t.Fatal("unchanged github token did not retain its proven template")
	}
}

func TestPersistenceTracksDashboardTokenSourceProvenance(t *testing.T) {
	t.Run("config env override", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "hive.yaml")
		envPath := filepath.Join(dir, "config.env")
		t.Setenv("YAML_DASHBOARD_TOKEN", "yaml-expanded-dashboard-secret")
		input := minimalValidYAML("testorg", "ghp_test") + `
dashboard:
  auth_token: ${YAML_DASHBOARD_TOKEN}
`
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(envPath, []byte("DASHBOARD_AUTH_TOKEN=config-env-dashboard-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadWithOverrides(path, envPath)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Dashboard.AuthToken != "config-env-dashboard-secret" {
			t.Fatalf("live dashboard token = %q", cfg.Dashboard.AuthToken)
		}
		data, err := cfg.persistenceBytes()
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if strings.Contains(content, "config-env-dashboard-secret") || strings.Contains(content, "yaml-expanded-dashboard-secret") || !strings.Contains(content, "${YAML_DASHBOARD_TOKEN}") {
			t.Fatalf("config.env token provenance was not restored safely:\n%s", content)
		}
	})

	t.Run("bootstrap env fill", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "hive.yaml")
		t.Setenv("DASHBOARD_AUTH_TOKEN", "bootstrap-dashboard-secret")
		input := minimalValidYAML("testorg", "ghp_test") + `
dashboard:
  auth_token: ""
`
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadWithOverrides(path, "-")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Dashboard.AuthToken != "bootstrap-dashboard-secret" {
			t.Fatalf("live dashboard token = %q", cfg.Dashboard.AuthToken)
		}
		data, err := cfg.persistenceBytes()
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if strings.Contains(content, "bootstrap-dashboard-secret") || !strings.Contains(content, `auth_token: ""`) {
			t.Fatalf("bootstrap token provenance was not restored safely:\n%s", content)
		}
	})
}

func TestSavePersistenceBytesWritesOneFrozenPayload(t *testing.T) {
	dir := t.TempDir()
	origBackup := backupFile
	backupFile = filepath.Join(dir, "hive.yaml.bak")
	t.Cleanup(func() { backupFile = origBackup })
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "before-freeze"},
		Agents:     map[string]AgentConfig{"scanner": {Role: "scanner"}},
	}
	payload, err := cfg.persistencePayload()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Project.Org = "after-freeze"
	if err := cfg.savePersistenceBytes(payload); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"primary": cfg.SourcePath,
		"backup":  backupFile,
		"overlay": DashboardOverlayFile,
	} {
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(actual, payload) {
			t.Errorf("%s did not receive the frozen persistence payload", name)
		}
		if bytes.Contains(actual, []byte("after-freeze")) {
			t.Errorf("%s reserialized the mutated live config", name)
		}
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

	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(minimalValidYAML("testorg", "${HIVE_GITHUB_TOKEN}")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatal(err)
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
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	// No KUBERNETES_SERVICE_HOST and (on dev/CI machines) no
	// serviceaccount token file — Docker mode.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

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

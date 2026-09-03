package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// minimalValidYAML returns the smallest YAML that passes validate().
// It uses a real token so github auth check passes.
func minimalValidYAML(org, token string) string {
	return `
project:
  org: ` + org + `
  repos:
    - repo-a
github:
  token: ` + token + `
agents:
  worker:
    backend: claude
    enabled: true
`
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Load — happy path
// ---------------------------------------------------------------------------

func TestLoad_ValidConfig(t *testing.T) {
	yaml := `
project:
  org: my-org
  repos:
    - my-repo
    - other-repo
  ai_author: bot
  primary_repo: my-repo
github:
  token: ghp_test
agents:
  scanner:
    backend: claude
    model: claude-3-5-sonnet
    enabled: true
dashboard:
  port: 4000
  snapshot_dir: /tmp/snaps
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Project.Org != "my-org" {
		t.Errorf("Project.Org = %q, want %q", cfg.Project.Org, "my-org")
	}
	if len(cfg.Project.Repos) != 2 {
		t.Errorf("len(Project.Repos) = %d, want 2", len(cfg.Project.Repos))
	}
	if cfg.GitHub.Token != "ghp_test" {
		t.Errorf("GitHub.Token = %q, want %q", cfg.GitHub.Token, "ghp_test")
	}
	if cfg.Dashboard.Port != 4000 {
		t.Errorf("Dashboard.Port = %d, want 4000", cfg.Dashboard.Port)
	}
	if cfg.Agents["scanner"].Backend != "claude" {
		t.Errorf("Agents[scanner].Backend = %q, want %q", cfg.Agents["scanner"].Backend, "claude")
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/hive.yaml")
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTempConfig(t, ":::invalid yaml:::")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid YAML, got nil")
	}
}

// ---------------------------------------------------------------------------
// expandEnvVars
// ---------------------------------------------------------------------------

func TestExpandEnvVars_KnownVar(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret123")
	got := expandEnvVars("token: ${MY_TOKEN}")
	want := "token: secret123"
	if got != want {
		t.Errorf("expandEnvVars() = %q, want %q", got, want)
	}
}

func TestExpandEnvVars_UnsetVarLeftAsIs(t *testing.T) {
	// Guarantee the variable is not set.
	os.Unsetenv("DEFINITELY_NOT_SET_XYZ")
	got := expandEnvVars("token: ${DEFINITELY_NOT_SET_XYZ}")
	want := "token: ${DEFINITELY_NOT_SET_XYZ}"
	if got != want {
		t.Errorf("expandEnvVars() = %q, want %q", got, want)
	}
}

func TestExpandEnvVars_MultipleVars(t *testing.T) {
	t.Setenv("ORG", "my-org")
	t.Setenv("REPO", "my-repo")
	got := expandEnvVars("org: ${ORG}\nrepo: ${REPO}")
	want := "org: my-org\nrepo: my-repo"
	if got != want {
		t.Errorf("expandEnvVars() = %q, want %q", got, want)
	}
}

func TestExpandEnvVars_NoPlaceholders(t *testing.T) {
	input := "plain text without placeholders"
	got := expandEnvVars(input)
	if got != input {
		t.Errorf("expandEnvVars() modified input unexpectedly: %q", got)
	}
}

func TestExpandEnvVars_EmptyString(t *testing.T) {
	got := expandEnvVars("")
	if got != "" {
		t.Errorf("expandEnvVars(\"\") = %q, want empty string", got)
	}
}

// ---------------------------------------------------------------------------
// applyDefaults
// ---------------------------------------------------------------------------

func TestApplyDefaults_DashboardPort(t *testing.T) {
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Dashboard.Port != defaultDashboardPort {
		t.Errorf("Dashboard.Port = %d, want %d", cfg.Dashboard.Port, defaultDashboardPort)
	}
}

func TestApplyDefaults_DashboardPortNotOverridden(t *testing.T) {
	yaml := minimalValidYAML("my-org", "ghp_tok") + "\ndashboard:\n  port: 9090\n"
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Dashboard.Port != 9090 {
		t.Errorf("Dashboard.Port = %d, want 9090 (explicit value should not be overwritten)", cfg.Dashboard.Port)
	}
}

func TestApplyDefaults_EvalInterval(t *testing.T) {
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Governor.EvalIntervalS != defaultEvalIntervalS {
		t.Errorf("Governor.EvalIntervalS = %d, want %d", cfg.Governor.EvalIntervalS, defaultEvalIntervalS)
	}
}

func TestApplyDefaults_PollInterval(t *testing.T) {
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := time.Duration(defaultPollIntervalMins) * time.Minute
	if cfg.Policies.PollInterval != want {
		t.Errorf("Policies.PollInterval = %v, want %v", cfg.Policies.PollInterval, want)
	}
}

func TestApplyDefaults_DataDirs(t *testing.T) {
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Data.MetricsDir != "/data/metrics" {
		t.Errorf("Data.MetricsDir = %q, want %q", cfg.Data.MetricsDir, "/data/metrics")
	}
	if cfg.Data.LogsDir != "/data/logs" {
		t.Errorf("Data.LogsDir = %q, want %q", cfg.Data.LogsDir, "/data/logs")
	}
	if cfg.Data.BobSessionsDir != "/data/home/.bob" {
		t.Errorf("Data.BobSessionsDir = %q, want %q", cfg.Data.BobSessionsDir, "/data/home/.bob")
	}
}

func TestApplyDefaults_AgentBeadsDir(t *testing.T) {
	// Agent has no beads_dir set — default should be /data/beads/<name>.
	path := writeTempConfig(t, minimalValidYAML("my-org", "ghp_tok"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Agents["worker"].BeadsDir != "/data/beads/worker" {
		t.Errorf("Agents[worker].BeadsDir = %q, want %q", cfg.Agents["worker"].BeadsDir, "/data/beads/worker")
	}
}

func TestApplyDefaults_AgentEnabled(t *testing.T) {
	// An agent without an explicit enabled field defaults to true.
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Agents["scanner"].Enabled {
		t.Errorf("Agents[scanner].Enabled = false, want true (applyDefaults should set it)")
	}
}

func TestApplyDefaults_AgentExplicitlyDisabled(t *testing.T) {
	// An agent with enabled: false explicitly set stays disabled.
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
    enabled: false
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Agents["scanner"].Enabled {
		t.Errorf("Agents[scanner].Enabled = true, want false (explicit enabled: false must be preserved)")
	}
}

// ---------------------------------------------------------------------------
// validate — missing required fields
// ---------------------------------------------------------------------------

func TestValidate_MissingOrg(t *testing.T) {
	// On a live hive host HIVE_REPO is set and applyBootstrapEnv backfills
	// project.org from it, so validation would pass. Clear it so the test is
	// hermetic (empty means "unset" to applyBootstrapEnv).
	t.Setenv("HIVE_REPO", "")
	yaml := `
project:
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for missing org, got nil")
	}
}

func TestValidate_EmptyRepos(t *testing.T) {
	yaml := `
project:
  org: my-org
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("Load() should allow empty repos (L1 inception), got: %v", err)
	}
}

func TestValidate_NoAgents(t *testing.T) {
	// On a live hive host the default agents_dir (/data/agent-configs) holds
	// real per-agent overlays, which Load merges in — so "no agents" would
	// validate fine. Point agents_dir at an empty temp dir to stay hermetic.
	yaml := fmt.Sprintf(`
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
data:
  agents_dir: %s
`, t.TempDir())
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for no agents, got nil")
	}
}

func TestValidate_NoGitHubAuth(t *testing.T) {
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error when neither github.token nor github.app_id is set, got nil")
	}
}

func TestValidate_GitHubAppIDAccepted(t *testing.T) {
	// app_id without token should be accepted.
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  app_id: 12345
  installation_id: 67890
  key_file: /tmp/key.pem
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err != nil {
		t.Errorf("Load() unexpected error with app_id auth: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validate — invalid backend
// ---------------------------------------------------------------------------

func TestValidate_InvalidBackend(t *testing.T) {
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  w:
    backend: openai
`
	path := writeTempConfig(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid backend, got nil")
	}
}

func TestValidate_ValidBackends(t *testing.T) {
	validBackends := SupportedBackends()
	for _, backend := range validBackends {
		t.Run(backend, func(t *testing.T) {
			yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  w:
    backend: ` + backend + `
`
			path := writeTempConfig(t, yaml)
			_, err := Load(path)
			if err != nil {
				t.Errorf("Load() unexpected error for backend %q: %v", backend, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EnabledAgents
// ---------------------------------------------------------------------------

func TestEnabledAgents_FiltersDisabled(t *testing.T) {
	// After Load(), applyDefaults sets all agents to enabled=true.
	// Test EnabledAgents() directly on a crafted Config where one is disabled.
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"active":   {Backend: "claude", Enabled: true},
			"inactive": {Backend: "copilot", Enabled: false},
		},
	}
	enabled := cfg.EnabledAgents()
	if _, ok := enabled["active"]; !ok {
		t.Error("EnabledAgents() missing 'active' agent")
	}
	if _, ok := enabled["inactive"]; ok {
		t.Error("EnabledAgents() should not include 'inactive' (disabled) agent")
	}
	if len(enabled) != 1 {
		t.Errorf("EnabledAgents() returned %d agents, want 1", len(enabled))
	}
}

func TestEnabledAgents_AllEnabled(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"a": {Backend: "claude", Enabled: true},
			"b": {Backend: "copilot", Enabled: true},
		},
	}
	enabled := cfg.EnabledAgents()
	if len(enabled) != 2 {
		t.Errorf("EnabledAgents() returned %d agents, want 2", len(enabled))
	}
}

func TestEnabledAgents_NoneEnabled(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"a": {Backend: "claude", Enabled: false},
		},
	}
	enabled := cfg.EnabledAgents()
	if len(enabled) != 0 {
		t.Errorf("EnabledAgents() returned %d agents, want 0", len(enabled))
	}
}

// ---------------------------------------------------------------------------
// Env var interpolation via Load()
// ---------------------------------------------------------------------------

func TestLoad_EnvVarInToken(t *testing.T) {
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_from_env")
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ${HIVE_GITHUB_TOKEN}
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.Token != "ghp_from_env" {
		t.Errorf("GitHub.Token = %q, want %q", cfg.GitHub.Token, "ghp_from_env")
	}
}

func TestLoad_UnsetEnvVarInToken_FailsValidation(t *testing.T) {
	// The placeholder remains unexpanded → token stays "${MISSING_TOKEN}" →
	// not empty, so validate() passes. But the raw placeholder string is
	// preserved as the token value. Verify Load succeeds and token holds the
	// unexpanded placeholder (the caller's responsibility to handle).
	os.Unsetenv("MISSING_TOKEN")
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ${MISSING_TOKEN}
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.GitHub.Token != "${MISSING_TOKEN}" {
		t.Errorf("GitHub.Token = %q, want ${MISSING_TOKEN} (unexpanded)", cfg.GitHub.Token)
	}
}

func TestLoad_EnvVarInOrg(t *testing.T) {
	t.Setenv("HIVE_ORG", "env-org")
	yaml := `
project:
  org: ${HIVE_ORG}
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Project.Org != "env-org" {
		t.Errorf("Project.Org = %q, want %q", cfg.Project.Org, "env-org")
	}
}

// ---------------------------------------------------------------------------
// config.env overrides
// ---------------------------------------------------------------------------

func writeConfigEnv(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing config.env: %v", err)
	}
	return path
}

func TestParseEnvFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")
	content := `# Comment line
PROJECT_ORG=override-org
PROJECT_REPOS=repo-x repo-y repo-z
DASHBOARD_PORT=4000
`
	os.WriteFile(path, []byte(content), 0o600)

	env, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile() error = %v", err)
	}
	if env["PROJECT_ORG"] != "override-org" {
		t.Errorf("PROJECT_ORG = %q, want %q", env["PROJECT_ORG"], "override-org")
	}
	if env["PROJECT_REPOS"] != "repo-x repo-y repo-z" {
		t.Errorf("PROJECT_REPOS = %q", env["PROJECT_REPOS"])
	}
	if env["DASHBOARD_PORT"] != "4000" {
		t.Errorf("DASHBOARD_PORT = %q, want %q", env["DASHBOARD_PORT"], "4000")
	}
}

func TestParseEnvFile_QuotedValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")
	content := `KEY1="double quoted"
KEY2='single quoted'
`
	os.WriteFile(path, []byte(content), 0o600)

	env, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile() error = %v", err)
	}
	if env["KEY1"] != "double quoted" {
		t.Errorf("KEY1 = %q, want %q", env["KEY1"], "double quoted")
	}
	if env["KEY2"] != "single quoted" {
		t.Errorf("KEY2 = %q, want %q", env["KEY2"], "single quoted")
	}
}

func TestParseEnvFile_MissingFile(t *testing.T) {
	_, err := ParseEnvFile("/nonexistent/config.env")
	if err == nil {
		t.Fatal("ParseEnvFile() expected error for missing file, got nil")
	}
}

func TestLoadWithOverrides_ConfigEnvOverridesOrg(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("yaml-org", "ghp_tok")), 0o600)

	envPath := filepath.Join(dir, "config.env")
	os.WriteFile(envPath, []byte("PROJECT_ORG=env-org\n"), 0o600)

	cfg, err := LoadWithOverrides(yamlPath, envPath)
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if cfg.Project.Org != "env-org" {
		t.Errorf("Project.Org = %q, want %q (config.env should override yaml)", cfg.Project.Org, "env-org")
	}
}

func TestLoadWithOverrides_ConfigEnvOverridesRepos(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("my-org", "ghp_tok")), 0o600)

	envPath := filepath.Join(dir, "config.env")
	os.WriteFile(envPath, []byte("PROJECT_REPOS=my-org/alpha my-org/beta\n"), 0o600)

	cfg, err := LoadWithOverrides(yamlPath, envPath)
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if len(cfg.Project.Repos) != 2 {
		t.Fatalf("len(Project.Repos) = %d, want 2", len(cfg.Project.Repos))
	}
	// PROJECT_REPOS still overrides the YAML; the org-qualified entries are
	// normalized to bare repo names on load, because repo targets are built as
	// org + "/" + repo and "my-org/alpha" would otherwise resolve to
	// "my-org/my-org/alpha".
	want := []string{"alpha", "beta"}
	for i := range want {
		if cfg.Project.Repos[i] != want[i] {
			t.Errorf("Project.Repos[%d] = %q, want %q", i, cfg.Project.Repos[i], want[i])
		}
	}
}

// Positive control for the normalization above: config.env values that are
// already bare must survive the override path untouched.
func TestLoadWithOverrides_ConfigEnvBareReposUnchanged(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("my-org", "ghp_tok")), 0o600)

	envPath := filepath.Join(dir, "config.env")
	os.WriteFile(envPath, []byte("PROJECT_REPOS=alpha beta\n"), 0o600)

	cfg, err := LoadWithOverrides(yamlPath, envPath)
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	want := []string{"alpha", "beta"}
	if len(cfg.Project.Repos) != len(want) {
		t.Fatalf("Project.Repos = %v, want %v", cfg.Project.Repos, want)
	}
	for i := range want {
		if cfg.Project.Repos[i] != want[i] {
			t.Errorf("Project.Repos[%d] = %q, want %q", i, cfg.Project.Repos[i], want[i])
		}
	}
}

func TestLoadWithOverrides_DashDisabled(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("my-org", "ghp_tok")), 0o600)

	cfg, err := LoadWithOverrides(yamlPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if cfg.Project.Org != "my-org" {
		t.Errorf("Project.Org = %q, want %q (dash should skip config.env)", cfg.Project.Org, "my-org")
	}
}

func TestLoadWithOverrides_AutoDetectConfigEnv(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("yaml-org", "ghp_tok")), 0o600)

	writeConfigEnv(t, dir, "PROJECT_ORG=auto-detected-org\n")

	cfg, err := LoadWithOverrides(yamlPath, "")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if cfg.Project.Org != "auto-detected-org" {
		t.Errorf("Project.Org = %q, want %q (auto-detected config.env)", cfg.Project.Org, "auto-detected-org")
	}
}

func TestLoadWithOverrides_DashboardPort(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "hive.yaml")
	os.WriteFile(yamlPath, []byte(minimalValidYAML("my-org", "ghp_tok")+"\ndashboard:\n  port: 3001\n"), 0o600)

	envPath := filepath.Join(dir, "config.env")
	os.WriteFile(envPath, []byte("DASHBOARD_PORT=9090\n"), 0o600)

	cfg, err := LoadWithOverrides(yamlPath, envPath)
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v", err)
	}
	if cfg.Dashboard.Port != 9090 {
		t.Errorf("Dashboard.Port = %d, want 9090 (config.env override)", cfg.Dashboard.Port)
	}
}

func TestAgentConfig_ClearOnKick(t *testing.T) {
	yaml := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
    clear_on_kick: true
  ci-maintainer:
    backend: claude
    clear_on_kick: false
`
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Agents["scanner"].ClearOnKick {
		t.Error("scanner.ClearOnKick should be true")
	}
	if cfg.Agents["ci-maintainer"].ClearOnKick {
		t.Error("ci-maintainer.ClearOnKick should be false")
	}
}

func TestModeConfig_UnmarshalYAML(t *testing.T) {
	yamlContent := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
governor:
  modes:
    idle:
      threshold: 0
      scanner: 15m
      supervisor: pause
    busy:
      threshold: 10
      scanner: 5m
`
	path := writeTempConfig(t, yamlContent)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Governor.Modes["idle"].Threshold != 0 {
		t.Errorf("idle threshold = %d", cfg.Governor.Modes["idle"].Threshold)
	}
	if cfg.Governor.Modes["idle"].Cadences["scanner"] != "15m" {
		t.Errorf("idle scanner cadence = %q", cfg.Governor.Modes["idle"].Cadences["scanner"])
	}
	if cfg.Governor.Modes["idle"].Cadences["supervisor"] != "pause" {
		t.Errorf("idle supervisor cadence = %q", cfg.Governor.Modes["idle"].Cadences["supervisor"])
	}
	if cfg.Governor.Modes["busy"].Threshold != 10 {
		t.Errorf("busy threshold = %d", cfg.Governor.Modes["busy"].Threshold)
	}
}

func TestModeConfig_UnmarshalYAML_InvalidThreshold(t *testing.T) {
	yamlContent := `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
governor:
  modes:
    idle:
      threshold: not-a-number
      scanner: 15m
`
	path := writeTempConfig(t, yamlContent)
	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid threshold")
	}
}

func TestApplyConfigEnv_AllFields(t *testing.T) {
	yamlContent := minimalValidYAML("default-org", "ghp_tok")
	path := writeTempConfig(t, yamlContent)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	envDir := t.TempDir()
	envPath := filepath.Join(envDir, "config.env")
	envContent := `PROJECT_ORG=overridden-org
PROJECT_REPOS=repo-x repo-y
PROJECT_AI_AUTHOR=custom-bot
PROJECT_PRIMARY_REPO=overridden-org/repo-x
DASHBOARD_PORT=9999
DASHBOARD_AUTH_TOKEN=secret123
AGENTS_ENABLED=worker
`
	os.WriteFile(envPath, []byte(envContent), 0o644)

	err = cfg.applyConfigEnv(envPath)
	if err != nil {
		t.Fatalf("applyConfigEnv() error = %v", err)
	}
	if cfg.Project.Org != "overridden-org" {
		t.Errorf("org = %q", cfg.Project.Org)
	}
	if len(cfg.Project.Repos) != 2 {
		t.Errorf("repos = %v", cfg.Project.Repos)
	}
	if cfg.Project.AIAuthor != "custom-bot" {
		t.Errorf("ai_author = %q", cfg.Project.AIAuthor)
	}
	if cfg.Project.PrimaryRepo != "overridden-org/repo-x" {
		t.Errorf("primary_repo = %q", cfg.Project.PrimaryRepo)
	}
	if cfg.Dashboard.Port != 9999 {
		t.Errorf("port = %d", cfg.Dashboard.Port)
	}
	if cfg.Dashboard.AuthToken != "secret123" {
		t.Errorf("auth_token = %q", cfg.Dashboard.AuthToken)
	}
}

func TestApplyDefaults_AllGovernorDefaults(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:  GitHubConfig{Token: "t"},
		Agents:  map[string]AgentConfig{"a": {Backend: "claude"}},
	}
	cfg.applyDefaults()

	if cfg.Governor.EvalIntervalS != defaultEvalIntervalS {
		t.Errorf("eval_interval = %d", cfg.Governor.EvalIntervalS)
	}
	if cfg.Policies.PollInterval != time.Duration(defaultPollIntervalMins)*time.Minute {
		t.Errorf("poll_interval = %v", cfg.Policies.PollInterval)
	}
	if cfg.Data.MetricsDir != "/data/metrics" {
		t.Errorf("metrics_dir = %q", cfg.Data.MetricsDir)
	}
	if cfg.Data.LogsDir != "/data/logs" {
		t.Errorf("logs_dir = %q", cfg.Data.LogsDir)
	}
	if cfg.Data.BobSessionsDir != "/data/home/.bob" {
		t.Errorf("bob_sessions_dir = %q", cfg.Data.BobSessionsDir)
	}
	if len(cfg.Governor.Labels.Exempt) == 0 {
		t.Error("expected default exempt labels")
	}
	if cfg.Governor.Labels.AutoMerge != DefaultAutoMergeLabel {
		t.Errorf("automerge label = %q, want %q", cfg.Governor.Labels.AutoMerge, DefaultAutoMergeLabel)
	}
	if cfg.Governor.Sensing.TTLSeconds != defaultSensingTTLSeconds {
		t.Errorf("sensing_ttl = %d", cfg.Governor.Sensing.TTLSeconds)
	}
	if cfg.Governor.Sensing.PullbackSeconds != defaultSensingPullbackSeconds {
		t.Errorf("sensing_pullback = %d", cfg.Governor.Sensing.PullbackSeconds)
	}
	if cfg.Governor.Health.HealthcheckInterval != defaultHealthcheckIntervalS {
		t.Errorf("healthcheck = %d", cfg.Governor.Health.HealthcheckInterval)
	}
	if cfg.Governor.Health.RestartCooldown != defaultRestartCooldownS {
		t.Errorf("restart_cooldown = %d", cfg.Governor.Health.RestartCooldown)
	}
	if cfg.Governor.Budget.PeriodDays != defaultBudgetPeriodDays {
		t.Errorf("budget_period = %d", cfg.Governor.Budget.PeriodDays)
	}
	if cfg.Governor.Budget.CriticalPct != defaultBudgetCriticalPct {
		t.Errorf("budget_critical = %d", cfg.Governor.Budget.CriticalPct)
	}
}

func TestApplyDefaults_KnowledgeDefaults(t *testing.T) {
	cfg := &Config{
		Project:   ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:    GitHubConfig{Token: "t"},
		Agents:    map[string]AgentConfig{"a": {Backend: "claude"}},
		Knowledge: KnowledgeConfig{Enabled: true},
	}
	cfg.applyDefaults()

	if cfg.Knowledge.Engine != defaultKnowledgeEngine {
		t.Errorf("engine = %q", cfg.Knowledge.Engine)
	}
	if cfg.Knowledge.Primer.MaxFacts != defaultKnowledgeMaxFacts {
		t.Errorf("max_facts = %d", cfg.Knowledge.Primer.MaxFacts)
	}
	if cfg.Knowledge.Primer.MergeStrategy != "precedence" {
		t.Errorf("merge_strategy = %q", cfg.Knowledge.Primer.MergeStrategy)
	}
	if len(cfg.Knowledge.Primer.Priority) == 0 {
		t.Error("expected default priority")
	}
	if cfg.Knowledge.Curator.AutoPromoteThreshold != defaultPromoteThreshold {
		t.Errorf("threshold = %f", cfg.Knowledge.Curator.AutoPromoteThreshold)
	}
}

func TestApplyDefaults_ExistingValuesNotOverridden(t *testing.T) {
	cfg := &Config{
		Project:   ProjectConfig{Org: "o", Repos: []string{"r"}},
		GitHub:    GitHubConfig{Token: "t"},
		Agents:    map[string]AgentConfig{"a": {Backend: "claude"}},
		Dashboard: DashboardConfig{Port: 8080},
		Governor: GovernorConfig{
			EvalIntervalS: 600,
			Labels:        LabelsConfig{Exempt: []string{"custom-label"}, AutoMerge: "ship-it"},
			Sensing:       SensingConfig{TTLSeconds: 1800, PullbackSeconds: 1800},
		},
	}
	cfg.applyDefaults()

	if cfg.Dashboard.Port != 8080 {
		t.Errorf("port = %d, want 8080", cfg.Dashboard.Port)
	}
	if cfg.Governor.EvalIntervalS != 600 {
		t.Errorf("eval = %d, want 600", cfg.Governor.EvalIntervalS)
	}
	if len(cfg.Governor.Labels.Exempt) != 1 || cfg.Governor.Labels.Exempt[0] != "custom-label" {
		t.Errorf("exempt = %v", cfg.Governor.Labels.Exempt)
	}
	if cfg.Governor.Labels.AutoMerge != "ship-it" {
		t.Errorf("automerge label = %q, want ship-it", cfg.Governor.Labels.AutoMerge)
	}
	if cfg.Governor.Sensing.TTLSeconds != 1800 {
		t.Errorf("ttl = %d", cfg.Governor.Sensing.TTLSeconds)
	}
}

func TestSaveRoundTrip(t *testing.T) {
	content := `
project:
  org: testorg
  name: testproject
  repos:
    - repo-a
    - repo-b
agents:
  scanner:
    enabled: true
    backend: claude
    model: claude-sonnet-4-6
governor:
  eval_interval_s: 300
  modes:
    surge:
      threshold: 130
      scanner: pause
    idle:
      threshold: 0
      scanner: 5m
  health:
    healthcheck_interval: 300
    restart_cooldown: 60
github:
  app_id: 123
`
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	os.WriteFile(path, []byte(content), 0o600)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Project.Repos = append(cfg.Project.Repos, "repo-c")
	cfg.Governor.Modes["surge"] = ModeConfig{
		Threshold: 200,
		Cadences:  map[string]Cadence{"scanner": "pause"},
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	cfg2, err := Load(path)
	if err != nil {
		t.Fatalf("reload after save: %v", err)
	}

	if len(cfg2.Project.Repos) != 3 || cfg2.Project.Repos[2] != "repo-c" {
		t.Errorf("repos = %v, want [repo-a repo-b repo-c]", cfg2.Project.Repos)
	}
	if cfg2.Governor.Modes["surge"].Threshold != 200 {
		t.Errorf("surge threshold = %d, want 200", cfg2.Governor.Modes["surge"].Threshold)
	}
	if cfg2.Governor.Modes["surge"].Cadences["scanner"] != "pause" {
		t.Errorf("surge scanner cadence = %q, want pause", cfg2.Governor.Modes["surge"].Cadences["scanner"])
	}
	if cfg2.Governor.Modes["idle"].Threshold != 0 {
		t.Errorf("idle threshold = %d, want 0", cfg2.Governor.Modes["idle"].Threshold)
	}
	if cfg2.SourcePath != path {
		t.Errorf("SourcePath = %q, want %q", cfg2.SourcePath, path)
	}
}

// ---------------------------------------------------------------------------
// Save — guard against empty/invalid config
// ---------------------------------------------------------------------------

func TestSave_BlocksEmptyOrg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	os.WriteFile(path, []byte("placeholder"), 0o600)

	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: ""}, // missing org
		Agents:     map[string]AgentConfig{"w": {Backend: "claude"}},
	}
	err := cfg.Save()
	if err == nil {
		t.Fatal("Save() should have returned an error for empty project.org")
	}

	// Verify the file was NOT overwritten.
	data, _ := os.ReadFile(path)
	if string(data) != "placeholder" {
		t.Errorf("Save() overwrote file despite empty org; got %q", string(data))
	}
}

func TestSave_BlocksNoAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	os.WriteFile(path, []byte("placeholder"), 0o600)

	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "my-org", Repos: []string{"r"}},
		Agents:     map[string]AgentConfig{}, // no agents
	}
	err := cfg.Save()
	if err == nil {
		t.Fatal("Save() should have returned an error for empty agents")
	}

	data, _ := os.ReadFile(path)
	if string(data) != "placeholder" {
		t.Errorf("Save() overwrote file despite no agents; got %q", string(data))
	}
}

func TestSave_AllowsValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	os.WriteFile(path, []byte("old"), 0o600)

	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "my-org", Repos: []string{"r"}},
		Agents:     map[string]AgentConfig{"w": {Backend: "claude"}},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() should succeed for valid config: %v", err)
	}

	data, _ := os.ReadFile(path)
	if string(data) == "old" {
		t.Error("Save() did not write the new config")
	}
}

func TestLoad_OTelConfig(t *testing.T) {
	yaml := minimalValidYAML("my-org", "ghp_tok") + `
otel:
  enabled: true
  endpoint: https://otel.example.com/v1/traces
  service_name: hive-dev
  insecure: true
  headers:
    authorization: Bearer ${OTEL_TOKEN}
`
	t.Setenv("OTEL_TOKEN", "test-token")
	path := writeTempConfig(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	got := cfg.EffectiveOTel()
	if !got.Enabled {
		t.Fatal("OTel.Enabled = false, want true")
	}
	if got.Endpoint != "https://otel.example.com/v1/traces" {
		t.Errorf("OTel.Endpoint = %q", got.Endpoint)
	}
	if got.ServiceNameOrDefault() != "hive-dev" {
		t.Errorf("service_name = %q", got.ServiceNameOrDefault())
	}
	if !got.Insecure {
		t.Error("OTel.Insecure = false, want true")
	}
	if got.Headers["authorization"] != "Bearer test-token" {
		t.Errorf("authorization header = %q", got.Headers["authorization"])
	}
}

func TestEffectiveOTel_LegacyTracingFallback(t *testing.T) {
	cfg := &Config{Tracing: OTelConfig{Enabled: true, Endpoint: "https://legacy.example.com"}}
	got := cfg.EffectiveOTel()
	if !got.Enabled || got.Endpoint != "https://legacy.example.com" {
		t.Fatalf("legacy tracing fallback = %+v", got)
	}
	if got.ServiceNameOrDefault() != DefaultOTelServiceName {
		t.Errorf("default service name = %q", got.ServiceNameOrDefault())
	}
}

// TestSelfAuthoredAutoMergeAllowed guards the bug fix: the self-authored
// auto-merge sweep must be OFF at ACMM L4 (examples/acmm/l4.md: "DO NOT merge
// pull requests — L4 is not an auto-merge level") and at L5 (l5.md: "Merge
// pull requests — no; L5 PRs remain hold-gated for human review"), and ON
// only at SelfMergeMinACMMLevel (L6, l6.md: "Merge PRs when CI passes") and
// above — matching console, which runs at L6 and must be unaffected. Both the
// auto_merge.self_authored flag AND the ACMM level must independently allow
// it.
func TestSelfAuthoredAutoMergeAllowed(t *testing.T) {
	l4, l5, l6, l7 := 4, 5, 6, 7
	cases := []struct {
		name  string
		cfg   AutoMergeConfig
		level *int
		want  bool
	}{
		{"nil acmm level fails closed even with flag on", AutoMergeConfig{SelfAuthored: boolPtr(true)}, nil, false},
		{"nil acmm level fails closed with default flag", AutoMergeConfig{}, nil, false},
		{"L4 hive (ks/hive bug repro) stays disabled despite default-on flag", AutoMergeConfig{}, &l4, false},
		{"L4 hive stays disabled with explicit flag on", AutoMergeConfig{SelfAuthored: boolPtr(true)}, &l4, false},
		{"L5 hive stays disabled: L5 is hold-gated, not auto-merge", AutoMergeConfig{}, &l5, false},
		{"L6 hive (console) is enabled by default", AutoMergeConfig{}, &l6, true},
		{"L6 hive with flag explicitly off stays disabled", AutoMergeConfig{SelfAuthored: boolPtr(false)}, &l6, false},
		{"L7 (above minimum) stays enabled", AutoMergeConfig{}, &l7, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.SelfAuthoredAutoMergeAllowed(tc.level); got != tc.want {
				t.Errorf("SelfAuthoredAutoMergeAllowed(%v) = %v, want %v", tc.level, got, tc.want)
			}
		})
	}
}

func TestAutoMergeConfigRequiredCheckSet(t *testing.T) {
	cases := []struct {
		name     string
		checks   []string
		wantSet  map[string]bool
		wantKnow bool
	}{
		{"unset returns not-declared", nil, nil, false},
		{"empty slice returns not-declared", []string{}, nil, false},
		{"all-blank entries return not-declared", []string{"  ", ""}, nil, false},
		{"single entry", []string{"build-gate"}, map[string]bool{"build-gate": true}, true},
		{"multiple entries, whitespace trimmed", []string{" build-gate ", "lint"}, map[string]bool{"build-gate": true, "lint": true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AutoMergeConfig{RequiredChecks: tc.checks}
			got, know := cfg.RequiredCheckSet()
			if know != tc.wantKnow {
				t.Fatalf("RequiredCheckSet() requiredKnown = %v, want %v", know, tc.wantKnow)
			}
			if !know {
				if got != nil {
					t.Fatalf("RequiredCheckSet() set = %v, want nil when not declared", got)
				}
				return
			}
			if len(got) != len(tc.wantSet) {
				t.Fatalf("RequiredCheckSet() set = %v, want %v", got, tc.wantSet)
			}
			for k, v := range tc.wantSet {
				if got[k] != v {
					t.Errorf("RequiredCheckSet()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

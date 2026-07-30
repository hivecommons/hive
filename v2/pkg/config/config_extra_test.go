package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPRsAllowed(t *testing.T) {
	p := &ProjectConfig{}
	if !p.PRsAllowed() {
		t.Error("default should be true")
	}

	f := false
	p.OpenPRs = &f
	if p.PRsAllowed() {
		t.Error("should be false when set to false")
	}

	tr := true
	p.OpenPRs = &tr
	if !p.PRsAllowed() {
		t.Error("should be true when set to true")
	}
}

func TestShouldIncludeRepos(t *testing.T) {
	a := &AgentConfig{}
	if !a.ShouldIncludeRepos() {
		t.Error("default should be true")
	}

	f := false
	a.IncludeRepos = &f
	if a.ShouldIncludeRepos() {
		t.Error("should be false when set")
	}
}

func TestGetBeadRole(t *testing.T) {
	a := &AgentConfig{}
	if got := a.GetBeadRole(); got != "worker" {
		t.Errorf("default = %q, want worker", got)
	}

	a.BeadRole = "supervisor"
	if got := a.GetBeadRole(); got != "supervisor" {
		t.Errorf("got %q, want supervisor", got)
	}
}

func TestGetSortOrder(t *testing.T) {
	a := &AgentConfig{}
	if got := a.GetSortOrder(); got != 100 {
		t.Errorf("default worker = %d, want 100", got)
	}

	a.BeadRole = "supervisor"
	if got := a.GetSortOrder(); got != 0 {
		t.Errorf("supervisor default = %d, want 0", got)
	}

	a.SortOrder = 50
	if got := a.GetSortOrder(); got != 50 {
		t.Errorf("explicit = %d, want 50", got)
	}
}

func TestOnDemandAgentsFromPacks(t *testing.T) {
	result := OnDemandAgentsFromPacks()
	if result == nil {
		t.Fatal("expected non-nil map")
	}
}

func TestSaveAndRemoveAgentFile(t *testing.T) {
	dir := t.TempDir()
	agent := AgentConfig{
		Backend: "claude",
		Model:   "claude-sonnet-4-6",
		Enabled: true,
	}

	if err := SaveAgentFile(dir, "test-agent", agent); err != nil {
		t.Fatal(err)
	}

	overrides, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := overrides["test-agent"]; !ok {
		t.Error("test-agent not found in overrides")
	}

	if err := RemoveAgentFile(dir, "test-agent"); err != nil {
		t.Fatal(err)
	}

	overrides2, _ := LoadAgentOverrides(dir)
	if _, ok := overrides2["test-agent"]; ok {
		t.Error("test-agent should be removed")
	}
}

func TestRemoveAgentFileNotExists(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveAgentFile(dir, "nonexistent"); err != nil {
		t.Errorf("removing non-existent file should not error: %v", err)
	}
}

func TestLoadAgentOverridesEmptyDir(t *testing.T) {
	overrides, err := LoadAgentOverrides("")
	if err != nil {
		t.Fatal(err)
	}
	if overrides != nil {
		t.Error("empty dir should return nil")
	}
}

func TestLoadAgentOverridesNonExistent(t *testing.T) {
	overrides, err := LoadAgentOverrides("/nonexistent/dir")
	if err != nil {
		t.Fatal(err)
	}
	if overrides != nil {
		t.Error("non-existent dir should return nil")
	}
}

func TestApplyAgentDefaultsExtended(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude"},
		},
	}
	cfg.ApplyAgentDefaults("scanner")
	a := cfg.Agents["scanner"]
	if !a.Enabled {
		t.Error("should be enabled by default")
	}
	if !a.ClearOnKick {
		t.Error("ClearOnKick should default to true")
	}
	if a.Role != "scanner" {
		t.Errorf("Role = %q, want scanner", a.Role)
	}
}

func TestApplyAgentDefaultsMissing(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{},
	}
	cfg.ApplyAgentDefaults("nonexistent")
}

func TestMergeAgentOverridesExtended(t *testing.T) {
	cfg := &Config{}
	overlays := map[string]AgentConfig{
		"new-agent": {Backend: "copilot", Model: "gpt-4"},
	}
	cfg.MergeAgentOverrides(overlays)
	if _, ok := cfg.Agents["new-agent"]; !ok {
		t.Error("overlay agent not merged")
	}
	if !cfg.Agents["new-agent"].Managed {
		t.Error("merged agent should be Managed")
	}
}

func TestMergeAgentOverridesNilMap(t *testing.T) {
	cfg := &Config{Agents: nil}
	cfg.MergeAgentOverrides(map[string]AgentConfig{
		"test": {Backend: "claude"},
	})
	if cfg.Agents == nil {
		t.Error("Agents map should be initialized")
	}
}

func TestWildcardMatch_Regex(t *testing.T) {
	if !WildcardMatch("bug: fix issue #123", "/bug.*#\\d+/") {
		t.Error("regex pattern should match")
	}
	if WildcardMatch("feature: add X", "/bug.*#\\d+/") {
		t.Error("regex pattern should not match non-bug")
	}
}

func TestWildcardMatch_InvalidRegex(t *testing.T) {
	if WildcardMatch("test", "/[invalid/") {
		t.Error("invalid regex should not match")
	}
}

func TestWildcardMatch_WildcardPrefix(t *testing.T) {
	if !WildcardMatch("hello-world", "*world") {
		t.Error("*world should match hello-world")
	}
	// plain pattern is a substring match
	if !WildcardMatch("hello-world", "hello") {
		t.Error("plain 'hello' should substring-match hello-world")
	}
	if WildcardMatch("hello-world", "goodbye") {
		t.Error("plain 'goodbye' should not match hello-world")
	}
}

func TestWildcardMatch_WildcardNoPrefix(t *testing.T) {
	// Pattern "fix*issue" — text must start with "fix"
	if !WildcardMatch("fix-the-issue", "fix*issue") {
		t.Error("fix*issue should match fix-the-issue")
	}
	if WildcardMatch("my-fix-the-issue", "fix*issue") {
		t.Error("fix*issue should not match text not starting with fix")
	}
}

func TestWildcardMatch_WildcardNoSuffix(t *testing.T) {
	// Pattern "hello*world" — text must end with "world"
	if !WildcardMatch("hello-cruel-world", "hello*world") {
		t.Error("hello*world should match hello-cruel-world")
	}
	if WildcardMatch("hello-cruel-world-now", "hello*world") {
		t.Error("hello*world should not match text not ending with world")
	}
}

func TestWildcardMatch_SubstringMatch(t *testing.T) {
	if !WildcardMatch("this is a test string", "test") {
		t.Error("plain text should substring match")
	}
	if WildcardMatch("hello world", "goodbye") {
		t.Error("should not match absent substring")
	}
}

func TestWildcardMatch_WildcardNotFound(t *testing.T) {
	if WildcardMatch("abc", "x*y") {
		t.Error("x*y should not match abc")
	}
}

func TestSave_SuccessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")

	cfg := &Config{
		SourcePath: path,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude"},
		},
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	if len(data) == 0 {
		t.Error("saved config is empty")
	}
}

func TestSave_NoSourcePath(t *testing.T) {
	cfg := &Config{
		SourcePath: "",
		Project:    ProjectConfig{Org: "org"},
		Agents:     map[string]AgentConfig{"x": {}},
	}
	if err := cfg.Save(); err == nil {
		t.Error("expected error with no source path")
	}
}

func TestSave_EmptyOrg(t *testing.T) {
	cfg := &Config{
		SourcePath: "/tmp/test.yaml",
		Project:    ProjectConfig{Org: ""},
		Agents:     map[string]AgentConfig{"x": {}},
	}
	if err := cfg.Save(); err == nil {
		t.Error("expected error with empty org (save guard)")
	}
}

func TestSave_NoAgents(t *testing.T) {
	cfg := &Config{
		SourcePath: "/tmp/test.yaml",
		Project:    ProjectConfig{Org: "org"},
		Agents:     map[string]AgentConfig{},
	}
	if err := cfg.Save(); err == nil {
		t.Error("expected error with no agents (save guard)")
	}
}

func TestMatchesAny(t *testing.T) {
	if !MatchesAny("hello world", []string{"hello*"}) {
		t.Error("should match wildcard")
	}
	if MatchesAny("hello world", []string{"goodbye*"}) {
		t.Error("should not match")
	}
	if MatchesAny("test", nil) {
		t.Error("nil patterns should not match")
	}
	if !MatchesAny("test", []string{"*"}) {
		t.Error("star should match everything")
	}
}

func TestSaveAgentFileErrorPath(t *testing.T) {
	err := SaveAgentFile("/nonexistent/dir/agents", "test", AgentConfig{})
	if err == nil {
		t.Error("expected error for bad dir")
	}
}

func TestACMMPackByLevelAllLevels(t *testing.T) {
	for level := 1; level <= 6; level++ {
		pack, err := ACMMPackByLevel(level)
		if err != nil {
			t.Errorf("ACMMPackByLevel(%d) error: %v", level, err)
		}
		if len(pack.Agents) == 0 {
			t.Errorf("ACMMPackByLevel(%d) returned empty agents", level)
		}
	}
	_, err := ACMMPackByLevel(99)
	if err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestApplyBootstrapEnv(t *testing.T) {
	t.Setenv("HIVE_REPO", "testorg/testrepo")
	cfg := &Config{}
	cfg.applyBootstrapEnv()
	if cfg.Project.Org != "testorg" {
		t.Errorf("Org = %q, want testorg", cfg.Project.Org)
	}
	if len(cfg.Project.Repos) != 1 || cfg.Project.Repos[0] != "testrepo" {
		t.Errorf("Repos = %v", cfg.Project.Repos)
	}
	if cfg.Project.PrimaryRepo != "testrepo" {
		t.Errorf("PrimaryRepo = %q", cfg.Project.PrimaryRepo)
	}
}

func TestApplyBootstrapEnvNoOverwrite(t *testing.T) {
	t.Setenv("HIVE_REPO", "neworg/newrepo")
	cfg := &Config{Project: ProjectConfig{Org: "existing", Repos: []string{"existing"}, PrimaryRepo: "existing"}}
	cfg.applyBootstrapEnv()
	if cfg.Project.Org != "existing" {
		t.Errorf("should not overwrite existing Org")
	}
}

func TestApplyBootstrapEnvEmpty(t *testing.T) {
	t.Setenv("HIVE_REPO", "")
	cfg := &Config{}
	cfg.applyBootstrapEnv()
	if cfg.Project.Org != "" {
		t.Error("empty env should not set Org")
	}
}

func TestApplyBootstrapEnvInvalid(t *testing.T) {
	t.Setenv("HIVE_REPO", "noslash")
	cfg := &Config{}
	cfg.applyBootstrapEnv()
	if cfg.Project.Org != "" {
		t.Error("invalid format should not set Org")
	}
}

func TestExpandEnvVars(t *testing.T) {
	t.Setenv("TEST_VAR", "hello")
	got := expandEnvVars("${TEST_VAR} world")
	if got != "hello world" {
		t.Errorf("expandEnvVars = %q", got)
	}

	got2 := expandEnvVars("${NONEXISTENT_VAR}")
	if got2 != "${NONEXISTENT_VAR}" {
		t.Errorf("missing var should stay: %q", got2)
	}
}

func TestApplyConfigEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte(`PROJECT_ORG=myorg
PROJECT_REPOS=repo1 repo2
PROJECT_AI_AUTHOR=bot
PROJECT_PRIMARY_REPO=repo1
PROJECT_OPEN_PRS=true
DASHBOARD_PORT=9999
DASHBOARD_AUTH_TOKEN=secret123
`), 0o644)

	cfg := &Config{Agents: map[string]AgentConfig{}}
	if err := cfg.applyConfigEnv(envFile); err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Org != "myorg" {
		t.Errorf("Org = %q", cfg.Project.Org)
	}
	if len(cfg.Project.Repos) != 2 {
		t.Errorf("Repos = %v", cfg.Project.Repos)
	}
	if cfg.Project.AIAuthor != "bot" {
		t.Errorf("AIAuthor = %q", cfg.Project.AIAuthor)
	}
	if cfg.Dashboard.Port != 9999 {
		t.Errorf("Port = %d", cfg.Dashboard.Port)
	}
	if cfg.Dashboard.AuthToken != "secret123" {
		t.Errorf("AuthToken = %q", cfg.Dashboard.AuthToken)
	}
	if cfg.Project.OpenPRs == nil || !*cfg.Project.OpenPRs {
		t.Error("OpenPRs should be true")
	}
}

func TestApplyConfigEnvBadFile(t *testing.T) {
	cfg := &Config{}
	err := cfg.applyConfigEnv("/nonexistent/env/file")
	if err == nil {
		t.Error("expected error for bad file")
	}
}

func TestApplyConfigEnvAgentsEnabled(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte("AGENTS_ENABLED=scanner quality\n"), 0o644)

	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Enabled: false},
			"quality": {Enabled: false},
		},
	}
	if err := cfg.applyConfigEnv(envFile); err != nil {
		t.Fatal(err)
	}
	if !cfg.Agents["scanner"].Enabled {
		t.Error("scanner should be enabled")
	}
	if !cfg.Agents["quality"].Enabled {
		t.Error("quality should be enabled")
	}
}

func TestApplyConfigEnvDashboardTokenFallback(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "test.env")
	os.WriteFile(envFile, []byte("HIVE_DASHBOARD_TOKEN=fallback-token\n"), 0o644)

	cfg := &Config{}
	if err := cfg.applyConfigEnv(envFile); err != nil {
		t.Fatal(err)
	}
	if cfg.Dashboard.AuthToken != "fallback-token" {
		t.Errorf("AuthToken = %q, want fallback-token", cfg.Dashboard.AuthToken)
	}
}

func TestACMMPacks(t *testing.T) {
	packs := ACMMPacks()
	if len(packs) == 0 {
		t.Fatal("ACMMPacks returned empty")
	}
	if len(packs) < 6 {
		t.Errorf("expected at least 6 packs, got %d", len(packs))
	}
	for i := 1; i < len(packs); i++ {
		if packs[i].Level < packs[i-1].Level {
			t.Errorf("packs not sorted: level %d before %d", packs[i-1].Level, packs[i].Level)
		}
	}
	for _, p := range packs {
		if len(p.Agents) == 0 {
			t.Errorf("pack level %d has no agents", p.Level)
		}
	}
}

func TestSaveAgentFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	agent := AgentConfig{
		Backend:     "claude",
		Model:       "claude-sonnet-4-6",
		Enabled:     true,
		DisplayName: "Test Agent",
		Role:        "worker",
	}
	if err := SaveAgentFile(dir, "myagent", agent); err != nil {
		t.Fatal(err)
	}

	overrides, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := overrides["myagent"]
	if !ok {
		t.Fatal("myagent not found")
	}
	if loaded.Backend != "claude" {
		t.Errorf("Backend = %q", loaded.Backend)
	}
	if loaded.DisplayName != "Test Agent" {
		t.Errorf("DisplayName = %q", loaded.DisplayName)
	}
	if !loaded.Managed {
		t.Error("loaded agent should be Managed")
	}
}

func TestResolveAgent_ByName(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ID: "scan-001"},
			"fixer":   {Backend: "copilot", ID: "fix-001"},
		},
	}

	name, ok := cfg.ResolveAgent("scanner")
	if !ok || name != "scanner" {
		t.Errorf("ResolveAgent(scanner) = (%q, %v), want (scanner, true)", name, ok)
	}
}

func TestResolveAgent_ByID(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ID: "scan-001"},
		},
	}

	name, ok := cfg.ResolveAgent("scan-001")
	if !ok || name != "scanner" {
		t.Errorf("ResolveAgent(scan-001) = (%q, %v), want (scanner, true)", name, ok)
	}
}

func TestResolveAgent_NotFound(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ID: "scan-001"},
		},
	}

	name, ok := cfg.ResolveAgent("nonexistent")
	if ok {
		t.Errorf("ResolveAgent(nonexistent) = (%q, %v), expected not found", name, ok)
	}
}

func TestAgentByID_Found(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ID: "scan-001", Model: "sonnet"},
		},
	}

	agent, ok := cfg.AgentByID("scan-001")
	if !ok {
		t.Fatal("expected to find agent by ID")
	}
	if agent.Model != "sonnet" {
		t.Errorf("agent.Model = %q, want sonnet", agent.Model)
	}
}

func TestAgentByID_NotFound(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", ID: "scan-001"},
		},
	}

	_, ok := cfg.AgentByID("nonexistent")
	if ok {
		t.Error("expected not to find agent with nonexistent ID")
	}
}

func TestWatcherSkipNext(t *testing.T) {
	w := NewWatcher("/tmp/test.yaml", func(c *Config) {}, nil)
	w.SkipNext()
	w.mu.Lock()
	if !w.skipNext {
		t.Error("skipNext should be true after SkipNext()")
	}
	w.mu.Unlock()
}

func TestEnabledExplicitlySet_Default(t *testing.T) {
	a := &AgentConfig{}
	if a.EnabledExplicitlySet() {
		t.Error("default AgentConfig should have enabledSet=false")
	}
}

func TestEnabledExplicitlySet_ViaYAML(t *testing.T) {
	yamlData := []byte("enabled: false\nbackend: claude\n")
	var a AgentConfig
	if err := yaml.Unmarshal(yamlData, &a); err != nil {
		t.Fatal(err)
	}
	if !a.EnabledExplicitlySet() {
		t.Error("after YAML with 'enabled' key, EnabledExplicitlySet should be true")
	}
}

func TestResolvedAPIURL_Default(t *testing.T) {
	g := GitHubConfig{}
	if got := g.ResolvedAPIURL(); got != DefaultGitHubAPIURL {
		t.Errorf("ResolvedAPIURL() = %q, want %q", got, DefaultGitHubAPIURL)
	}
}

func TestResolvedAPIURL_Custom(t *testing.T) {
	g := GitHubConfig{APIURL: "https://github.ibm.com/api/v3"}
	if got := g.ResolvedAPIURL(); got != "https://github.ibm.com/api/v3" {
		t.Errorf("ResolvedAPIURL() = %q", got)
	}
}

func TestResolvedBaseURL_Default(t *testing.T) {
	g := GitHubConfig{}
	if got := g.ResolvedBaseURL(); got != DefaultGitHubBaseURL {
		t.Errorf("ResolvedBaseURL() = %q, want %q", got, DefaultGitHubBaseURL)
	}
}

func TestResolvedBaseURL_Custom(t *testing.T) {
	g := GitHubConfig{BaseURL: "https://github.ibm.com"}
	if got := g.ResolvedBaseURL(); got != "https://github.ibm.com" {
		t.Errorf("ResolvedBaseURL() = %q", got)
	}
}

// OAuth login hosts are ALWAYS public github.com, even when the App/repo host is
// GHE — this is the split that keeps device-flow login working on GHE hives.
func TestOAuthURLs_AlwaysPublic_EvenOnGHE(t *testing.T) {
	ghe := GitHubConfig{
		APIURL:        "https://github.ibm.com/api/v3",
		BaseURL:       "https://github.ibm.com",
		OAuthClientID: "", // blank must fall back to the public github.com client
	}
	if got := ghe.OAuthAPIURL(); got != DefaultGitHubAPIURL {
		t.Errorf("OAuthAPIURL() on GHE hive = %q, want github.com %q", got, DefaultGitHubAPIURL)
	}
	if got := ghe.OAuthBaseURL(); got != DefaultGitHubBaseURL {
		t.Errorf("OAuthBaseURL() on GHE hive = %q, want github.com %q", got, DefaultGitHubBaseURL)
	}
	if got := ghe.OAuthClientIDResolved(); got != DefaultOAuthClientID {
		t.Errorf("OAuthClientIDResolved() blank on GHE = %q, want public %q", got, DefaultOAuthClientID)
	}
	// Meanwhile the App/repo host must STILL be GHE — the split does not touch it.
	if got := ghe.ResolvedAPIURL(); got != "https://github.ibm.com/api/v3" {
		t.Errorf("ResolvedAPIURL() (App host) = %q, want GHE preserved", got)
	}
}

func TestOAuthClientIDResolved_ConfiguredWins(t *testing.T) {
	g := GitHubConfig{OAuthClientID: "Ov23liCUSTOM"}
	if got := g.OAuthClientIDResolved(); got != "Ov23liCUSTOM" {
		t.Errorf("OAuthClientIDResolved() = %q, want the configured value", got)
	}
}

// App-bot authorship defaults ON (nil == true, the fleet norm); only an explicit
// false opts out.
func TestAppAuthoredPRsEnabled_DefaultsOn(t *testing.T) {
	if !(GitHubConfig{}).AppAuthoredPRsEnabled() {
		t.Error("AppAuthoredPRsEnabled() with unset flag = false, want true (default on)")
	}
	f := false
	if (GitHubConfig{AppAuthoredPRs: &f}).AppAuthoredPRsEnabled() {
		t.Error("AppAuthoredPRsEnabled() with explicit false = true, want false")
	}
	tr := true
	if !(GitHubConfig{AppAuthoredPRs: &tr}).AppAuthoredPRsEnabled() {
		t.Error("AppAuthoredPRsEnabled() with explicit true = false, want true")
	}
}

// With App-bot mode on (the default) and ai_author empty, the effective author
// is the App bot login; an explicit ai_author still wins; an explicit opt-out
// yields no author.
func TestEffectiveAIAuthor_AppBotDefault(t *testing.T) {
	botHive := &Config{GitHub: GitHubConfig{AppID: 42, AppSlug: "acme-hive"}}
	if got := botHive.EffectiveAIAuthor(); got != "acme-hive[bot]" {
		t.Errorf("EffectiveAIAuthor() default = %q, want acme-hive[bot]", got)
	}
	withAuthor := &Config{GitHub: GitHubConfig{AppID: 42, AppSlug: "acme-hive"}}
	withAuthor.Project.AIAuthor = "alice"
	if got := withAuthor.EffectiveAIAuthor(); got != "alice" {
		t.Errorf("EffectiveAIAuthor() with ai_author = %q, want alice", got)
	}
	f := false
	optOut := &Config{GitHub: GitHubConfig{AppID: 42, AppSlug: "acme-hive", AppAuthoredPRs: &f}}
	if got := optOut.EffectiveAIAuthor(); got != "" {
		t.Errorf("EffectiveAIAuthor() opted out = %q, want empty", got)
	}
}

func TestRemoveAgentFile_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../etc/passwd", "sub/agent", "back\\slash"} {
		err := RemoveAgentFile(dir, bad)
		if err == nil {
			t.Errorf("RemoveAgentFile(%q) should reject path traversal", bad)
		}
	}
}

func TestSaveAgentFile_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape", "sub/dir", "back\\slash"} {
		err := SaveAgentFile(dir, bad, AgentConfig{})
		if err == nil {
			t.Errorf("SaveAgentFile(%q) should reject path traversal", bad)
		}
	}
}

func TestSaveAgentFile_WriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	// Make dir read-only after creation
	os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)

	err := SaveAgentFile(dir, "test", AgentConfig{Backend: "claude"})
	if err == nil {
		t.Error("expected error writing to read-only dir")
	}
}

func TestLoadWithOverridesFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hive.yaml")
	os.WriteFile(cfgFile, []byte(`project:
  org: testorg
  repos: [testrepo]
  primary_repo: testrepo
github:
  token: ghp_testtoken123
agents:
  scanner:
    backend: claude
`), 0o644)

	cfg, err := LoadWithOverrides(cfgFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Org != "testorg" {
		t.Errorf("Org = %q", cfg.Project.Org)
	}
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Error("scanner agent missing")
	}
}

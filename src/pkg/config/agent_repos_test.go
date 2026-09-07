package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newScopeTestConfig(t *testing.T, agents map[string]AgentConfig) *Config {
	t.Helper()
	dir := t.TempDir()
	hermeticPersistPaths(t, dir)
	if agents == nil {
		agents = map[string]AgentConfig{"scanner": {Backend: "claude"}}
	}
	return &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project: ProjectConfig{
			Org:         "acme",
			Repos:       []string{"console", "dashboard", "laredo/cuga-agent"},
			PrimaryRepo: "console",
		},
		GitHub: GitHubConfig{AppID: 3568013},
		Agents: agents,
		Data:   DataConfig{AgentsDir: t.TempDir()},
	}
}

// The scope can only ever NARROW. Everything that is not explicitly scoped —
// which is every agent on every hive that does not use the feature — serves the
// whole hive, and so does an agent nobody has heard of.
func TestAgentServesRepo_UnscopedAndUnknownServeEverything(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"scanner": {Backend: "claude"},
	})
	for _, repo := range []string{"console", "dashboard", "laredo/cuga-agent", "never-heard-of-it"} {
		if !cfg.AgentServesRepo("scanner", repo) {
			t.Errorf("unscoped agent refused %q", repo)
		}
		if !cfg.AgentServesRepo("some-other-agent", repo) {
			t.Errorf("unknown agent refused %q; an unknown scope must fail open", repo)
		}
	}
	if got := cfg.AgentRepoScope("scanner"); got != nil {
		t.Errorf("AgentRepoScope(unscoped) = %v, want nil", got)
	}
	if got := cfg.ReposForAgent("scanner"); len(got) != 3 {
		t.Errorf("ReposForAgent(unscoped) = %v, want all three", got)
	}
	if got := cfg.RepoScopedAgents(); got != nil {
		t.Errorf("RepoScopedAgents = %v, want nil when nothing is scoped", got)
	}
}

func TestAgentServesRepo_MatchesEverySpelling(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"Console", "laredo/cuga-agent"}},
	})
	for _, repo := range []string{"console", "Console", "acme/console", "ACME/Console", "laredo/cuga-agent"} {
		if !cfg.AgentServesRepo("schema", repo) {
			t.Errorf("scoped agent refused %q, which is in its scope", repo)
		}
	}
	if cfg.AgentServesRepo("schema", "dashboard") {
		t.Error("scoped agent served a repo outside its scope")
	}
	// A same-named repo in a different org is a different repository.
	if cfg.AgentServesRepo("schema", "other/console") {
		t.Error("scoped agent served a same-named repo in another org")
	}
	// A caller that cannot name a repo must not be refused on a scope it could
	// not have violated.
	if !cfg.AgentServesRepo("schema", "") {
		t.Error("scoped agent refused an unnamed repo; that must fail open")
	}
}

// A replica is the same agent run more than once. A replica that ignored the
// scope would be a hole straight through it.
func TestAgentServesRepo_ReplicaInheritsScope(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"schema":   {Backend: "claude", Repos: []string{"console"}},
		"schema-2": {Backend: "claude", ReplicaOf: "schema"},
	})
	if !cfg.AgentServesRepo("schema-2", "console") {
		t.Error("replica refused its base agent's repo")
	}
	if cfg.AgentServesRepo("schema-2", "dashboard") {
		t.Error("replica escaped its base agent's scope")
	}
	if got := cfg.ReposForAgent("schema-2"); len(got) != 1 || got[0] != "console" {
		t.Errorf("ReposForAgent(replica) = %v, want [console]", got)
	}
}

// ReposForAgent intersects with project.repos: a scope naming a repo the hive
// does not watch must not conjure it into a kick.
func TestReposForAgent_IntersectsWithProjectRepos(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"dashboard", "not-watched"}},
	})
	got := cfg.ReposForAgent("schema")
	if len(got) != 1 || got[0] != "dashboard" {
		t.Fatalf("ReposForAgent = %v, want [dashboard]", got)
	}
	// The DECLARED scope keeps the unwatched entry — that is what makes the
	// misconfiguration visible instead of silently disappearing.
	scope := cfg.AgentRepoScope("schema")
	if len(scope) != 2 {
		t.Errorf("AgentRepoScope = %v, want the declared list including the unwatched entry", scope)
	}
}

// $HIVE_REPO must never point a specialist at a repo it may not write to: every
// shipped template passes it straight to `gh ... --repo "$HIVE_REPO"`.
func TestPrimaryRepoForAgent(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"scanner": {Backend: "claude"},
		"schema":  {Backend: "claude", Repos: []string{"dashboard"}},
		"onprim":  {Backend: "claude", Repos: []string{"console", "dashboard"}},
		"lost":    {Backend: "claude", Repos: []string{"not-watched"}},
	})
	if got := cfg.PrimaryRepoForAgent("scanner"); got != "console" {
		t.Errorf("unscoped agent primary = %q, want console (the hive primary)", got)
	}
	if got := cfg.PrimaryRepoForAgent("schema"); got != "dashboard" {
		t.Errorf("scoped-away-from-primary agent primary = %q, want dashboard", got)
	}
	if got := cfg.PrimaryRepoForAgent("onprim"); got != "console" {
		t.Errorf("agent whose scope INCLUDES the hive primary should keep it, got %q", got)
	}
	// A scope that matches nothing falls back to the hive primary rather than
	// producing an empty $HIVE_REPO; the warning below is how that is surfaced.
	if got := cfg.PrimaryRepoForAgent("lost"); got != "console" {
		t.Errorf("agent with an unmatchable scope primary = %q, want the hive primary", got)
	}
}

func TestAgentsForRepo(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"scanner": {Backend: "claude"},
		"schema":  {Backend: "claude", Repos: []string{"console"}},
		"rustdoc": {Backend: "claude", Repos: []string{"dashboard"}},
	})
	got := cfg.AgentsForRepo("console")
	if len(got) != 2 || got[0] != "scanner" || got[1] != "schema" {
		t.Errorf("AgentsForRepo(console) = %v, want [scanner schema] — unscoped agents serve every repo", got)
	}
	if got := cfg.AgentsForRepo("dashboard"); len(got) != 2 || got[1] != "scanner" {
		t.Errorf("AgentsForRepo(dashboard) = %v, want [rustdoc scanner]", got)
	}
}

func TestSetAgentReposAndSave_PersistsAndClaimsOwnership(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{"schema": {Backend: "claude"}})

	changed, err := cfg.SetAgentReposAndSave("schema", []string{" console ", "", "dashboard"})
	if err != nil {
		t.Fatalf("SetAgentReposAndSave: %v", err)
	}
	if !changed {
		t.Fatal("changed = false for a real scope change")
	}
	if got := cfg.Agents["schema"].Repos; len(got) != 2 || got[0] != "console" || got[1] != "dashboard" {
		t.Errorf("Repos = %v, want the trimmed pair", got)
	}
	if !cfg.Agents["schema"].ReposIsOperatorOwned() {
		t.Error("an operator scope edit did not claim ownership of the field")
	}

	raw, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "repos:") || !strings.Contains(string(raw), "repos_owner: operator") {
		t.Errorf("scope not persisted:\n%s", raw)
	}

	reloaded, err := Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.AgentServesRepo("schema", "console") || reloaded.AgentServesRepo("schema", "laredo/cuga-agent") {
		t.Error("scope did not survive the reload intact")
	}

	// Re-applying the same scope is a no-op.
	if changed, err := cfg.SetAgentReposAndSave("schema", []string{"console", "dashboard"}); err != nil || changed {
		t.Errorf("re-apply: changed=%v err=%v, want false/nil", changed, err)
	}

	// Clearing returns the agent to hive-wide, and that decision is itself
	// operator-owned.
	if changed, err := cfg.SetAgentReposAndSave("schema", nil); err != nil || !changed {
		t.Fatalf("clear: changed=%v err=%v", changed, err)
	}
	if cfg.Agents["schema"].IsRepoScoped() {
		t.Error("agent still scoped after clearing")
	}
	if !cfg.AgentServesRepo("schema", "laredo/cuga-agent") {
		t.Error("cleared agent does not serve every repo")
	}
}

func TestSetAgentReposAndSave_Errors(t *testing.T) {
	cfg := newScopeTestConfig(t, nil)
	if _, err := cfg.SetAgentReposAndSave("", []string{"console"}); err == nil {
		t.Error("expected an error for an empty agent name")
	}
	if _, err := cfg.SetAgentReposAndSave("nope", []string{"console"}); err == nil {
		t.Error("expected an error for an unknown agent")
	}
	var nilCfg *Config
	if _, err := nilCfg.SetAgentReposAndSave("scanner", nil); err == nil {
		t.Error("expected an error on a nil config")
	}
	if !nilCfg.AgentServesRepo("scanner", "console") {
		t.Error("nil config must fail open")
	}
}

// A scope that matches nothing is inert, and inert is silent without this.
func TestAgentRepoScopeWarnings(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"ok":      {Backend: "claude", Repos: []string{"console"}},
		"typo":    {Backend: "claude", Repos: []string{"console", "consle"}},
		"orphan":  {Backend: "claude", Repos: []string{"gone-repo"}},
		"unscope": {Backend: "claude"},
	})
	warnings := AgentRepoScopeWarnings(cfg)
	joined := strings.Join(warnings, "\n")
	if strings.Contains(joined, `"ok"`) || strings.Contains(joined, `"unscope"`) {
		t.Errorf("warned about a healthy agent:\n%s", joined)
	}
	if !strings.Contains(joined, `"consle"`) {
		t.Errorf("no warning for the unwatched entry:\n%s", joined)
	}
	// The orphan gets BOTH: the bad entry and the "no repos to work" summary.
	if !strings.Contains(joined, `"gone-repo"`) || !strings.Contains(joined, "no repos to work") {
		t.Errorf("orphaned agent not fully reported:\n%s", joined)
	}
	if got := AgentRepoScopeWarnings(nil); got != nil {
		t.Errorf("AgentRepoScopeWarnings(nil) = %v, want nil", got)
	}
}

// A hive with no scoped agents must save byte-identically to before: both
// fields are omitempty.
func TestAgentRepos_AbsentFromConfigWhenUnused(t *testing.T) {
	cfg := newScopeTestConfig(t, nil)
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repos_owner") {
		t.Errorf("repos_owner written for a hive with no scoped agents:\n%s", raw)
	}
}

func TestRepoScopedAgents(t *testing.T) {
	cfg := newScopeTestConfig(t, map[string]AgentConfig{
		"zeta":    {Backend: "claude", Repos: []string{"console"}},
		"alpha":   {Backend: "claude", Repos: []string{"dashboard"}},
		"unscope": {Backend: "claude"},
	})
	got := cfg.RepoScopedAgents()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Errorf("RepoScopedAgents = %v, want [alpha zeta]", got)
	}
}

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ownerPromptTestConfig() *Config {
	return &Config{
		Project: ProjectConfig{Org: "myorg", Repos: []string{"repo1", "Other-Org/shared", " "}},
	}
}

func TestIsManagedRepo(t *testing.T) {
	c := ownerPromptTestConfig()
	for slug, want := range map[string]bool{
		"myorg/repo1":      true,
		"MyOrg/Repo1":      true,
		" myorg/repo1 ":    true,
		"other-org/shared": true,
		"myorg/shared":     false,
		"evil/repo1":       false,
		"repo1":            false,
		"":                 false,
	} {
		if got := c.IsManagedRepo(slug); got != want {
			t.Errorf("IsManagedRepo(%q) = %v, want %v", slug, got, want)
		}
	}
	var nilCfg *Config
	if nilCfg.IsManagedRepo("myorg/repo1") || nilCfg.ManagedRepoSlugs() != nil || nilCfg.OwnerPromptRepos() != nil || nilCfg.OwnerPromptRepoAllowed("myorg/repo1") || nilCfg.SeedGitHubPromptAllowed("myorg/repo1") {
		t.Fatal("nil config must deny everything")
	}
}

func TestManagedRepoSlugs(t *testing.T) {
	got := ownerPromptTestConfig().ManagedRepoSlugs()
	if strings.Join(got, ",") != "myorg/repo1,Other-Org/shared" {
		t.Fatalf("ManagedRepoSlugs = %v", got)
	}
	noOrg := &Config{Project: ProjectConfig{Repos: []string{"bare", "a/b"}}}
	if got := noOrg.ManagedRepoSlugs(); strings.Join(got, ",") != "a/b" {
		t.Fatalf("a bare repo with no org is not a slug: %v", got)
	}
}

func TestNormalizeOwnerPromptRepo(t *testing.T) {
	c := ownerPromptTestConfig()
	if got, err := c.NormalizeOwnerPromptRepo(" MYORG/REPO1 "); err != nil || got != "myorg/repo1" {
		t.Fatalf("managed repo: got %q, %v", got, err)
	}
	if _, err := c.NormalizeOwnerPromptRepo("evil/repo"); !errors.Is(err, ErrNotManagedRepo) {
		t.Fatalf("non-managed repo: err = %v, want ErrNotManagedRepo", err)
	}
	for _, bad := range []string{"", "repo1", "a/b/c", "/repo", "org/"} {
		if _, err := c.NormalizeOwnerPromptRepo(bad); err == nil || errors.Is(err, ErrNotManagedRepo) {
			t.Fatalf("malformed %q: err = %v, want a slug error", bad, err)
		}
	}
	var nilCfg *Config
	if _, err := nilCfg.NormalizeOwnerPromptRepo("a/b"); !errors.Is(err, ErrNotManagedRepo) {
		t.Fatalf("nil config: err = %v", err)
	}
}

func TestGitHubPromptAllowedOwnerPath(t *testing.T) {
	c := ownerPromptTestConfig()
	c.Variables.Security.OwnerGitHubPromptRepos = []OwnerPromptRepo{{Repo: "myorg/repo1"}, {Repo: "evil/repo"}}
	c.Variables.Security.GitHubPromptAllowlist = []string{"seed/defs"}

	// Owner approval works without allow_github_prompt, for that repo only.
	if !c.GitHubPromptAllowed("myorg/repo1") || !c.GitHubDefinitionAllowed("MYORG/repo1") {
		t.Fatal("owner-approved managed repo must be allowed")
	}
	if c.GitHubPromptAllowed("seed/defs") {
		t.Fatal("an owner approval must not switch the seed allowlist on")
	}
	// An approval for a repo outside project.repos never counts.
	if c.GitHubPromptAllowed("evil/repo") {
		t.Fatal("owner entry for a non-managed repo must be inert")
	}
	// Dropping the repo from project.repos revokes it.
	c.Project.Repos = []string{"other"}
	if c.GitHubPromptAllowed("myorg/repo1") {
		t.Fatal("approval must lapse when the repo is no longer managed")
	}
	if c.GitHubPromptAllowed("") {
		t.Fatal("empty slug must be denied")
	}

	// The seed path is unchanged.
	c.Variables.Security.AllowGitHubPrompt = true
	if !c.GitHubPromptAllowed("seed/defs") || !c.SeedGitHubPromptAllowed("Seed/Defs") {
		t.Fatal("seed allowlist must still work")
	}
	if c.SeedGitHubPromptAllowed("") {
		t.Fatal("empty slug must be denied by the seed gate")
	}
}

func redirectPVCFiles(t *testing.T, dir string) {
	t.Helper()
	origRuntime, origOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { RuntimeConfigFile, DashboardOverlayFile = origRuntime, origOverlay })
}

const ownerPromptSeed = `
project:
  org: testorg
  repos: [repo1, repo2]
github:
  token: ghp_x
agents:
  scanner:
    backend: copilot
`

func TestOwnerPromptRepoPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	redirectPVCFiles(t, dir)
	seedPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(seedPath, []byte(ownerPromptSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(seedPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHubPromptAllowed("testorg/repo1") {
		t.Fatal("nothing approved yet")
	}

	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	entry, changed, err := cfg.AllowOwnerPromptRepoAndSave("TestOrg/Repo1", "alice", at)
	if err != nil || !changed {
		t.Fatalf("approve: changed=%v err=%v", changed, err)
	}
	if entry.Repo != "testorg/repo1" || entry.ApprovedBy != "alice" || entry.ApprovedAt != "2026-11-16T12:00:00Z" {
		t.Fatalf("entry = %+v", entry)
	}
	// Re-approving is a no-op that keeps the first record.
	again, changed, err := cfg.AllowOwnerPromptRepoAndSave("testorg/repo1", "bob", at.Add(time.Hour))
	if err != nil || changed || again.ApprovedBy != "alice" {
		t.Fatalf("re-approve: %+v changed=%v err=%v", again, changed, err)
	}
	// A non-managed repo is refused and nothing changes.
	if _, changed, err := cfg.AllowOwnerPromptRepoAndSave("evil/repo", "alice", at); !errors.Is(err, ErrNotManagedRepo) || changed {
		t.Fatalf("non-managed: changed=%v err=%v", changed, err)
	}

	for _, p := range []string{seedPath, RuntimeConfigFile} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if !strings.Contains(string(data), "owner_github_prompt_repos") {
			t.Fatalf("%s does not carry the approval:\n%s", p, data)
		}
	}

	reloaded, err := Load(seedPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.GitHubPromptAllowed("testorg/repo1") || reloaded.GitHubPromptAllowed("testorg/repo2") {
		t.Fatalf("after reload: %+v", reloaded.OwnerPromptRepos())
	}
	if got := reloaded.OwnerPromptRepos(); len(got) != 1 || got[0].ApprovedBy != "alice" {
		t.Fatalf("reloaded approvals = %+v", got)
	}

	// Revoke persists too.
	if changed, err := reloaded.RevokeOwnerPromptRepoAndSave("TESTORG/repo1"); err != nil || !changed {
		t.Fatalf("revoke: changed=%v err=%v", changed, err)
	}
	if changed, err := reloaded.RevokeOwnerPromptRepoAndSave("testorg/repo1"); err != nil || changed {
		t.Fatalf("second revoke: changed=%v err=%v", changed, err)
	}
	again2, err := Load(seedPath)
	if err != nil {
		t.Fatalf("reload after revoke: %v", err)
	}
	if again2.GitHubPromptAllowed("testorg/repo1") || len(again2.OwnerPromptRepos()) != 0 {
		t.Fatal("revoke did not survive reload")
	}
}

func TestRevokeOwnerPromptRepoKeepsOthers(t *testing.T) {
	c := ownerPromptTestConfig()
	c.Variables.Security.OwnerGitHubPromptRepos = []OwnerPromptRepo{{Repo: "a/one"}, {Repo: "myorg/repo1"}, {Repo: "b/two"}}
	// No source path: the in-memory change happens and the save error is returned.
	changed, err := c.RevokeOwnerPromptRepoAndSave("myorg/repo1")
	if !changed || err == nil {
		t.Fatalf("changed=%v err=%v, want change plus a no-source-path error", changed, err)
	}
	got := c.OwnerPromptRepos()
	if len(got) != 2 || got[0].Repo != "a/one" || got[1].Repo != "b/two" {
		t.Fatalf("remaining = %+v", got)
	}
	var nilCfg *Config
	if _, err := nilCfg.RevokeOwnerPromptRepoAndSave("a/b"); err == nil {
		t.Fatal("nil config revoke must error")
	}
}

// TestOwnerPromptRepoOverlayCannotApprove keeps the seed-only invariant: the
// user-writable dashboard overlay cannot approve a repo.
func TestOwnerPromptRepoOverlayCannotApprove(t *testing.T) {
	dir := t.TempDir()
	redirectPVCFiles(t, dir)
	seedPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(seedPath, []byte(ownerPromptSeed), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := ownerPromptSeed + `
variables:
  security:
    owner_github_prompt_repos:
      - repo: testorg/repo1
`
	if err := os.WriteFile(DashboardOverlayFile, []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.GitHubPromptAllowed("testorg/repo1") {
		t.Fatal("the dashboard overlay must not be able to approve a prompt source repo")
	}
}

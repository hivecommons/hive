package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRepoOpenPRsMode(t *testing.T) {
	cases := map[string]struct {
		want RepoOpenPRsMode
		ok   bool
	}{
		"":              {RepoOpenPRsOn, true},
		"on":            {RepoOpenPRsOn, true},
		" OFF ":         {RepoOpenPRsOff, true},
		"workflow_only": {RepoOpenPRsWorkflowOnly, true},
		"workflow-only": {RepoOpenPRsWorkflowOnly, true},
		"sometimes":     {"", false},
	}
	for in, c := range cases {
		got, ok := ParseRepoOpenPRsMode(in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseRepoOpenPRsMode(%q) = %q, %v; want %q, %v", in, got, ok, c.want, c.ok)
		}
	}
}

// Resolver precedence: project.open_prs: false wins over every repo; then
// the repo's own mode; unset is on. Other repos are unaffected.
func TestRepoOpenPRsModePrecedence(t *testing.T) {
	cfg := &Config{Project: ProjectConfig{Org: "acme", Repos: []string{"docs", "api", "web"}}}
	cfg.Project.RepoPolicies = []RepoPolicy{
		{Repo: "docs", OpenPRs: "workflow_only"},
		{Repo: "acme/api", OpenPRs: "off"},
	}
	for repo, want := range map[string]RepoOpenPRsMode{
		"docs":      RepoOpenPRsWorkflowOnly,
		"acme/docs": RepoOpenPRsWorkflowOnly,
		"api":       RepoOpenPRsOff,
		"web":       RepoOpenPRsOn,
	} {
		if got := cfg.RepoOpenPRsMode(repo); got != want {
			t.Errorf("RepoOpenPRsMode(%q) = %q, want %q", repo, got, want)
		}
	}

	off := false
	cfg.Project.OpenPRs = &off
	for _, repo := range []string{"docs", "api", "web"} {
		if got := cfg.RepoOpenPRsMode(repo); got != RepoOpenPRsOff {
			t.Errorf("project.open_prs=false: RepoOpenPRsMode(%q) = %q, want off", repo, got)
		}
		if reason := cfg.RepoOpenPRsRefusal(repo, true); !strings.Contains(reason, "project.open_prs") {
			t.Errorf("project.open_prs=false must refuse even a workflow PR on %s, got %q", repo, reason)
		}
	}
	on := true
	cfg.Project.OpenPRs = &on
	if got := cfg.RepoOpenPRsMode("docs"); got != RepoOpenPRsWorkflowOnly {
		t.Errorf("project.open_prs=true must not override the repo mode, got %q", got)
	}

	var nilCfg *Config
	if got := nilCfg.RepoOpenPRsMode("docs"); got != RepoOpenPRsOn {
		t.Errorf("nil config = %q, want on", got)
	}
}

func TestRepoOpenPRsRefusal(t *testing.T) {
	cfg := &Config{Project: ProjectConfig{Org: "acme", Repos: []string{"docs", "api", "web"}}}
	cfg.Project.RepoPolicies = []RepoPolicy{
		{Repo: "docs", OpenPRs: "workflow_only"},
		{Repo: "api", OpenPRs: "off"},
	}
	if r := cfg.RepoOpenPRsRefusal("docs", false); !strings.Contains(r, "workflow_only") {
		t.Errorf("workflow_only must refuse a non-workflow PR, got %q", r)
	}
	if r := cfg.RepoOpenPRsRefusal("docs", true); r != "" {
		t.Errorf("workflow_only must allow a workflow PR, got %q", r)
	}
	for _, wf := range []bool{false, true} {
		if r := cfg.RepoOpenPRsRefusal("api", wf); !strings.Contains(r, "open_prs: off") {
			t.Errorf("off must refuse every PR (workflow=%v), got %q", wf, r)
		}
		if r := cfg.RepoOpenPRsRefusal("web", wf); r != "" {
			t.Errorf("a repo without a mode must be unaffected (workflow=%v), got %q", wf, r)
		}
	}
}

func TestRepoOpenPRsWidens(t *testing.T) {
	if !RepoOpenPRsOn.Widens(RepoOpenPRsOff) || !RepoOpenPRsWorkflowOnly.Widens(RepoOpenPRsOff) || !RepoOpenPRsOn.Widens(RepoOpenPRsWorkflowOnly) {
		t.Error("moving toward on must widen")
	}
	if RepoOpenPRsOff.Widens(RepoOpenPRsOn) || RepoOpenPRsWorkflowOnly.Widens(RepoOpenPRsOn) || RepoOpenPRsOn.Widens(RepoOpenPRsOn) {
		t.Error("narrowing or keeping the mode must not widen")
	}
}

func TestSetRepoOpenPRsForRepoAndSave(t *testing.T) {
	cfg := newPauseTestConfig(t, "api", "web")
	changed, err := cfg.SetRepoOpenPRsForRepoAndSave("api", RepoOpenPRsWorkflowOnly)
	if err != nil || !changed {
		t.Fatalf("set workflow_only: changed=%v err=%v", changed, err)
	}
	if got := cfg.RepoOpenPRsMode("acme/api"); got != RepoOpenPRsWorkflowOnly {
		t.Fatalf("mode after set = %q", got)
	}
	reloaded, err := Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("Load() after save: %v", err)
	}
	if got := reloaded.RepoOpenPRsMode("api"); got != RepoOpenPRsWorkflowOnly {
		t.Fatalf("workflow_only did not persist, got %q", got)
	}
	if got := reloaded.RepoOpenPRsMode("web"); got != RepoOpenPRsOn {
		t.Fatalf("other repo changed: %q", got)
	}
	changed, err = cfg.SetRepoOpenPRsForRepoAndSave("ACME/API", "workflow-only")
	if err != nil || changed {
		t.Fatalf("idempotent set: changed=%v err=%v", changed, err)
	}
	if _, err := cfg.SetRepoOpenPRsForRepoAndSave("api", "sometimes"); err == nil {
		t.Fatal("unknown mode must be refused")
	}
	if changed, err = cfg.SetRepoOpenPRsForRepoAndSave("api", RepoOpenPRsOn); err != nil || !changed {
		t.Fatalf("clear: changed=%v err=%v", changed, err)
	}
	if _, ok := cfg.RepoPolicyFor("api"); ok {
		t.Fatal("empty repo policy should be pruned once open_prs is cleared")
	}
}

// Clearing open_prs must not prune a policy row that still carries a forge
// override.
func TestSetRepoOpenPRsKeepsForgeRow(t *testing.T) {
	cfg := newPauseTestConfig(t, "api")
	cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "api", Forge: "github", OpenPRs: "off"}}
	if _, err := cfg.SetRepoOpenPRsForRepoAndSave("api", RepoOpenPRsOn); err != nil {
		t.Fatal(err)
	}
	rp, ok := cfg.RepoPolicyFor("api")
	if !ok || rp.Forge != "github" {
		t.Fatalf("forge override lost when clearing open_prs: %+v ok=%v", rp, ok)
	}
}

func TestValidateRepoOpenPRs(t *testing.T) {
	cfg := &Config{Project: ProjectConfig{RepoPolicies: []RepoPolicy{{Repo: "a", OpenPRs: "workflow_only"}, {Repo: "b"}}}}
	if err := cfg.validateRepoOpenPRs(); err != nil {
		t.Fatalf("valid modes refused: %v", err)
	}
	cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies, RepoPolicy{Repo: "c", OpenPRs: "maybe"})
	if err := cfg.validateRepoOpenPRs(); err == nil || !strings.Contains(err.Error(), "repo_policies[2].open_prs") {
		t.Fatalf("unknown mode must be refused with its field, got %v", err)
	}
	// A value that got in without validation fails closed.
	if got := cfg.RepoOpenPRsMode("c"); got != RepoOpenPRsOff {
		t.Fatalf("unreadable mode must fail closed, got %q", got)
	}
}

// The dashboard writes the mode into the overlay; a reload from the seed
// (LoadWithDashboardOverlay) must carry it, and must leave the seed's other
// per-repo fields alone (#19740 shape).
func TestLoadWithDashboardOverlay_CarriesRepoOpenPRs(t *testing.T) {
	seed := `
project:
  org: testorg
  repos: [docs, api]
  repo_policies:
    - repo: api
      companion_repos: [api-site]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
`
	// A short overlay (no agents) as the dashboard can leave it.
	overlay := `
project:
  org: testorg
  repos: [docs, api]
  repo_policies:
    - repo: docs
      open_prs: workflow_only
    - repo: api
      open_prs: "off"
`
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = orig })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if got := merged.RepoOpenPRsMode("docs"); got != RepoOpenPRsWorkflowOnly {
		t.Errorf("docs open_prs lost on reload: %q", got)
	}
	if got := merged.RepoOpenPRsMode("api"); got != RepoOpenPRsOff {
		t.Errorf("api open_prs lost on reload: %q", got)
	}
	if got := merged.CompanionReposFor("api"); len(got) != 1 || got[0] != "testorg/api-site" {
		t.Errorf("seed companion_repos clobbered: %v", got)
	}

	// A silent overlay keeps the seed's value.
	if err := os.WriteFile(overlayPath, []byte("project:\n  org: testorg\n  repos: [docs, api]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte(strings.Replace(seed, "companion_repos: [api-site]", "open_prs: workflow_only", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err = LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay (silent overlay): %v", err)
	}
	if got := merged.RepoOpenPRsMode("api"); got != RepoOpenPRsWorkflowOnly {
		t.Errorf("silent overlay clobbered the seed's open_prs: %q", got)
	}
}

// A workflow hand-off that opens a PR resolves to no agent in an off repo,
// and still resolves in a workflow_only one (that is what the mode admits).
func TestWorkflowDispatchQualifiesUnderRepoOpenPRs(t *testing.T) {
	c := advisoryFleet()
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); !ok {
		t.Fatalf("baseline: quality must qualify, got %q", why)
	}
	c.Project.RepoPolicies = []RepoPolicy{{Repo: "cncf/techdocs", OpenPRs: string(RepoOpenPRsWorkflowOnly)}}
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); !ok {
		t.Fatalf("workflow_only must admit the hand-off, got %q", why)
	}
	c.Project.RepoPolicies = []RepoPolicy{{Repo: "cncf/techdocs", OpenPRs: string(RepoOpenPRsOff)}}
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); ok || !strings.Contains(why, "open PRs is off") {
		t.Fatalf("off must refuse a PR hand-off: ok=%v why=%q", ok, why)
	}
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs", Reviews: true}); !ok {
		t.Fatalf("a reviewing hand-off comments only and is unaffected by off, got %q", why)
	}
}

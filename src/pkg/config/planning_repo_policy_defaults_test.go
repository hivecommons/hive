package config

import (
	"testing"
)

// The planning label trigger (RFC #7993) resolves every knob through an
// OrDefault getter; a wrong default silently changes which labels arm the
// architect lane. Pin both branches of all five getters.
func TestPlanningConfig_OrDefaultGetters(t *testing.T) {
	var zero PlanningConfig

	if got := zero.PlanLabelsOrDefault(); len(got) != 1 || got[0] != DefaultPlanLabel {
		t.Errorf("PlanLabelsOrDefault() zero = %v, want [%q]", got, DefaultPlanLabel)
	}
	if got := zero.DesignLabelsOrDefault(); len(got) != 1 || got[0] != DefaultDesignLabel {
		t.Errorf("DesignLabelsOrDefault() zero = %v, want [%q]", got, DefaultDesignLabel)
	}
	if got := zero.DesignApprovedLabelOrDefault(); got != DefaultDesignApprovedLabel {
		t.Errorf("DesignApprovedLabelOrDefault() zero = %q, want %q", got, DefaultDesignApprovedLabel)
	}
	if got := zero.MaxDesignRevisionsOrDefault(); got != DefaultMaxDesignRevisions {
		t.Errorf("MaxDesignRevisionsOrDefault() zero = %d, want %d", got, DefaultMaxDesignRevisions)
	}
	if got := zero.MaxConcurrentDesignsOrDefault(); got != DefaultMaxConcurrentDesigns {
		t.Errorf("MaxConcurrentDesignsOrDefault() zero = %d, want %d", got, DefaultMaxConcurrentDesigns)
	}

	set := PlanningConfig{
		PlanLabels:           []string{"epic", "roadmap"},
		DesignLabels:         []string{"rfc"},
		DesignApprovedLabel:  "rfc-approved",
		MaxDesignRevisions:   7,
		MaxConcurrentDesigns: 1,
	}
	if got := set.PlanLabelsOrDefault(); len(got) != 2 || got[0] != "epic" || got[1] != "roadmap" {
		t.Errorf("PlanLabelsOrDefault() configured = %v, want [epic roadmap]", got)
	}
	if got := set.DesignLabelsOrDefault(); len(got) != 1 || got[0] != "rfc" {
		t.Errorf("DesignLabelsOrDefault() configured = %v, want [rfc]", got)
	}
	if got := set.DesignApprovedLabelOrDefault(); got != "rfc-approved" {
		t.Errorf("DesignApprovedLabelOrDefault() configured = %q, want rfc-approved", got)
	}
	if got := set.MaxDesignRevisionsOrDefault(); got != 7 {
		t.Errorf("MaxDesignRevisionsOrDefault() configured = %d, want 7", got)
	}
	if got := set.MaxConcurrentDesignsOrDefault(); got != 1 {
		t.Errorf("MaxConcurrentDesignsOrDefault() configured = %d, want 1", got)
	}
}

// AppSignedCommitsEnabled gates whether the PR-request watcher re-authors
// agent branches through createCommitOnBranch. Opt-in: nil and false both
// mean off.
func TestAppSignedCommitsEnabled(t *testing.T) {
	if (GitHubConfig{}).AppSignedCommitsEnabled() {
		t.Error("AppSignedCommitsEnabled() with nil flag = true, want false (opt-in)")
	}
	f := false
	if (GitHubConfig{AppSignedCommits: &f}).AppSignedCommitsEnabled() {
		t.Error("AppSignedCommitsEnabled() with explicit false = true, want false")
	}
	tr := true
	if !(GitHubConfig{AppSignedCommits: &tr}).AppSignedCommitsEnabled() {
		t.Error("AppSignedCommitsEnabled() with explicit true = false, want true")
	}
}

// SelfAuthorizationHoldEnvOverrideSet tells callers (e.g. the dashboard)
// that HIVE_SELF_AUTHORIZATION_HOLD is pinning the effective value, so a
// config edit will not take effect. Without the env var it must be false.
func TestSelfAuthorizationHoldEnvOverrideSet(t *testing.T) {
	if (GitHubConfig{}).SelfAuthorizationHoldEnvOverrideSet() {
		t.Error("SelfAuthorizationHoldEnvOverrideSet() with no env override = true, want false")
	}

	t.Setenv("HIVE_SELF_AUTHORIZATION_HOLD", "false")
	yaml := `
project:
  org: my-org
  repos: [repo-a]
github:
  token: ghp_tok
agents:
  w:
    backend: claude
`
	cfg, err := Load(writeTempConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.GitHub.SelfAuthorizationHoldEnvOverrideSet() {
		t.Error("SelfAuthorizationHoldEnvOverrideSet() after HIVE_SELF_AUTHORIZATION_HOLD load = false, want true")
	}
}

// SetSelfAuthorizationHoldForRepoAndSave is the dashboard's single-repo
// #5117 override mutator: it must report whether anything changed, persist
// only real changes, and let nil clear the override back to inheritance.
func TestSetSelfAuthorizationHoldForRepoAndSave(t *testing.T) {
	var nilCfg *Config
	if _, err := nilCfg.SetSelfAuthorizationHoldForRepoAndSave("api", nil); err == nil {
		t.Error("nil config: want error, got nil")
	}

	cfg := newPauseTestConfig(t, "api", "web")
	if _, err := cfg.SetSelfAuthorizationHoldForRepoAndSave("   ", nil); err == nil {
		t.Error("blank repo: want error, got nil")
	}

	f := false
	changed, err := cfg.SetSelfAuthorizationHoldForRepoAndSave("api", &f)
	if err != nil || !changed {
		t.Fatalf("set new override: changed=%v err=%v, want true nil", changed, err)
	}
	if cfg.SelfAuthorizationHoldEnabledForRepo("api") {
		t.Error("override false not effective after set")
	}

	// Persisted: a fresh Load must see the override.
	reloaded, err := Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("Load() after save error = %v", err)
	}
	rp, ok := reloaded.RepoPolicyFor("api")
	if !ok || rp.SelfAuthorizationHold == nil || *rp.SelfAuthorizationHold {
		t.Fatalf("reloaded RepoPolicyFor(api) = %+v ok=%v, want persisted false override", rp, ok)
	}

	// Same value again — no change, no save.
	changed, err = cfg.SetSelfAuthorizationHoldForRepoAndSave("api", &f)
	if err != nil || changed {
		t.Errorf("idempotent set: changed=%v err=%v, want false nil", changed, err)
	}

	// Flip existing override — change. Org-qualified spelling must match the
	// bare entry.
	tr := true
	changed, err = cfg.SetSelfAuthorizationHoldForRepoAndSave("acme/api", &tr)
	if err != nil || !changed {
		t.Errorf("flip override: changed=%v err=%v, want true nil", changed, err)
	}
	if !cfg.SelfAuthorizationHoldEnabledForRepo("api") {
		t.Error("override true not effective after flip")
	}

	// nil clears the override so the repo inherits the hive-wide default.
	changed, err = cfg.SetSelfAuthorizationHoldForRepoAndSave("api", nil)
	if err != nil || !changed {
		t.Errorf("clear override: changed=%v err=%v, want true nil", changed, err)
	}
	if _, ok := cfg.RepoPolicyFor("api"); ok {
		t.Error("policy entry should be removed once its only override is cleared")
	}

	// Clearing a repo that has no override is a no-op.
	changed, err = cfg.SetSelfAuthorizationHoldForRepoAndSave("web", nil)
	if err != nil || changed {
		t.Errorf("clear absent override: changed=%v err=%v, want false nil", changed, err)
	}
}

// ClearRepoPolicies runs when a repo-list save migrates the hive to another
// org: bare overrides from the previous org must not silently retarget to
// same-named repos there.
func TestClearRepoPolicies(t *testing.T) {
	var nilCfg *Config
	if nilCfg.ClearRepoPolicies() {
		t.Error("nil config: ClearRepoPolicies() = true, want false")
	}

	cfg := newPauseTestConfig(t, "api")
	if cfg.ClearRepoPolicies() {
		t.Error("empty policies: ClearRepoPolicies() = true, want false")
	}

	f := false
	cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "api", SelfAuthorizationHold: &f}}
	if !cfg.ClearRepoPolicies() {
		t.Error("non-empty policies: ClearRepoPolicies() = false, want true")
	}
	if cfg.Project.RepoPolicies != nil {
		t.Errorf("RepoPolicies after clear = %v, want nil", cfg.Project.RepoPolicies)
	}
}

// PruneRepoPoliciesToWatched drops overrides for repos no longer in
// project.repos, matching every spelling the pause machinery accepts.
func TestPruneRepoPoliciesToWatched(t *testing.T) {
	var nilCfg *Config
	if nilCfg.PruneRepoPoliciesToWatched() {
		t.Error("nil config: PruneRepoPoliciesToWatched() = true, want false")
	}

	cfg := newPauseTestConfig(t, "api", "web")
	if cfg.PruneRepoPoliciesToWatched() {
		t.Error("no policies: PruneRepoPoliciesToWatched() = true, want false")
	}

	f := false
	cfg.Project.RepoPolicies = []RepoPolicy{
		{Repo: "API", SelfAuthorizationHold: &f},      // watched, case-insensitive
		{Repo: "acme/web", SelfAuthorizationHold: &f}, // watched, org-qualified
	}
	if cfg.PruneRepoPoliciesToWatched() {
		t.Error("all watched: PruneRepoPoliciesToWatched() = true, want false")
	}
	if len(cfg.Project.RepoPolicies) != 2 {
		t.Fatalf("all-watched prune changed policies: %v", cfg.Project.RepoPolicies)
	}

	cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies,
		RepoPolicy{Repo: "gone", SelfAuthorizationHold: &f})
	if !cfg.PruneRepoPoliciesToWatched() {
		t.Error("unwatched entry present: PruneRepoPoliciesToWatched() = false, want true")
	}
	if len(cfg.Project.RepoPolicies) != 2 {
		t.Fatalf("prune kept %v, want the 2 watched entries", cfg.Project.RepoPolicies)
	}
	for _, rp := range cfg.Project.RepoPolicies {
		if rp.Repo == "gone" {
			t.Error("unwatched override survived prune")
		}
	}

	// Pruning the last remaining entries resets the slice to nil.
	cfg.Project.RepoPolicies = []RepoPolicy{{Repo: "gone", SelfAuthorizationHold: &f}}
	if !cfg.PruneRepoPoliciesToWatched() {
		t.Error("all unwatched: PruneRepoPoliciesToWatched() = false, want true")
	}
	if cfg.Project.RepoPolicies != nil {
		t.Errorf("RepoPolicies after full prune = %v, want nil", cfg.Project.RepoPolicies)
	}
}

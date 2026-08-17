package config

import (
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// repo_target.go — ValidateRepoTargets (the Config-level wrapper)
// ---------------------------------------------------------------------------

func TestValidateRepoTargets_NilConfig(t *testing.T) {
	if got := ValidateRepoTargets(nil); got != nil {
		t.Errorf("nil config should return nil, got %+v", got)
	}
}

func TestValidateRepoTargets_ValidConfig(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{
			Org:         "kubestellar",
			PrimaryRepo: "hive",
			Repos:       []string{"hive"},
		},
	}
	if got := ValidateRepoTargets(cfg); got != nil {
		t.Errorf("valid config should return nil, got %+v", got)
	}
}

func TestValidateRepoTargets_BadOrg(t *testing.T) {
	cfg := &Config{
		Project: ProjectConfig{
			Org:   "https://github.com/kubestellar",
			Repos: []string{"hive"},
		},
	}
	got := ValidateRepoTargets(cfg)
	if got == nil {
		t.Fatal("expected issue for URL org")
	}
	if got.Field != "project.org" {
		t.Errorf("field = %q, want project.org", got.Field)
	}
}

// ---------------------------------------------------------------------------
// config.go — KnownForge
// ---------------------------------------------------------------------------

func TestKnownForge(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"https://github.com", true},
		{"github.ibm.com", true},
		{"https://github.ibm.com/api/v3", true},
		{"unknown-forge.example.com", false},
		{"", true}, // empty defaults to ForgePublic via normalizeForgeHost
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := KnownForge(tt.host); got != tt.want {
				t.Errorf("KnownForge(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// provenance.go — ForgeOfAppID (exported)
// ---------------------------------------------------------------------------

func TestForgeOfAppID_Exported(t *testing.T) {
	if got := ForgeOfAppID(PublicGitHubAppID); got != "github.com" {
		t.Errorf("public = %q", got)
	}
	if got := ForgeOfAppID(EnterpriseGitHubAppID); got != "github.ibm.com" {
		t.Errorf("enterprise = %q", got)
	}
	if got := ForgeOfAppID(0); got != "" {
		t.Errorf("unknown = %q", got)
	}
}

// ---------------------------------------------------------------------------
// provenance.go — RejectIdentitySet with errors.Is
// ---------------------------------------------------------------------------

func TestRejectIdentitySet_ErrorsIs(t *testing.T) {
	err := RejectIdentitySet(GitHubConfig{
		AppID:   EnterpriseGitHubAppID,
		AppSlug: EnterpriseGitHubAppSlug,
		// empty URLs → defaults to public → split-forge
	})
	if err == nil {
		t.Fatal("expected error for split-forge config")
	}
	if !errors.Is(err, ErrIdentitySetMismatch) {
		t.Errorf("error should wrap ErrIdentitySetMismatch, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// provenance.go — seedHasIsPublic (layers_yaml.go)
// ---------------------------------------------------------------------------

func TestSeedHasIsPublic(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{"present and true", "hub:\n  is_public: true\n", true},
		{"present and false", "hub:\n  is_public: false\n", true},
		{"absent — other hub fields", "hub:\n  url: https://example.com\n", false},
		{"no hub block at all", "project:\n  org: test\n", false},
		{"empty YAML has no hub", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := seedHasIsPublic([]byte(tt.yaml)); got != tt.want {
				t.Errorf("seedHasIsPublic = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// agent_overlay.go — MarkAgentRemoved / ClearAgentRemoved
// ---------------------------------------------------------------------------

func TestMarkAgentRemoved(t *testing.T) {
	cfg := &Config{}

	// First mark returns true.
	if !cfg.MarkAgentRemoved("scanner") {
		t.Error("first mark should return true")
	}
	if !cfg.IsAgentRemoved("scanner") {
		t.Error("scanner should be removed")
	}

	// Duplicate mark returns false.
	if cfg.MarkAgentRemoved("scanner") {
		t.Error("duplicate mark should return false")
	}

	// Empty name returns false.
	if cfg.MarkAgentRemoved("") {
		t.Error("empty name should return false")
	}
}

func TestClearAgentRemoved(t *testing.T) {
	cfg := &Config{}
	cfg.MarkAgentRemoved("scanner")
	cfg.MarkAgentRemoved("quality")

	// Clear scanner.
	if !cfg.ClearAgentRemoved("scanner") {
		t.Error("clear existing should return true")
	}
	if cfg.IsAgentRemoved("scanner") {
		t.Error("scanner should no longer be removed")
	}

	// Quality still removed.
	if !cfg.IsAgentRemoved("quality") {
		t.Error("quality should still be removed")
	}

	// Clear nonexistent returns false.
	if cfg.ClearAgentRemoved("nonexistent") {
		t.Error("clearing nonexistent should return false")
	}
}

// ---------------------------------------------------------------------------
// config.go — AgentConfig replica/ownership methods
// ---------------------------------------------------------------------------

func TestAgentConfig_IsReplica(t *testing.T) {
	a := AgentConfig{}
	if a.IsReplica() {
		t.Error("default agent should not be a replica")
	}
	a.ReplicaOf = "scanner"
	if !a.IsReplica() {
		t.Error("agent with ReplicaOf should be a replica")
	}
}

func TestAgentConfig_BaseName(t *testing.T) {
	tests := []struct {
		name      string
		agent     AgentConfig
		wantBase  string
	}{
		{"replica returns ReplicaOf", AgentConfig{ReplicaOf: "scanner", name: "scanner-2", ID: "s2"}, "scanner"},
		{"non-replica returns name", AgentConfig{name: "quality", ID: "q"}, "quality"},
		{"no name returns ID", AgentConfig{ID: "q"}, "q"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.agent.BaseName(); got != tt.wantBase {
				t.Errorf("BaseName = %q, want %q", got, tt.wantBase)
			}
		})
	}
}

func TestAgentConfig_ModelIsOperatorOwned(t *testing.T) {
	a := AgentConfig{}
	if a.ModelIsOperatorOwned() {
		t.Error("default should not be operator-owned")
	}
	a.ModelOwner = FieldOwnerOperator
	if !a.ModelIsOperatorOwned() {
		t.Error("operator-owned model should report true")
	}
	a.ModelOwner = FieldOwnerPack
	if a.ModelIsOperatorOwned() {
		t.Error("pack-owned model should not report operator-owned")
	}
}

func TestAgentConfig_BackendIsOperatorOwned(t *testing.T) {
	a := AgentConfig{}
	if a.BackendIsOperatorOwned() {
		t.Error("default should not be operator-owned")
	}
	a.BackendOwner = FieldOwnerOperator
	if !a.BackendIsOperatorOwned() {
		t.Error("operator-owned backend should report true")
	}
}

// ---------------------------------------------------------------------------
// config.go — BaseAgentName (Config method)
// ---------------------------------------------------------------------------

func TestConfig_BaseAgentName(t *testing.T) {
	cfg := &Config{
		Agents: map[string]AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
		},
	}
	if got := cfg.BaseAgentName("scanner"); got != "scanner" {
		t.Errorf("ordinary agent: got %q", got)
	}
	if got := cfg.BaseAgentName("scanner-2"); got != "scanner" {
		t.Errorf("replica: got %q want scanner", got)
	}
	if got := cfg.BaseAgentName("unknown"); got != "unknown" {
		t.Errorf("unknown: got %q want unknown", got)
	}

	// Nil config returns name as-is.
	var nilCfg *Config
	if got := nilCfg.BaseAgentName("x"); got != "x" {
		t.Errorf("nil config: got %q", got)
	}
}

// ---------------------------------------------------------------------------
// config.go — IsContributeRequireExplicitAccept
// ---------------------------------------------------------------------------

func TestIsContributeRequireExplicitAccept(t *testing.T) {
	h := HubConfig{}
	if h.IsContributeRequireExplicitAccept() {
		t.Error("nil pointer should resolve to false")
	}

	f := false
	h.ContributeRequireExplicitAccept = &f
	if h.IsContributeRequireExplicitAccept() {
		t.Error("explicit false should resolve to false")
	}

	tr := true
	h.ContributeRequireExplicitAccept = &tr
	if !h.IsContributeRequireExplicitAccept() {
		t.Error("explicit true should resolve to true")
	}
}

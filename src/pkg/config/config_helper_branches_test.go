package config

import (
	"os"
	"path/filepath"
	"testing"
)

// ---------------------------------------------------------------------------
// validSnapshotFrameAncestorHost (config.go): the CSP frame-ancestors host
// validator. The IP-literal and rejection branches were uncovered.
// ---------------------------------------------------------------------------

func TestValidSnapshotFrameAncestorHostBranches(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", false},                 // empty host is never a valid ancestor
		{"192.168.1.10", true},      // IPv4 literal accepted via netip
		{"::1", true},               // IPv6 literal accepted via netip
		{"example.com", true},       // plain DNS name
		{"grafana.internal.", true}, // trailing-dot FQDN allowed by pattern
		{"bad_host", false},         // underscore is not hostname-safe
		{"-leading.example", false}, // label cannot start with a hyphen
	}
	for _, tc := range cases {
		if got := validSnapshotFrameAncestorHost(tc.host); got != tc.want {
			t.Errorf("validSnapshotFrameAncestorHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SandboxEnvAllowlist (config.go): the global-fallback branch and the
// copy-on-return contract were uncovered — only the per-agent override branch
// had a test.
// ---------------------------------------------------------------------------

func TestSandboxEnvAllowlistFallsBackToGlobalAndCopies(t *testing.T) {
	global := AgentSandboxConfig{EnvAllowlist: []string{"PATH", "HOME"}}

	var nilAgent *AgentConfig
	got := nilAgent.SandboxEnvAllowlist(global)
	if len(got) != 2 || got[0] != "PATH" || got[1] != "HOME" {
		t.Fatalf("nil agent allowlist = %v, want the global list", got)
	}
	// Mutating the returned slice must not reach the global config: the
	// method promises a copy, and pkg/sandbox filters the returned slice.
	got[0] = "MUTATED"
	if global.EnvAllowlist[0] != "PATH" {
		t.Fatal("SandboxEnvAllowlist returned the global backing array, not a copy")
	}

	empty := &AgentConfig{Sandbox: &AgentSandboxOverride{}}
	if got := empty.SandboxEnvAllowlist(global); len(got) != 2 {
		t.Fatalf("empty per-agent allowlist should fall back to global, got %v", got)
	}

	agent := &AgentConfig{Sandbox: &AgentSandboxOverride{EnvAllowlist: []string{"TERM"}}}
	perAgent := agent.SandboxEnvAllowlist(global)
	if len(perAgent) != 1 || perAgent[0] != "TERM" {
		t.Fatalf("per-agent allowlist = %v, want [TERM]", perAgent)
	}
	perAgent[0] = "MUTATED"
	if agent.Sandbox.EnvAllowlist[0] != "TERM" {
		t.Fatal("SandboxEnvAllowlist returned the per-agent backing array, not a copy")
	}
}

// ---------------------------------------------------------------------------
// OAuthBaseURL / OAuthAPIURL (config.go): the override branches are the test
// seam pkg/dashboard relies on; only the default branches were covered here.
// ---------------------------------------------------------------------------

func TestOAuthURLOverridesWinOverDefaults(t *testing.T) {
	gh := GitHubConfig{
		OAuthBaseURLOverride: "http://127.0.0.1:9999",
		OAuthAPIURLOverride:  "http://127.0.0.1:9998",
	}
	if got := gh.OAuthBaseURL(); got != "http://127.0.0.1:9999" {
		t.Errorf("OAuthBaseURL() = %q, want the override", got)
	}
	if got := gh.OAuthAPIURL(); got != "http://127.0.0.1:9998" {
		t.Errorf("OAuthAPIURL() = %q, want the override", got)
	}
}

// ---------------------------------------------------------------------------
// replicaAgentName / normalizeReplicaCount (config.go): the replica-naming
// convention the dashboard, governor, and saved config all depend on.
// ---------------------------------------------------------------------------

func TestReplicaAgentNameAndNormalizeCount(t *testing.T) {
	if got := replicaAgentName("scanner", 1); got != "scanner" {
		t.Errorf(`replicaAgentName("scanner", 1) = %q, want the bare base name`, got)
	}
	if got := replicaAgentName("scanner", 0); got != "scanner" {
		t.Errorf(`replicaAgentName("scanner", 0) = %q, want the bare base name`, got)
	}
	if got := replicaAgentName("scanner", 2); got != "scanner-2" {
		t.Errorf(`replicaAgentName("scanner", 2) = %q, want "scanner-2"`, got)
	}

	if got := normalizeReplicaCount(0); got != 1 {
		t.Errorf("normalizeReplicaCount(0) = %d, want 1 (unset means one instance)", got)
	}
	if got := normalizeReplicaCount(3); got != 3 {
		t.Errorf("normalizeReplicaCount(3) = %d, want 3", got)
	}
}

// ---------------------------------------------------------------------------
// (*Config).HasAnyCadence (expected_active.go): the nil-receiver branch. A
// missing config must read as "cannot tell" (true), never as "unscheduled" —
// the dashboard turns false into an operator-facing warning.
// ---------------------------------------------------------------------------

func TestHasAnyCadenceNilConfigMeansCannotTell(t *testing.T) {
	var c *Config
	if !c.HasAnyCadence("scanner") {
		t.Fatal("nil config HasAnyCadence = false; a missing config must not accuse agents of being unscheduled")
	}
}

func TestHasAnyCadenceResolvesReplicaBase(t *testing.T) {
	c := &Config{
		Agents: map[string]AgentConfig{
			"scanner":   {},
			"scanner-2": {ReplicaOf: "scanner"},
		},
		Governor: GovernorConfig{Modes: map[string]ModeConfig{
			"busy": {Cadences: map[string]Cadence{"scanner": "15m"}},
		}},
	}
	if !c.HasAnyCadence("scanner-2") {
		t.Fatal("replica scanner-2 should inherit its base's cadence entry")
	}
	if c.HasAnyCadence("stranger") {
		t.Fatal("an agent named in no mode must report no cadence")
	}
}

// ---------------------------------------------------------------------------
// Layer.String (layers.go): every name is part of the /api/config/provenance
// contract; only a subset of the switch arms was exercised.
// ---------------------------------------------------------------------------

func TestLayerStringNamesAreStable(t *testing.T) {
	cases := []struct {
		layer Layer
		want  string
	}{
		{LayerSeed, "configmap-seed"},
		{LayerDashboardOverlay, "dashboard-overlay"},
		{LayerAgentOverlay, "agent-overlay"},
		{LayerConfigEnv, "config-env"},
		{LayerUnset, "unset"},
		{Layer(99), "unset"},
	}
	for _, tc := range cases {
		if got := tc.layer.String(); got != tc.want {
			t.Errorf("Layer(%d).String() = %q, want %q", tc.layer, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// SeedWinsGitHubApp (layers.go): ratchet C. The nil guards and the
// "seed real, overlay placeholder" adoption branch were uncovered.
// ---------------------------------------------------------------------------

func TestSeedWinsGitHubAppBranches(t *testing.T) {
	real := GitHubConfig{AppID: 42, InstallationID: 7, KeyFile: "/data/gh-app-key.pem"}
	placeholder := GitHubConfig{} // absent identifiers
	marked := GitHubConfig{AppID: 42, InstallationID: 7, KeyFile: "/etc/keys/PLACEHOLDER.pem"}

	if SeedWinsGitHubApp(nil, &Config{GitHub: placeholder}) {
		t.Error("nil seed must never win")
	}
	if SeedWinsGitHubApp(&Config{GitHub: real}, nil) {
		t.Error("nil overlay must never trigger the ratchet")
	}
	if !SeedWinsGitHubApp(&Config{GitHub: real}, &Config{GitHub: placeholder}) {
		t.Error("real seed vs placeholder overlay: the seed must win")
	}
	if !SeedWinsGitHubApp(&Config{GitHub: real}, &Config{GitHub: marked}) {
		t.Error("a PLACEHOLDER-marked key file must count as a placeholder overlay")
	}
	if SeedWinsGitHubApp(&Config{GitHub: placeholder}, &Config{GitHub: real}) {
		t.Error("placeholder seed must never displace a real overlay App")
	}
	if SeedWinsGitHubApp(&Config{GitHub: real}, &Config{GitHub: real}) {
		t.Error("two real Apps: overlay precedence stands, seed must not win")
	}
}

// ---------------------------------------------------------------------------
// saveDashboardOverlay (config.go): only the unwritable-tmp failure branch was
// covered. The non-Kubernetes no-op and the successful atomic write (temp file
// + rename, 0600) are the durability contract #3961/#5331 established.
// ---------------------------------------------------------------------------

func TestSaveDashboardOverlayIsNoOpOutsideKubernetes(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	restore := SetSATokenFileForTest(filepath.Join(t.TempDir(), "no-such-sa-token"))
	defer restore()

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(t.TempDir(), "hive.yaml.dashboard")
	defer func() { DashboardOverlayFile = origOverlay }()

	c := &Config{Project: ProjectConfig{Org: "hivecommons"}}
	if err := c.saveDashboardOverlay(); err != nil {
		t.Fatalf("saveDashboardOverlay outside Kubernetes = %v, want nil no-op", err)
	}
	if _, err := os.Stat(DashboardOverlayFile); !os.IsNotExist(err) {
		t.Fatal("non-Kubernetes save must not write an overlay file")
	}
}

func TestSaveDashboardOverlaySuccessWritesAtomically(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	dir := t.TempDir()
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	defer func() { DashboardOverlayFile = origOverlay }()

	c := &Config{Project: ProjectConfig{Org: "hivecommons"}}
	if err := c.saveDashboardOverlay(); err != nil {
		t.Fatalf("saveDashboardOverlay() = %v, want success", err)
	}

	info, err := os.Stat(DashboardOverlayFile)
	if err != nil {
		t.Fatalf("overlay file missing after successful save: %v", err)
	}
	// 0600, not 0644: a dashboard-minted auth token is persisted verbatim, so
	// the overlay is not reliably secret-free (#5331).
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("overlay file mode = %o, want 0600", got)
	}
	if _, err := os.Stat(DashboardOverlayFile + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file left behind after rename — the write was not atomic")
	}
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil || len(data) == 0 {
		t.Fatalf("overlay unreadable or empty after save: err=%v len=%d", err, len(data))
	}
}

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression tests for #7503: `converse: true` set in the per-agent overlay
// file (/data/agent-configs/<name>.yaml) was written to the capability file as
// empty. LoadWithDashboardOverlay replaced the whole agent entry with the
// dashboard overlay's copy, and that copy was silent on converse — nil, not
// false — so the one capability the operator had deliberately granted was
// dropped on the floor. Converse is a POINTER precisely so that "unset" and
// "explicitly false" stay distinguishable across the dashboard overlay; a layer
// that says nothing must not revoke what a lower layer set.

func writeConverseFixture(t *testing.T, dashboardConverse string) (seedPath string) {
	t.Helper()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedPath = filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
data:
  agents_dir: ` + agentsDir + `
agents:
  reviewer:
    backend: claude
    mode: ADVISORY
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	// The per-agent overlay is where the operator granted converse.
	reviewer := `
backend: claude
mode: ADVISORY
converse: true
`
	if err := os.WriteFile(filepath.Join(agentsDir, "reviewer.yaml"), []byte(reviewer), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  reviewer:
    backend: claude
    mode: ADVISORY
` + dashboardConverse
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	return seedPath
}

func TestLoadWithDashboardOverlay_SilentOverlayKeepsAgentOverlayConverse(t *testing.T) {
	seedPath := writeConverseFixture(t, "")

	// Precondition: plain Load honours the per-agent file.
	raw, err := Load(seedPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c := raw.Agents["reviewer"].Converse; c == nil || !*c {
		t.Fatalf("precondition: Load should carry converse: true from the per-agent overlay, got %v", c)
	}

	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	rv := merged.Agents["reviewer"]
	if rv.Converse == nil || !*rv.Converse {
		t.Errorf("converse: true from the per-agent overlay was dropped by a dashboard overlay that never set it (#7503): got %v", rv.Converse)
	}
	if rv.Mode != "ADVISORY" {
		t.Errorf("mode = %q, want ADVISORY", rv.Mode)
	}
}

// An EXPLICIT `converse: false` in the dashboard overlay is a revocation and
// must still win — the fix restores field precedence, it does not make the
// per-agent file unconditionally authoritative.
func TestLoadWithDashboardOverlay_ExplicitFalseStillRevokesConverse(t *testing.T) {
	seedPath := writeConverseFixture(t, "    converse: false\n")

	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	rv := merged.Agents["reviewer"]
	if rv.Converse == nil || *rv.Converse {
		t.Errorf("explicit converse: false in the dashboard overlay must win, got %v", rv.Converse)
	}
}

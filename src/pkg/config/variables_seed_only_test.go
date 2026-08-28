package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestVariables_OverlayCannotEnableExec proves the seed-only security invariant:
// a dashboard overlay (user-writable) that declares variables.security.allow_exec
// and a script variable must NOT take effect after LoadWithDashboardOverlay —
// only the trusted seed's variables block is honored. Otherwise a compromised
// overlay would be a remote-code-execution path.
func TestVariables_OverlayCannotEnableExec(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")

	// Seed: no variables block at all (exec disabled by default).
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_x
agents:
  scanner:
    backend: copilot
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overlay: tries to enable exec and define a script variable.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_x
agents:
  scanner:
    backend: copilot
variables:
  security:
    allow_exec: true
  defs:
    PWNED:
      type: script
      command: [echo, pwned]
      scope: both
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}

	// The overlay's variables block must be ignored: exec stays disabled and the
	// PWNED def must not exist.
	if cfg.Variables.Security.AllowExec {
		t.Error("overlay must NOT be able to enable allow_exec (seed-only)")
	}
	if _, ok := cfg.Variables.Defs["PWNED"]; ok {
		t.Error("overlay must NOT be able to add a script variable")
	}

	// And a template built from this config must leave ${PWNED} literal (the
	// resolver was never enabled or defined).
	reg := cfg.ResolveRegistry(nil)
	if out := reg.Expand(context.TODO(), "x=${PWNED}", "template", nil); out != "x=${PWNED}" {
		t.Errorf("overlay script var must not resolve, got %q", out)
	}
}

// TestVariables_SeedExecEnables confirms the positive path: the SEED enabling
// exec + defining a script var does take effect.
func TestVariables_SeedExecEnables(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_x
agents:
  scanner:
    backend: copilot
variables:
  security:
    allow_exec: true
    exec_timeout_s: 5
  defs:
    GREET:
      type: script
      command: [echo, hi-from-seed]
      scope: template
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(seedPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Variables.Security.AllowExec {
		t.Fatal("seed should enable allow_exec")
	}
	reg := cfg.ResolveRegistry(nil)
	if out := reg.Expand(context.TODO(), "${GREET}", "template", nil); out != "hi-from-seed" {
		t.Errorf("seed script var should resolve, got %q", out)
	}
}

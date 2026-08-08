package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadWithDashboardOverlay_OverlayWinsOnReload is the regression test for
// the runtime revert bug: a ConfigMap remount rewrites the seed back to its
// provision-time values and fires the config watcher; the reload must re-apply
// the dashboard overlay so runtime-reconciled agent fields (kick_template,
// mode, model set by ApplyPack when a hive's ACMM level was raised) survive.
// Before the fix, the watcher loaded the raw seed and silently reverted, e.g.
// scanner from scanner-holdgated.md (L5) back to scanner-advisory.md.
func TestLoadWithDashboardOverlay_OverlayWinsOnReload(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")

	// Seed = the stale ConfigMap: scanner is advisory (provision-time L2/L3).
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
    kick_template: scanner-advisory.md
    mode: ADVISORY
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overlay = the reconciled L5 state persisted by ApplyPack.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-opus-4-6
    kick_template: scanner-holdgated.md
    mode: ISSUES_AND_PRS
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	// Point the package overlay var at our temp file and fake K8s mode.
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	// Raw Load must return the stale seed value (this is what the watcher used
	// to do — and why it reverted).
	raw, err := Load(seedPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := raw.Agents["scanner"].KickTemplate; got != "scanner-advisory.md" {
		t.Fatalf("precondition: raw seed scanner should be advisory, got %q", got)
	}

	// LoadWithDashboardOverlay must apply the overlay so scanner is holdgated.
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	sc := merged.Agents["scanner"]
	if sc.KickTemplate != "scanner-holdgated.md" {
		t.Errorf("kick_template not restored from overlay: got %q want scanner-holdgated.md", sc.KickTemplate)
	}
	if sc.Mode != "ISSUES_AND_PRS" {
		t.Errorf("mode not restored: got %q want ISSUES_AND_PRS", sc.Mode)
	}
	if sc.Model != "claude-opus-4-6" {
		t.Errorf("model not restored: got %q want claude-opus-4-6", sc.Model)
	}
}

// TestLoadWithDashboardOverlay_NoOverlayFallsBackToSeed ensures that without an
// overlay (or outside K8s) the seed is used as-is — no behavior change.
func TestLoadWithDashboardOverlay_NoOverlayFallsBackToSeed(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    kick_template: scanner-advisory.md
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	DashboardOverlayFile = filepath.Join(dir, "does-not-exist.dashboard")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	cfg, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if got := cfg.Agents["scanner"].KickTemplate; got != "scanner-advisory.md" {
		t.Errorf("without overlay, seed should win: got %q", got)
	}
}

func TestLoadWithDashboardOverlay_OTelSurvivesShortOverlay(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
otel:
  enabled: true
  endpoint: https://otel.example.com/v1/traces
  service_name: hive-prod
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
	got := cfg.EffectiveOTel()
	if !got.Enabled || got.Endpoint != "https://otel.example.com/v1/traces" || got.ServiceName != "hive-prod" {
		t.Fatalf("overlay otel not adopted: %+v", got)
	}
}

func TestLoadWithDashboardOverlay_LegacyTracingMergesIntoPartialSeedOTel(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
otel:
  service_name: hive-seed
agents:
  scanner:
    backend: copilot
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
tracing:
  enabled: true
  endpoint: https://legacy.example.com/v1/traces
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
	got := cfg.EffectiveOTel()
	if !got.Enabled || got.Endpoint != "https://legacy.example.com/v1/traces" || got.ServiceName != "hive-seed" {
		t.Fatalf("legacy tracing overlay did not merge into partial otel: %+v", got)
	}
}

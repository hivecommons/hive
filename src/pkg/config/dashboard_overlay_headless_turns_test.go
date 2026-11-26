package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression tests for #19902: `headless_turns: true` could not be turned on
// for an existing Kubernetes spoke. LoadWithDashboardOverlay replaced each
// agent entry whole with the dashboard overlay's older copy, which was silent
// on the key, and the per-agent file carry (#18804) copied only paused and
// sandbox. So the key was dropped from both the per-agent file and the config
// file's agents.<name> block, and the next save rewrote the runtime config
// without it.

func writeHeadlessTurnsFixture(t *testing.T, seedHeadless, fileHeadless, overlayHeadless bool) (seedPath string) {
	t.Helper()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flag := func(on bool, indent string) string {
		if !on {
			return ""
		}
		return indent + "headless_turns: true\n"
	}
	seedPath = filepath.Join(dir, "hive.yaml.runtime")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
data:
  agents_dir: ` + agentsDir + `
agents:
  ci-maintainer:
    backend: claude
    model: claude-sonnet-4-6
    paused: true
` + flag(seedHeadless, "    ")
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if fileHeadless {
		agent := "backend: claude\nmodel: claude-sonnet-4-6\n" + flag(true, "")
		if err := os.WriteFile(filepath.Join(agentsDir, "ci-maintainer.yaml"), []byte(agent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  ci-maintainer:
    backend: claude
    model: claude-sonnet-4-6
    paused: true
` + flag(overlayHeadless, "    ")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	return seedPath
}

// assertHeadlessTurnsPersists checks the merged agent and the YAML a runtime
// rewrite would write for it.
func assertHeadlessTurnsPersists(t *testing.T, merged *Config) {
	t.Helper()
	a, ok := merged.Agents["ci-maintainer"]
	if !ok {
		t.Fatalf("agent ci-maintainer missing after load: %v", merged.Agents)
	}
	if !a.HeadlessTurnsEnabled() {
		t.Fatalf("headless_turns: true dropped by a dashboard overlay that never set it (#19902)")
	}
	if !a.Paused {
		t.Errorf("paused: true lost alongside headless_turns")
	}
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	if !strings.Contains(string(out), "headless_turns: true") {
		t.Errorf("runtime rewrite of the agent omits headless_turns:\n%s", out)
	}
}

func TestLoadWithDashboardOverlay_ConfigFileHeadlessTurnsSurvivesSilentOverlay(t *testing.T) {
	seedPath := writeHeadlessTurnsFixture(t, true, false, false)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	assertHeadlessTurnsPersists(t, merged)
}

func TestLoadWithDashboardOverlay_AgentFileHeadlessTurnsSurvivesSilentOverlay(t *testing.T) {
	seedPath := writeHeadlessTurnsFixture(t, false, true, false)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	assertHeadlessTurnsPersists(t, merged)
}

func TestLoadWithDashboardOverlay_OverlayHeadlessTurnsWins(t *testing.T) {
	seedPath := writeHeadlessTurnsFixture(t, false, false, true)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	assertHeadlessTurnsPersists(t, merged)
}

// With no layer opting in, the agent stays interactive.
func TestLoadWithDashboardOverlay_HeadlessTurnsOffByDefault(t *testing.T) {
	seedPath := writeHeadlessTurnsFixture(t, false, false, false)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if merged.Agents["ci-maintainer"].HeadlessTurnsEnabled() {
		t.Errorf("headless_turns turned on with no layer setting it")
	}
}

// The dashboard's off writes an explicit `headless_turns: false` to the
// overlay, but it cannot rewrite the read-only ConfigMap seed. That false
// must beat the seed's opt-in rather than read as unset.
func TestLoadWithDashboardOverlay_OverlayHeadlessTurnsFalseBeatsSeed(t *testing.T) {
	seedPath := writeHeadlessTurnsFixture(t, true, false, false)
	data, err := os.ReadFile(DashboardOverlayFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DashboardOverlayFile, append(data, []byte("    headless_turns: false\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if merged.Agents["ci-maintainer"].HeadlessTurnsEnabled() {
		t.Errorf("overlay headless_turns: false lost to the seed's true")
	}
}

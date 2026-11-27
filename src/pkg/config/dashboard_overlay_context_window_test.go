package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression tests for #19913: max_context_tokens and
// auto_compact_window_tokens set in /data/agent-configs/<agent>.yaml or in
// the config file's agents.<name> block were dropped on a Kubernetes spoke,
// because LoadWithDashboardOverlay replaced the agent entry whole with the
// dashboard overlay's older copy, which was silent on both keys, and the
// per-agent file carry copied only paused, sandbox and headless_turns.

// contextWindowFixture writes a seed, an optional agent file and an overlay.
// Each argument is the YAML lines for that layer's context keys ("" = none).
func contextWindowFixture(t *testing.T, seedKeys, fileKeys, overlayKeys string) (seedPath string) {
	t.Helper()
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	indent := func(keys, pad string) string {
		if keys == "" {
			return ""
		}
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(keys), "\n") {
			b.WriteString(pad + line + "\n")
		}
		return b.String()
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
  quality:
    backend: claude
    model: claude-sonnet-4-6
    paused: true
` + indent(seedKeys, "    ")
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if fileKeys != "" {
		agent := "backend: claude\nmodel: claude-sonnet-4-6\n" + indent(fileKeys, "")
		if err := os.WriteFile(filepath.Join(agentsDir, "quality.yaml"), []byte(agent), 0o644); err != nil {
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
  quality:
    backend: claude
    model: claude-sonnet-4-6
    paused: true
` + indent(overlayKeys, "    ")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	return seedPath
}

func loadContextWindowAgent(t *testing.T, seedPath string) AgentConfig {
	t.Helper()
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	a, ok := merged.Agents["quality"]
	if !ok {
		t.Fatalf("agent quality missing after load: %v", merged.Agents)
	}
	if !a.Paused {
		t.Errorf("paused: true lost alongside the context keys")
	}
	return a
}

// assertContextWindow checks the merged agent and the YAML a runtime rewrite
// would write for it.
func assertContextWindow(t *testing.T, a AgentConfig, maxCtx, compact int) {
	t.Helper()
	if a.MaxContextTokens != maxCtx {
		t.Errorf("max_context_tokens = %d, want %d (#19913)", a.MaxContextTokens, maxCtx)
	}
	if a.AutoCompactWindowTokens != compact {
		t.Errorf("auto_compact_window_tokens = %d, want %d (#19913)", a.AutoCompactWindowTokens, compact)
	}
	out, err := yaml.Marshal(a)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	for key, want := range map[string]int{"max_context_tokens": maxCtx, "auto_compact_window_tokens": compact} {
		if want == 0 {
			continue
		}
		if !strings.Contains(string(out), key+": ") {
			t.Errorf("runtime rewrite of the agent omits %s:\n%s", key, out)
		}
	}
}

const contextKeys = "max_context_tokens: 196608\nauto_compact_window_tokens: 160000"

func TestLoadWithDashboardOverlay_AgentFileContextWindowSurvivesSilentOverlay(t *testing.T) {
	a := loadContextWindowAgent(t, contextWindowFixture(t, "", contextKeys, ""))
	assertContextWindow(t, a, 196608, 160000)
}

func TestLoadWithDashboardOverlay_ConfigFileContextWindowSurvivesSilentOverlay(t *testing.T) {
	a := loadContextWindowAgent(t, contextWindowFixture(t, contextKeys, "", ""))
	assertContextWindow(t, a, 196608, 160000)
}

func TestLoadWithDashboardOverlay_OverlayContextWindowBeatsSeed(t *testing.T) {
	a := loadContextWindowAgent(t, contextWindowFixture(t,
		contextKeys, "",
		"max_context_tokens: 131072\nauto_compact_window_tokens: 120000"))
	assertContextWindow(t, a, 131072, 120000)
}

// The per-agent file is the higher layer, so its non-zero value wins over
// the overlay's.
func TestLoadWithDashboardOverlay_AgentFileContextWindowBeatsOverlay(t *testing.T) {
	a := loadContextWindowAgent(t, contextWindowFixture(t,
		"", contextKeys,
		"max_context_tokens: 131072\nauto_compact_window_tokens: 120000"))
	assertContextWindow(t, a, 196608, 160000)
}

// No layer sets the keys: both stay unset so the window is discovered.
func TestLoadWithDashboardOverlay_ContextWindowUnsetByDefault(t *testing.T) {
	a := loadContextWindowAgent(t, contextWindowFixture(t, "", "", ""))
	assertContextWindow(t, a, 0, 0)
}

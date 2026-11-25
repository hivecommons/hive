package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/config"
)

// opencode's TUI has no --model flag and prompts for permissions, so an
// unattended hive agent needs --auto, a private server per agent, and the model
// carried in inline config rather than argv.
func TestBackendLaunchCmd_OpencodeIsUnattendedStandaloneAndModelFree(t *testing.T) {
	for name, got := range map[string]string{
		"default":   backendLaunchCmdWithBobSession("opencode", "opencode/hy4-flash-free", "opencode", false, "", ""),
		"toolRules": toolRulesToLaunchCmdWithBobSession("opencode", "opencode/hy4-flash-free", "opencode", &config.ToolsConfig{}, false, "", ""),
	} {
		if got != "opencode --standalone --auto" {
			t.Errorf("%s launch = %q, want %q", name, got, "opencode --standalone --auto")
		}
		if strings.Contains(got, "--model") {
			t.Errorf("%s launch passes --model, which the opencode TUI does not accept: %q", name, got)
		}
	}
}

func TestAgentEnvPairs_OpencodeModelRidesInInlineConfig(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"dedupe": {Backend: "opencode", Model: "opencode/hy4-flash-free"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})
	m.mu.RLock()
	agent := m.agents["dedupe"]
	m.mu.RUnlock()

	p, ok := envPairsByKey(m, agent)["OPENCODE_CONFIG_CONTENT"]
	if !ok {
		t.Fatal("OPENCODE_CONFIG_CONTENT not exported for an opencode agent with a model")
	}
	var cfg map[string]string
	if err := json.Unmarshal([]byte(p.Value), &cfg); err != nil || cfg["model"] != "opencode/hy4-flash-free" {
		t.Fatalf("OPENCODE_CONFIG_CONTENT = %q (err %v), want model opencode/hy4-flash-free", p.Value, err)
	}
}

func TestAgentEnvPairs_NoOpencodeConfigForOtherBackends(t *testing.T) {
	m, agent := claudeAgentFixture(t, "")
	if _, ok := envPairsByKey(m, agent)["OPENCODE_CONFIG_CONTENT"]; ok {
		t.Fatal("OPENCODE_CONFIG_CONTENT leaked into a claude agent's env")
	}
}

// The bootstrap prompt must reach opencode through readiness-gated kick
// delivery: embedded on argv, opencode's TUI would read it as a directory.
func TestBackendDefersStartupKick_Opencode(t *testing.T) {
	if !backendDefersStartupKick("opencode") {
		t.Fatal("opencode must defer its startup kick until the TUI is ready")
	}
}

// Captured from opencode 2.3.2 in a hive pane (hanthor, 2026-11-25).
const opencodeIdlePane = ` ┃  Ask anything… "Fix broken tests"
 ┃  Build · hy4-flash-free OpenCode
 ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
 /data/agents/dedupe                       shift+tab agents  ctrl+p commands`

const opencodeBusyPane = ` ⠦ Thinking…
 ┃  Build · hy4-flash-free OpenCode
 ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
 ⬝⬝⬝⬝⬝⬝⬝⬝ esc interrupt     shift+tab agents  ctrl+p commands`

func TestOpencodePane_ReadyWhenIdleBusyWhenTurnRuns(t *testing.T) {
	if !panesignal.ShowsInputPrompt(opencodeIdlePane) || panesignal.ShowsAgentWorking(opencodeIdlePane) {
		t.Error("idle opencode pane must read as a ready, not-working prompt")
	}
	// A live opencode pane must count as a running CLI, or every kick restarts it.
	if !panesignal.HasCLIMarker(opencodeIdlePane) || !panesignal.HasCLIMarker(opencodeBusyPane) {
		t.Error("opencode panes must carry a CLI marker so a healthy TUI is not treated as a crashed shell")
	}
	if !panesignal.ShowsAgentWorking(opencodeBusyPane) {
		t.Error("opencode pane showing 'esc interrupt' must read as working, so a kick is not typed into a live turn")
	}
}

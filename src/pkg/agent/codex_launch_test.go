package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// codexLaunchCmd builds the codex launch command the way Start does for an
// agent with no ToolsConfig, mirroring agyLaunchCmd: normalize the configured
// model for the backend, then hand it to backendLaunchCmd.
func codexLaunchCmd(t *testing.T, model, effort string) string {
	t.Helper()
	isInference := IsInferenceBackend(codexBackend)
	return backendLaunchCmd("codex", normalizeModelNameForBackend(model, codexBackend, isInference), codexBackend, isInference, effort)
}

// TestCodexLaunchCommandLine_Model asserts the configured model reaches the
// codex command line. Before the codexBackend case existed, codex fell to the
// bare-binary default and the dashboard's model selection was silently
// dropped on the tmux launch path.
func TestCodexLaunchCommandLine_Model(t *testing.T) {
	if cmd := codexLaunchCmd(t, "gpt-6-astra", ""); !strings.Contains(cmd, "--model gpt-6-astra") {
		t.Errorf("codex launched without '--model gpt-6-astra'; cmd: %q", cmd)
	}
	if cmd := codexLaunchCmd(t, "", ""); strings.Contains(cmd, "--model") {
		t.Errorf("codex with no configured model must not be passed --model; cmd: %q", cmd)
	}
}

// TestCodexLaunchCommandLine_ReasoningEffort pins the reasoning_effort flag
// contract: the exact `-c model_reasoning_effort="<v>"` spelling the scripted
// launch paths (bin/agent-launch.sh, contributor relay) already use, present
// only when an effort is configured, and valid with or without --model.
func TestCodexLaunchCommandLine_ReasoningEffort(t *testing.T) {
	if cmd := codexLaunchCmd(t, "gpt-6-astra", "xhigh"); !strings.Contains(cmd, `--model gpt-6-astra -c model_reasoning_effort="xhigh"`) {
		t.Errorf("configured effort must ride after --model as -c model_reasoning_effort; cmd: %q", cmd)
	}
	if cmd := codexLaunchCmd(t, "", "high"); !strings.Contains(cmd, `-c model_reasoning_effort="high"`) {
		t.Errorf("effort with no model must still be passed (codex runs its default model at that effort); cmd: %q", cmd)
	}
	if cmd := codexLaunchCmd(t, "gpt-6-astra", ""); strings.Contains(cmd, "model_reasoning_effort") {
		t.Errorf("no configured effort must mean no -c key (codex's own default); cmd: %q", cmd)
	}
}

// TestCodexToolRulesLaunchCmd_MatchesBackendLaunchCmd pins the ToolsConfig
// path to the same model+effort contract: codex has no deny-tool flag (same
// shape as bob), so tools change nothing about the command line.
func TestCodexToolRulesLaunchCmd_MatchesBackendLaunchCmd(t *testing.T) {
	tools := &config.ToolsConfig{}
	got := toolRulesToLaunchCmd("codex", "gpt-6-astra", codexBackend, tools, false, "xhigh")
	want := backendLaunchCmd("codex", "gpt-6-astra", codexBackend, false, "xhigh")
	if got != want {
		t.Errorf("toolRulesToLaunchCmd(codex) = %q, want the backendLaunchCmd contract %q", got, want)
	}
}

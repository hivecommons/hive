package agent

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Launch-command tests for the per-agent reasoning effort on the claude
// backend (hivecommons/hive#8377): `--effort <v>` rides on the claude command
// line only when an effort is configured, only for a value Claude Code
// accepts, and only for claude — every other backend without an effort
// control ignores the field rather than growing a flag it cannot parse.

// claudeLaunchCmd builds the claude launch command the way Start does for an
// agent with no ToolsConfig, mirroring agyLaunchCmd / codexLaunchCmd.
func claudeLaunchCmd(t *testing.T, model, effort string, isInference bool) string {
	t.Helper()
	return backendLaunchCmd("claude", normalizeModelNameForBackend(model, config.ClaudeBackend, isInference), config.ClaudeBackend, isInference, effort)
}

func TestClaudeEffortFlag(t *testing.T) {
	cases := []struct {
		effort, want string
	}{
		{"", ""},
		{config.ClaudeEffortLow, " --effort low"},
		{config.ClaudeEffortMedium, " --effort medium"},
		{config.ClaudeEffortHigh, " --effort high"},
		{config.ClaudeEffortXHigh, " --effort xhigh"},
		{config.ClaudeEffortMax, " --effort max"},
		{"minimal", ""}, // codex-only value: dropped, never passed
		{"ultra", ""},   // muse-only value: dropped
		{"HIGH", ""},    // no case folding; the CLI's set is lowercase
	}
	for _, c := range cases {
		if got := claudeEffortFlag(c.effort); got != c.want {
			t.Errorf("claudeEffortFlag(%q) = %q, want %q", c.effort, got, c.want)
		}
	}
}

// TestClaudeLaunchCommandLine_ReasoningEffort pins the flag contract: present
// as `--effort <v>` after the permission flag when configured, absent when
// unset (Claude Code keeps its own default), and the security deny flags are
// untouched either way.
func TestClaudeLaunchCommandLine_ReasoningEffort(t *testing.T) {
	cmd := claudeLaunchCmd(t, "claude-fable-5", config.ClaudeEffortXHigh, false)
	if !strings.HasPrefix(cmd, "claude --model claude-fable-5 --dangerously-skip-permissions --effort xhigh") {
		t.Errorf("configured effort must ride after the permission flag as --effort; cmd: %q", cmd)
	}
	if strings.Count(cmd, "--effort") != 1 {
		t.Errorf("--effort must appear exactly once; cmd: %q", cmd)
	}
	if !strings.Contains(cmd, claudeGitHubWriteDenyFlags) {
		t.Errorf("effort must not displace the GitHub MCP write-deny flags; cmd: %q", cmd)
	}

	if cmd := claudeLaunchCmd(t, "claude-fable-5", "", false); strings.Contains(cmd, "--effort") {
		t.Errorf("no configured effort must mean no --effort (Claude Code's own default); cmd: %q", cmd)
	}
	if cmd := claudeLaunchCmd(t, "claude-fable-5", "minimal", false); strings.Contains(cmd, "--effort") {
		t.Errorf("a value claude does not accept must be dropped, not passed; cmd: %q", cmd)
	}
}

// TestClaudeLaunchCommandLine_InferenceKeepsEffort pins the inference route
// (`--bare --settings`): the same claude binary, so the same --effort.
func TestClaudeLaunchCommandLine_InferenceKeepsEffort(t *testing.T) {
	cmd := claudeLaunchCmd(t, "m", config.ClaudeEffortHigh, true)
	if !strings.Contains(cmd, " --bare --settings "+claudeInferenceSettingsPath+" --effort high") {
		t.Errorf("inference claude launch must carry --effort after --bare --settings; cmd: %q", cmd)
	}
	if cmd := claudeLaunchCmd(t, "m", "", true); strings.Contains(cmd, "--effort") {
		t.Errorf("inference claude launch with no effort must not carry --effort; cmd: %q", cmd)
	}
}

// TestClaudeToolRulesLaunchCmd_CarriesEffort pins the ToolsConfig path to the
// same contract: --effort before the per-tool denies, and absent when unset.
func TestClaudeToolRulesLaunchCmd_CarriesEffort(t *testing.T) {
	tools := &config.ToolsConfig{Rules: []config.ToolRule{{Pattern: "Bash(rm *)", Action: "deny"}}}
	cmd := toolRulesToLaunchCmd("claude", "claude-fable-5", config.ClaudeBackend, tools, false, config.ClaudeEffortMax)
	if !strings.HasPrefix(cmd, "claude --model claude-fable-5 --dangerously-skip-permissions --effort max") {
		t.Errorf("tools path must carry --effort after the permission flag; cmd: %q", cmd)
	}
	if !strings.Contains(cmd, "--disallowed-tools 'Bash(rm *)'") {
		t.Errorf("tools path must keep its deny rules; cmd: %q", cmd)
	}
	if cmd := toolRulesToLaunchCmd("claude", "claude-fable-5", config.ClaudeBackend, tools, false, ""); strings.Contains(cmd, "--effort") {
		t.Errorf("tools path with no effort must not carry --effort; cmd: %q", cmd)
	}
}

// TestOtherBackendsIgnoreClaudeEffort pins that a configured effort reaches
// only the backends with an effort control: copilot, gemini, goose and bob
// get no --effort even when a claude-valid value is stored (a backend switch
// after an effort was set must not break the new backend's launch).
func TestOtherBackendsIgnoreClaudeEffort(t *testing.T) {
	for _, backend := range []string{"copilot", "gemini", "goose", bobBackend, "unknown-cli"} {
		cmd := backendLaunchCmd(backend, "m", backend, false, config.ClaudeEffortHigh)
		if strings.Contains(cmd, "--effort") {
			t.Errorf("%s must ignore a configured effort; cmd: %q", backend, cmd)
		}
		toolsCmd := toolRulesToLaunchCmd(backend, "m", backend, &config.ToolsConfig{}, false, config.ClaudeEffortHigh)
		if strings.Contains(toolsCmd, "--effort") {
			t.Errorf("%s (tools path) must ignore a configured effort; cmd: %q", backend, toolsCmd)
		}
	}
}

// TestResolveReasoningEffort_Claude pins the attribution-trail rule: the
// configured value when claude accepts it (that is what --effort carried),
// "" when unset or when the value would have been dropped at launch.
func TestResolveReasoningEffort_Claude(t *testing.T) {
	cases := []struct {
		configured, want string
	}{
		{"", ""},
		{config.ClaudeEffortMedium, "medium"},
		{config.ClaudeEffortMax, "max"},
		{"minimal", ""},
	}
	for _, c := range cases {
		if got := ResolveReasoningEffort(config.ClaudeBackend, "claude-fable-5", c.configured); got != c.want {
			t.Errorf("ResolveReasoningEffort(claude, %q) = %q, want %q", c.configured, got, c.want)
		}
		// Unlike agy, claude's effort does not depend on a model being set.
		if got := ResolveReasoningEffort(config.ClaudeBackend, "", c.configured); got != c.want {
			t.Errorf("ResolveReasoningEffort(claude, no model, %q) = %q, want %q", c.configured, got, c.want)
		}
	}
	if got := ResolveReasoningEffort("copilot", "m", config.ClaudeEffortHigh); got != "" {
		t.Errorf("copilot must not report an effort it was never launched with; got %q", got)
	}
}

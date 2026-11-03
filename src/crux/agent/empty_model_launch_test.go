package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

// A production reviewer agent with agents[].model: "" launched as
//
//	claude --model --dangerously-skip-permissions --bare …
//
// so `--model` consumed the bypass flag. These tests pin, for every backend
// and both builder families, that an empty or whitespace model never renders
// a `--model` at all — and never a `--model` whose value is the next flag.

var emptyModelLaunchBackends = []string{
	"claude", "copilot", "gemini", "pi", "goose", codexBackend, "agy", "omp", bobBackend, "mystery",
}

func assertNoBareModelFlag(t *testing.T, label, cmd string) {
	t.Helper()
	if strings.Contains(cmd, "--model") {
		t.Errorf("%s: empty model must omit --model entirely; cmd=%q", label, cmd)
	}
	if _, dropped := config.DropDanglingModelFlag(cmd); dropped {
		t.Errorf("%s: rendered a dangling --model; cmd=%q", label, cmd)
	}
}

func TestLaunchBuilders_EmptyModelOmitsModelFlag(t *testing.T) {
	t.Setenv(agyLaunchModeEnv, agyLaunchModeInteractive)
	tools := &config.ToolsConfig{}
	for _, backend := range emptyModelLaunchBackends {
		for _, model := range []string{"", " ", "\t"} {
			for _, isInference := range []bool{false, true} {
				label := backend + "/" + strings.ReplaceAll(model, "\t", "TAB")
				assertNoBareModelFlag(t, "backendLaunchCmd "+label, backendLaunchCmd("bin", model, backend, isInference, "high"))
				assertNoBareModelFlag(t, "toolRulesToLaunchCmd "+label, toolRulesToLaunchCmd("bin", model, backend, tools, isInference, "high"))
			}
		}
	}
	assertNoBareModelFlag(t, "agyHeadlessTurnShellCommand", agyHeadlessTurnShellCommand("hive", "agy", " ", "", "/p", true))
}

// TestLaunchBuilders_ClaudeEmptyModelKeepsBypassFlagFirst is the exact live
// failure: with no model the bypass flag must directly follow the binary.
func TestLaunchBuilders_ClaudeEmptyModelKeepsBypassFlagFirst(t *testing.T) {
	for _, isInference := range []bool{false, true} {
		cmd := backendLaunchCmd("claude", "", "claude", isInference, "")
		if !strings.HasPrefix(cmd, "claude --dangerously-skip-permissions") {
			t.Errorf("isInference=%v: %q", isInference, cmd)
		}
		cmd = toolRulesToLaunchCmd("claude", "", "claude", &config.ToolsConfig{}, isInference, "")
		if !strings.HasPrefix(cmd, "claude --dangerously-skip-permissions") {
			t.Errorf("tools isInference=%v: %q", isInference, cmd)
		}
	}
	// A configured model still renders exactly as before.
	if cmd := backendLaunchCmd("claude", "claude-opus-4-8", "claude", false, ""); !strings.HasPrefix(cmd, "claude --model claude-opus-4-8 --dangerously-skip-permissions") {
		t.Errorf("configured model changed shape: %q", cmd)
	}
	if cmd := backendLaunchCmd("copilot", "auto", "copilot", false, ""); !strings.HasPrefix(cmd, "copilot --model auto --no-auto-update --allow-all") {
		t.Errorf("copilot auto sentinel must still pass through: %q", cmd)
	}
}

func TestDropDanglingModelFlag_LaunchTimeWarnsAndCleans(t *testing.T) {
	var buf bytes.Buffer
	m := &Manager{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	agent := &AgentProcess{Name: "reviewer"}

	bad := "/usr/local/bin/claude --model --dangerously-skip-permissions --bare --settings /tmp/.claude-inference-settings.json"
	got := m.dropDanglingModelFlag(agent, "claude", bad)
	if want := "/usr/local/bin/claude --dangerously-skip-permissions --bare --settings /tmp/.claude-inference-settings.json"; got != want {
		t.Errorf("cleaned = %q, want %q", got, want)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "bare --model") {
		t.Errorf("expected a WARN naming the bare --model; log=%s", buf.String())
	}

	buf.Reset()
	good := "claude --model claude-opus-4-8 --dangerously-skip-permissions"
	if got := m.dropDanglingModelFlag(agent, "claude", good); got != good || buf.Len() != 0 {
		t.Errorf("well-formed command must pass untouched and silently: got %q log=%s", got, buf.String())
	}
}

// TestResolveLaunchModel_InferenceDefaultFallback pins the fallback chain
// agent.model → inference default (governor.litellm.default_model / gateway
// default_model) → "" and that the WARN fires once per agent.
func TestResolveLaunchModel_InferenceDefaultFallback(t *testing.T) {
	var buf bytes.Buffer
	calls := 0
	m := &Manager{
		logger: slog.New(slog.NewTextHandler(&buf, nil)),
		project: crux.ProjectContext{InferenceDefaultModel: func(backend string) string {
			calls++
			if backend == "litellm" {
				return "claude-opus-4-8"
			}
			return ""
		}},
	}
	agent := &AgentProcess{Name: "reviewer"}

	if got := m.resolveLaunchModel(agent, "litellm", "", true); got != "claude-opus-4-8" {
		t.Fatalf("empty model on litellm = %q, want the configured default verbatim", got)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "claude-opus-4-8") {
		t.Errorf("fallback must WARN naming the model; log=%s", buf.String())
	}
	buf.Reset()
	if got := m.resolveLaunchModel(agent, "litellm", "  ", true); got != "claude-opus-4-8" || buf.Len() != 0 {
		t.Errorf("second fallback for the same agent must not WARN again: got %q log=%s", got, buf.String())
	}

	if got := m.resolveLaunchModel(agent, "litellm", " explicit-model ", true); got != "explicit-model" {
		t.Errorf("configured model must win: %q", got)
	}
	if got := m.resolveLaunchModel(agent, "vllm", "", true); got != "" {
		t.Errorf("no default for the backend → empty (flag omitted): %q", got)
	}
	before := calls
	if got := m.resolveLaunchModel(agent, "claude", "", false); got != "" || calls != before {
		t.Errorf("CLI backends never consult the inference default: got %q calls=%d", got, calls-before)
	}
	nilCtx := &Manager{logger: discardLogger()}
	if got := nilCtx.resolveLaunchModel(agent, "litellm", "", true); got != "" {
		t.Errorf("nil InferenceDefaultModel → no fallback: %q", got)
	}
}

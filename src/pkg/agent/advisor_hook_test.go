package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

func advisorTestManager() *Manager {
	return &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestAdvisorEnabledResolver(t *testing.T) {
	m := advisorTestManager()
	if m.advisorEnabledFor("scout") {
		t.Error("no resolver must mean advisor off")
	}
	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })
	if !m.advisorEnabledFor("scout") {
		t.Error("resolver must apply")
	}
	if m.advisorEnabledFor("other") {
		t.Error("resolver must be per-agent")
	}
	m.SetAdvisorEnabledResolver(nil)
	if m.advisorEnabledFor("scout") {
		t.Error("nil resolver must clear")
	}
}

func TestClaudeAdvisorSettingsShape(t *testing.T) {
	orig := advisorHookRunnerBinary
	advisorHookRunnerBinary = func() string { return "/usr/local/bin/hive" }
	defer func() { advisorHookRunnerBinary = orig }()

	raw := claudeAdvisorSettings()
	var settings struct {
		Hooks struct {
			Stop []struct {
				Hooks []struct {
					Type    string `json:"type"`
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings not JSON: %v (%s)", err, raw)
	}
	if len(settings.Hooks.Stop) != 1 || len(settings.Hooks.Stop[0].Hooks) != 1 {
		t.Fatalf("want exactly one Stop hook: %s", raw)
	}
	h := settings.Hooks.Stop[0].Hooks[0]
	if h.Type != "command" {
		t.Errorf("hook type = %q", h.Type)
	}
	if h.Command != "/usr/local/bin/hive "+advisor.HookSubcommand {
		t.Errorf("hook command = %q", h.Command)
	}
	if h.Timeout <= 0 {
		t.Errorf("hook timeout = %d", h.Timeout)
	}
}

func TestAdvisorLaunchFlag(t *testing.T) {
	orig := advisorHookRunnerBinary
	advisorHookRunnerBinary = func() string { return "hive" }
	defer func() { advisorHookRunnerBinary = orig }()

	m := advisorTestManager()
	agent := &AgentProcess{Name: "scout"}

	// Advisor off: no flag, no matter the backend.
	if got := m.advisorLaunchFlag(agent, config.AdvisorClaudeBackend, false); got != "" {
		t.Errorf("advisor off must project nothing: %q", got)
	}

	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })

	flag := m.advisorLaunchFlag(agent, config.AdvisorClaudeBackend, false)
	if !strings.HasPrefix(flag, " --settings '") {
		t.Fatalf("flag = %q", flag)
	}
	if !strings.Contains(flag, `"Stop"`) || !strings.Contains(flag, advisor.HookSubcommand) {
		t.Errorf("flag must carry the Stop hook: %q", flag)
	}

	// Unsupported backend: refused, agent launches without it.
	if got := m.advisorLaunchFlag(agent, "codex", false); got != "" {
		t.Errorf("unsupported backend must project nothing: %q", got)
	}
	// Inference-routed Claude: no interactive turn boundary to hook.
	if got := m.advisorLaunchFlag(agent, config.AdvisorClaudeBackend, true); got != "" {
		t.Errorf("inference agent must project nothing: %q", got)
	}
	// Other agents stay unadvised.
	if got := m.advisorLaunchFlag(&AgentProcess{Name: "other"}, config.AdvisorClaudeBackend, false); got != "" {
		t.Errorf("unadvised agent must project nothing: %q", got)
	}
}

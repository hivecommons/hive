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
	if got := m.advisorLaunchFlag(agent, "goose", false); got != "" {
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

func TestAdvisorCopilotHooksJSONShape(t *testing.T) {
	orig := advisorHookRunnerBinary
	advisorHookRunnerBinary = func() string { return "/usr/local/bin/hive" }
	defer func() { advisorHookRunnerBinary = orig }()

	var doc struct {
		Version int `json:"version"`
		Hooks   struct {
			AgentStop []struct {
				Type       string `json:"type"`
				Bash       string `json:"bash"`
				TimeoutSec int    `json:"timeoutSec"`
			} `json:"agentStop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(copilotAdvisorHooksJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Hooks.AgentStop) != 1 {
		t.Fatalf("want exactly one agentStop hook: %+v", doc)
	}
	h := doc.Hooks.AgentStop[0]
	want := "/usr/local/bin/hive " + advisor.HookSubcommand + " --format copilot"
	if h.Type != "command" || h.Bash != want || h.TimeoutSec <= 0 {
		t.Errorf("hook = %+v, want command %q", h, want)
	}
}

func TestAdvisorCodexHooksJSONShape(t *testing.T) {
	orig := advisorHookRunnerBinary
	advisorHookRunnerBinary = func() string { return "/usr/local/bin/hive" }
	defer func() { advisorHookRunnerBinary = orig }()

	var doc struct {
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
	if err := json.Unmarshal(codexAdvisorHooksJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Hooks.Stop) != 1 || len(doc.Hooks.Stop[0].Hooks) != 1 {
		t.Fatalf("want exactly one Stop hook: %+v", doc)
	}
	h := doc.Hooks.Stop[0].Hooks[0]
	want := "/usr/local/bin/hive " + advisor.HookSubcommand + " --format codex"
	if h.Type != "command" || h.Command != want || h.Timeout <= 0 {
		t.Errorf("hook = %+v, want command %q", h, want)
	}
}

func TestAdvisorLaunchFlagCopilotAndCodex(t *testing.T) {
	m := advisorTestManager()
	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })
	agent := &AgentProcess{Name: "scout", UID: 2001}

	if got := m.advisorLaunchFlag(agent, config.AdvisorCodexBackend, false); got != " -c features.hooks=true" {
		t.Errorf("codex flag = %q", got)
	}
	// Copilot has no launch flag; its hooks ride COPILOT_HOME.
	if got := m.advisorLaunchFlag(agent, config.AdvisorCopilotBackend, false); got != "" {
		t.Errorf("copilot flag = %q, want none", got)
	}
	// No hive-owned per-agent home (root agent): refused, launches without it.
	root := &AgentProcess{Name: "scout"}
	if got := m.advisorLaunchFlag(root, config.AdvisorCodexBackend, false); got != "" {
		t.Errorf("codex without a per-agent home must project nothing: %q", got)
	}
}

func TestAdvisorHooksPathsAndEnv(t *testing.T) {
	origHome := sharedAgentHome
	sharedAgentHome = "/data/home"
	defer func() { sharedAgentHome = origHome }()

	m := advisorTestManager()
	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })
	agent := &AgentProcess{Name: "scout", UID: 2001}

	if got, want := advisorHooksPath(agent, "copilot"), "/data/home/agents/scout/.copilot/hooks/hive-advisor.json"; got != want {
		t.Errorf("copilot hooks path = %q, want %q", got, want)
	}
	if got, want := advisorHooksPath(agent, "codex"), codexHomePath("scout")+"/hooks.json"; got != want {
		t.Errorf("codex hooks path = %q, want %q", got, want)
	}
	if got := advisorHooksPath(agent, "claude"); got != "" {
		t.Errorf("claude has no hook file: %q", got)
	}
	if !m.advisorHooksActive(agent, "copilot") {
		t.Error("copilot hooks must be active when the advisor is on")
	}
	if m.advisorHooksActive(&AgentProcess{Name: "other", UID: 2002}, "copilot") {
		t.Error("unadvised agent must not project hooks")
	}
}

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/advisorhook"
	"github.com/hivecommons/hive/crux/config"
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
	if h.Command != "/usr/local/bin/hive "+advisorhook.HookSubcommand {
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
	if !strings.Contains(flag, `"Stop"`) || !strings.Contains(flag, advisorhook.HookSubcommand) {
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
	want := "/usr/local/bin/hive " + advisorhook.HookSubcommand + " --format copilot"
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
	want := "/usr/local/bin/hive " + advisorhook.HookSubcommand + " --format codex"
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

// codexTrustFake installs a fake su-exec and a fake `codex` on PATH, points
// codexHomePrefix at a temp dir, and returns the manager (logging into logs),
// the advised agent, and the file the fake records its stdin to. body is the
// fake codex script after the shebang; it may use $REC (stdin record) and
// $HOOKS (the hooks.json path hive writes).
func codexTrustFake(t *testing.T, body string) (*Manager, *AgentProcess, *bytes.Buffer, string) {
	t.Helper()
	withFakeSuExec(t)
	root := t.TempDir()
	origPrefix := codexHomePrefix
	codexHomePrefix = filepath.Join(root, ".codex-")
	t.Cleanup(func() { codexHomePrefix = origPrefix })

	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(root, "codex-stdin.log")
	hooks := filepath.Join(codexHomePath("scout"), codexHooksFileName)
	script := fmt.Sprintf("#!/bin/sh\nREC=%q\nHOOKS=%q\nprintf '%%s\\n' \"$*\" > \"$REC.args\"\nprintf '%%s\\n' \"$CODEX_HOME\" > \"$REC.home\"\nenv > \"$REC.env\"\n%s", rec, hooks, body)
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	m := &Manager{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	m.SetAdvisorEnabledResolver(func(name string) bool { return name == "scout" })
	return m, &AgentProcess{Name: "scout", UID: 2001}, &logs, rec
}

// codexTrustFakeServer answers initialize, hooks/list (with one Stop hook
// from $HOOKS in the given trust state) and config/batchWrite, recording
// every line hive sends.
func codexTrustFakeServer(trustStatus string) string {
	hooksList := `{"id":2,"result":{"data":[{"cwd":"/","hooks":[` +
		`{"key":"other:stop:0:0","eventName":"stop","sourcePath":"/elsewhere/hooks.json","trustStatus":"untrusted","currentHash":"sha256:other"},` +
		`{"key":"'"$HOOKS"':stop:0:0","eventName":"stop","sourcePath":"'"$HOOKS"'","trustStatus":"` + trustStatus + `","currentHash":"sha256:abc123"}]}]}}`
	return `while IFS= read -r line; do
  printf '%s\n' "$line" >> "$REC"
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}' ;;
    *'"method":"hooks/list"'*) printf '%s\n' '` + hooksList + `' ;;
    *'"method":"config/batchWrite"'*) printf '%s\n' '{"id":3,"result":{"status":"ok"}}' ;;
  esac
done
`
}

// codexTrustSent returns the JSON-RPC messages hive sent to the fake.
func codexTrustSent(t *testing.T, rec string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatalf("fake codex recorded no stdin: %v", err)
	}
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("hive sent a non-JSON line %q: %v", line, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func codexTrustMethods(msgs []map[string]any) []string {
	var methods []string
	for _, msg := range msgs {
		methods = append(methods, fmt.Sprint(msg["method"]))
	}
	return methods
}

func TestCodexAdvisorHookTrustRPC(t *testing.T) {
	t.Run("untrusted hook is trusted via config/batchWrite", func(t *testing.T) {
		t.Setenv("HIVE_TRUST_CANARY", "canary-secret")
		m, agent, logs, rec := codexTrustFake(t, codexTrustFakeServer("untrusted"))
		m.provisionAdvisorHooks(agent, config.AdvisorCodexBackend)

		hooks := filepath.Join(codexHomePath("scout"), codexHooksFileName)
		if _, err := os.Stat(hooks); err != nil {
			t.Fatalf("hooks.json not written: %v", err)
		}
		if args, _ := os.ReadFile(rec + ".args"); strings.TrimSpace(string(args)) != "app-server --listen stdio://" {
			t.Errorf("codex args = %q", args)
		}
		if home, _ := os.ReadFile(rec + ".home"); strings.TrimSpace(string(home)) != codexHomePath("scout") {
			t.Errorf("CODEX_HOME = %q, want %q", home, codexHomePath("scout"))
		}
		// The app-server runs as the agent UID: the manager's environment
		// (hub secrets) must not reach it; PATH must, so codex resolves.
		env, _ := os.ReadFile(rec + ".env")
		if strings.Contains(string(env), "HIVE_TRUST_CANARY=") || strings.Contains(string(env), "canary-secret") {
			t.Errorf("manager env leaked into codex app-server: %s", env)
		}
		if !strings.Contains(string(env), "PATH=") {
			t.Errorf("codex app-server env lacks PATH: %s", env)
		}
		msgs := codexTrustSent(t, rec)
		want := []string{"initialize", "initialized", "hooks/list", "config/batchWrite"}
		if got := codexTrustMethods(msgs); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("methods = %v, want %v", got, want)
		}
		params, _ := msgs[3]["params"].(map[string]any)
		edits, _ := params["edits"].([]any)
		if len(edits) != 1 {
			t.Fatalf("edits = %v", params["edits"])
		}
		edit, _ := edits[0].(map[string]any)
		if edit["keyPath"] != "hooks.state" || edit["mergeStrategy"] != "upsert" {
			t.Errorf("edit = %v", edit)
		}
		value, _ := edit["value"].(map[string]any)
		entry, _ := value[hooks+":stop:0:0"].(map[string]any)
		if entry["trusted_hash"] != "sha256:abc123" || len(value) != 1 {
			t.Errorf("value = %v, want only hive's hook key with its currentHash", value)
		}
		if params["reloadUserConfig"] != true {
			t.Errorf("reloadUserConfig = %v", params["reloadUserConfig"])
		}
		if strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("unexpected warning: %s", logs.String())
		}
	})

	t.Run("already trusted hook is left alone", func(t *testing.T) {
		m, agent, logs, rec := codexTrustFake(t, codexTrustFakeServer("trusted"))
		m.provisionAdvisorHooks(agent, config.AdvisorCodexBackend)

		got := codexTrustMethods(codexTrustSent(t, rec))
		for _, method := range got {
			if method == "config/batchWrite" {
				t.Fatalf("trusted hook must not be re-trusted: %v", got)
			}
		}
		if strings.Join(got, ",") != "initialize,initialized,hooks/list" {
			t.Errorf("methods = %v", got)
		}
		if strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("unexpected warning: %s", logs.String())
		}
	})

	t.Run("server request with a colliding id is skipped", func(t *testing.T) {
		// A server->client request (has a method) that reuses id 1 must not
		// be mistaken for the initialize response.
		body := strings.Replace(codexTrustFakeServer("untrusted"),
			`*'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{"userAgent":"fake"}}' ;;`,
			`*'"method":"initialize"'*) printf '%s\n' '{"id":1,"method":"item/tool/requestUserInput","params":{}}' '{"id":1,"result":{"userAgent":"fake"}}' ;;`, 1)
		if body == codexTrustFakeServer("untrusted") {
			t.Fatal("fake server script did not match; update the test")
		}
		m, agent, logs, rec := codexTrustFake(t, body)
		m.provisionAdvisorHooks(agent, config.AdvisorCodexBackend)

		if got := codexTrustMethods(codexTrustSent(t, rec)); strings.Join(got, ",") != "initialize,initialized,hooks/list,config/batchWrite" {
			t.Errorf("methods = %v", got)
		}
		if strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("unexpected warning: %s", logs.String())
		}
	})

	t.Run("timeout is honoured when a descendant keeps stdout open", func(t *testing.T) {
		orig := codexHookTrustTimeout
		codexHookTrustTimeout = 300 * time.Millisecond
		t.Cleanup(func() { codexHookTrustTimeout = orig })
		// The fake never answers and leaves a background child holding the
		// stdout pipe; killing codex alone would not close it.
		m, agent, logs, _ := codexTrustFake(t, "sleep 20 &\nwhile IFS= read -r line; do :; done\n")
		start := time.Now()
		m.provisionAdvisorHooks(agent, config.AdvisorCodexBackend)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("trust exchange took %s; timeout not honoured", took)
		}
		out := logs.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "timed out after") {
			t.Errorf("want a timeout warning, got: %s", out)
		}
	})

	t.Run("codex exiting immediately warns and launches anyway", func(t *testing.T) {
		m, agent, logs, _ := codexTrustFake(t, "exit 0\n")
		m.provisionAdvisorHooks(agent, config.AdvisorCodexBackend)

		if _, err := os.Stat(filepath.Join(codexHomePath("scout"), codexHooksFileName)); err != nil {
			t.Fatalf("hooks.json must still be written: %v", err)
		}
		out := logs.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "failed to trust") || !strings.Contains(out, "agent=scout") || !strings.Contains(out, "reason=") {
			t.Errorf("want a warning naming the agent and reason, got: %s", out)
		}
		// The launch flag is still projected: the agent launches with hooks on.
		if got := m.advisorLaunchFlag(agent, config.AdvisorCodexBackend, false); got != " -c features.hooks=true" {
			t.Errorf("codex flag = %q", got)
		}
	})
}

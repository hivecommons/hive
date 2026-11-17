package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/meetllm"
)

// withMeetingSignIn stubs the operator Claude token and the dashboard
// Copilot token file for one test.
func withMeetingSignIn(t *testing.T, claudeToken, copilotFile string) {
	t.Helper()
	prevClaude, prevFile := meetingClaudeOperatorToken, meetingCopilotTokenFile
	meetingClaudeOperatorToken = func() string { return claudeToken }
	meetingCopilotTokenFile = copilotFile
	t.Cleanup(func() { meetingClaudeOperatorToken, meetingCopilotTokenFile = prevClaude, prevFile })
}

func mapEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The meetings CLI gets the agents' sign-in: the operator's Claude token
// (never a stray CLAUDE_CODE_OAUTH_TOKEN of the hive's own), and the
// Copilot token from the environment or the dashboard sign-in file.
func TestMeetingCLIGetenv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "copilot-user-token")
	if err := os.WriteFile(file, []byte("gho_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withMeetingSignIn(t, "sk-ant-oat-operator", file)
	get := meetingCLIGetenv(mapEnv(map[string]string{meetllm.EnvClaudeOAuthToken: "stale", "PATH": "/bin"}))
	if get(meetllm.EnvClaudeOAuthToken) != "sk-ant-oat-operator" || get(meetllm.EnvCopilotToken) != "gho_file" || get("PATH") != "/bin" {
		t.Fatal("sign-in overlay")
	}
	if got := meetingCLIGetenv(mapEnv(map[string]string{meetllm.EnvCopilotToken: " gho_env "}))(meetllm.EnvCopilotToken); got != "gho_env" {
		t.Fatalf("env copilot token %q", got)
	}
	withMeetingSignIn(t, "", filepath.Join(dir, "absent"))
	get = meetingCLIGetenv(mapEnv(map[string]string{meetllm.EnvClaudeOAuthToken: "stale"}))
	if get(meetllm.EnvClaudeOAuthToken) != "" || get(meetllm.EnvCopilotToken) != "" {
		t.Fatal("no sign-in configured, yet one was passed")
	}
}

// Spek's `hive meeting-llm` child gets each backend's sign-in variables,
// a gateway's CA bundle, and never the hive's GitHub tokens.
func TestMeetingSpekEnvSignIn(t *testing.T) {
	withMeetingSignIn(t, "sk-ant-oat-operator", filepath.Join(t.TempDir(), "absent"))
	base := mapEnv(map[string]string{
		"PATH": "/bin", "GH_TOKEN": "ghp_hive", "HIVE_GITHUB_TOKEN": "x", meetllm.EnvCopilotToken: "gho_x",
		"OMP_AUTH_BROKER_URL": "https://broker", "OMP_AUTH_BROKER_TOKEN": "obt",
	})
	envOf := func(llm config.ResolvedMeetingsLLM) map[string]string {
		out := map[string]string{}
		for _, kv := range meetingSpekEnv(base, llm, "/hive", "/u", "/s", 0) {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		if out["GH_TOKEN"] != "" || out["HIVE_GITHUB_TOKEN"] != "" {
			t.Fatalf("hive tokens reached spek: %v", out)
		}
		return out
	}
	claudeLLM := cliLLM("", "")
	if env := envOf(claudeLLM); env[meetllm.EnvClaudeOAuthToken] != "sk-ant-oat-operator" || env[meetllm.EnvCopilotToken] != "" {
		t.Fatalf("claude env %v", env)
	}
	copilotLLM := cliLLM("", "")
	copilotLLM.AgentBackend = meetllm.BackendCopilot
	if env := envOf(copilotLLM); env[meetllm.EnvCopilotToken] != "gho_x" || env[meetllm.EnvClaudeOAuthToken] != "" {
		t.Fatalf("copilot env %v", env)
	}
	ompLLM := cliLLM("", "")
	ompLLM.AgentBackend = meetllm.BackendOMP
	if env := envOf(ompLLM); env["OMP_AUTH_BROKER_URL"] != "https://broker" || env["OMP_AUTH_BROKER_TOKEN"] != "obt" {
		t.Fatalf("omp env %v", env)
	}
	gw := config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeOpenAI, Endpoint: "https://gw", Model: "m", CABundle: "/ca.pem", AgentBackend: "corp-gw"}
	if env := envOf(gw); env[meetllm.EnvCABundle] != "/ca.pem" || env[meetllm.EnvEndpoint] != "https://gw" {
		t.Fatalf("gateway env %v", env)
	}
	gw.CABundle = ""
	if _, ok := envOf(gw)[meetllm.EnvCABundle]; ok {
		t.Fatal("empty CA bundle exported")
	}
}

// On a backend with no warm mode the persona runs one CLI per call, with
// the agents' sign-in and the cold timeouts; no process starts up front.
func TestMeetingCLIModelCold(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	llm := cliLLM(filepath.Join(t.TempDir(), "copilot"), "")
	llm.AgentBackend = meetllm.BackendCopilot
	llm.CLI.AddressedTimeoutMS = config.DefaultMeetingsColdCLIAddressedTimeoutMS
	m, ok := meetingCLIModel(llm, meetllm.Config{MaxInFlight: 1}, "m1", logger)
	if !ok || m.Config.Session != nil || m.Config.Backend != meetllm.BackendCopilot || m.Config.Getenv == nil ||
		m.Config.Timeout.Milliseconds() != config.DefaultMeetingsColdCLIAddressedTimeoutMS {
		t.Fatalf("cold model %+v %v", m.Config, ok)
	}
	llm.AgentBackend = "codex"
	if _, ok := meetingCLIModel(llm, meetllm.Config{}, "m2", logger); ok {
		t.Fatal("codex has no profile")
	}
	if _, ok := meetingCLIModel(llm, meetllm.Config{}, "m3", nil); ok {
		t.Fatal("codex has no profile (no logger)")
	}
}

// A gateway CA bundle that cannot be read leaves the persona without a
// model instead of an unverified client.
func TestMeetingPersonaFactoryBadCABundle(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	persona := cfg.Meetings.Persona.Resolve()
	persona.Enabled = true
	llm := config.ResolvedMeetingsLLM{Backend: config.MeetingsLLMModeOpenAI, Endpoint: "https://gw", Model: "m", AgentBackend: "corp-gw",
		CABundle: filepath.Join(t.TempDir(), "absent.pem"), MaxInFlight: 1}
	e := meetingPersonaFactory(cfg, persona, llm, store, nil, nil, "", 0, false, slog.New(slog.DiscardHandler), nil)(meetings.Meeting{ID: "m1"})
	if e == nil {
		t.Fatal("want an engine")
	}
	e.Close()
}

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

const inferenceTestKickKey = "sk-hive-kick-0123"

type fakeInferenceRelay struct {
	enrolled []string
	released int
	err      error
}

func (f *fakeInferenceRelay) Enroll(agentName string) (string, string, []byte, func(), error) {
	if f.err != nil {
		return "", "", nil, nil, f.err
	}
	f.enrolled = append(f.enrolled, agentName)
	return "https://hive-inference.ns.svc:18446", inferenceTestKickKey, []byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"), func() { f.released++ }, nil
}

func TestSandboxInferenceEnvCarriesOnlyTheKickKey(t *testing.T) {
	env := sandboxInferenceEnv("https://hive-inference.ns.svc:18446", inferenceTestKickKey, "/workspace/.hive/inference-ca.pem", nil)
	if env["ANTHROPIC_BASE_URL"] != "https://hive-inference.ns.svc:18446" || env["ANTHROPIC_API_KEY"] != inferenceTestKickKey {
		t.Fatalf("env = %v", env)
	}
	if env["NODE_EXTRA_CA_CERTS"] != "/workspace/.hive/inference-ca.pem" {
		t.Fatalf("the CLI must trust the listener's CA: %v", env)
	}
	if env[claudeAutoModeServerEnv] != claudeAutoModeServerOff {
		t.Fatalf("auto-mode server review must be off through the translator: %v", env)
	}
	for k := range env {
		if strings.Contains(k, "TOKEN") || strings.Contains(k, "SECRET") {
			t.Errorf("env %s would be dropped by the sandbox credential filter", k)
		}
	}
}

func TestEnrollInferenceRelayGating(t *testing.T) {
	relay := &fakeInferenceRelay{}
	for _, tc := range []struct {
		name string
		e    *SandboxExecutor
		spec SandboxKickSpec
	}{
		{"no listener", &SandboxExecutor{}, SandboxKickSpec{Agent: "a", Runtime: config.SandboxRuntimeJob, InferenceRouted: true}},
		{"not a gateway backend", &SandboxExecutor{InferenceRelay: relay}, SandboxKickSpec{Agent: "a", Runtime: config.SandboxRuntimeJob}},
		{"podman runtime", &SandboxExecutor{InferenceRelay: relay}, SandboxKickSpec{Agent: "a", Runtime: config.SandboxRuntimePodman, InferenceRouted: true}},
	} {
		env, release := tc.e.enrollInferenceRelay(tc.spec, t.TempDir())
		release()
		if env != nil {
			t.Errorf("%s: env = %v, want none", tc.name, env)
		}
	}
	if len(relay.enrolled) != 0 {
		t.Fatalf("enrolled %v outside a routed Job kick", relay.enrolled)
	}
}

func TestEnrollInferenceRelayJobKick(t *testing.T) {
	relay := &fakeInferenceRelay{}
	e := &SandboxExecutor{InferenceRelay: relay}
	ws := t.TempDir()
	env, release := e.enrollInferenceRelay(SandboxKickSpec{Agent: "scanner", Runtime: config.SandboxRuntimeJob, InferenceRouted: true}, ws)
	if env["ANTHROPIC_API_KEY"] != inferenceTestKickKey || env["NODE_EXTRA_CA_CERTS"] != "/workspace/.hive/inference-ca.pem" {
		t.Fatalf("env = %v", env)
	}
	if _, err := os.Stat(filepath.Join(ws, ".hive", "inference-ca.pem")); err != nil {
		t.Fatalf("CA bundle not written: %v", err)
	}
	if len(relay.enrolled) != 1 || relay.enrolled[0] != "scanner" || relay.released != 0 {
		t.Fatalf("enrolled=%v released=%d", relay.enrolled, relay.released)
	}
	release()
	if relay.released != 1 {
		t.Fatal("kick key not revoked when the kick ended")
	}
}

func TestEnrollInferenceRelayCarriesContextSettings(t *testing.T) {
	t.Setenv(inferencePassContextWindowEnv, "")
	t.Setenv(inferenceAutoCompactPercentEnv, "80")
	for _, window := range []int{0, 160000} {
		t.Run(strconv.Itoa(window), func(t *testing.T) {
			cfg := config.AgentConfig{Backend: "litellm", Model: "vllm/Qwen", AutoCompactWindowTokens: window}
			m := NewManager(map[string]config.AgentConfig{"scanner": cfg}, discardLogger(), ProjectContext{})
			m.SetInferenceContextWindowLookup(func(string) (int, string) { return 262144, cfg.Model })
			agent := &AgentProcess{Name: "scanner", Config: cfg}
			e := &SandboxExecutor{InferenceRelay: &fakeInferenceRelay{}}
			env, release := e.enrollInferenceRelay(SandboxKickSpec{
				Agent: "scanner", Runtime: config.SandboxRuntimeJob, InferenceRouted: true,
				ClaudeContextEnv: m.claudeContextEnv(agent, cfg.Backend, cfg.Model),
			}, t.TempDir())
			defer release()
			got := sandbox.SanitizedEnv(nil, nil, env)
			if !slices.Contains(got, claudeMaxContextTokensEnv+"=262144") {
				t.Fatalf("context window missing from job env: %v", got)
			}
			if window == 0 {
				if !slices.Contains(got, claudeAutoCompactPercentEnv+"=80") {
					t.Fatalf("compaction percentage missing from job env: %v", got)
				}
			} else if !slices.Contains(got, claudeAutoCompactWindowEnv+"=160000") || slices.Contains(got, claudeAutoCompactPercentEnv+"=80") {
				t.Fatalf("explicit compaction window must override percentage: %v", got)
			}
		})
	}
}

func TestEnrollInferenceRelayFailureIsNotFatal(t *testing.T) {
	e := &SandboxExecutor{InferenceRelay: &fakeInferenceRelay{err: errors.New("boom")}}
	env, release := e.enrollInferenceRelay(SandboxKickSpec{Agent: "a", Runtime: config.SandboxRuntimeJob, InferenceRouted: true}, t.TempDir())
	release()
	if env != nil {
		t.Fatalf("env = %v, want none so the Job keeps its own model path", env)
	}
}

func TestManagerSetSandboxInferenceRelayInstallsAndClears(t *testing.T) {
	m := &Manager{}
	var host crux.Host = m
	relay := &fakeInferenceRelay{}
	host.SetSandboxInferenceRelay(relay)
	if m.sandboxInferenceRelayLoad() != SandboxInferenceRelay(relay) {
		t.Fatal("listener was not installed")
	}
	host.SetSandboxInferenceRelay(nil)
	if m.sandboxInferenceRelayLoad() != nil {
		t.Fatal("a nil listener must switch the feature off")
	}
}

// A sandboxed agent never passes through the tmux launch that sets its
// translator route, so a routed Job kick sets it itself.
func TestRouteSandboxInferenceLocked(t *testing.T) {
	type routed struct{ agent, backend, model string }
	var got []routed
	m := &Manager{}
	m.inferenceRouteCallback = func(a, b, mo string) { got = append(got, routed{a, b, mo}) }
	agent := &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "vllm", Model: "qwen"}}

	if m.routeSandboxInferenceLocked(agent, config.SandboxRuntimeJob) {
		t.Fatal("routed with no listener installed")
	}
	m.SetSandboxInferenceRelay(&fakeInferenceRelay{})
	if m.routeSandboxInferenceLocked(agent, config.SandboxRuntimePodman) {
		t.Fatal("routed a podman kick")
	}
	if m.routeSandboxInferenceLocked(&AgentProcess{Name: "c", Config: config.AgentConfig{Backend: "claude"}}, config.SandboxRuntimeJob) {
		t.Fatal("routed a backend that is not a model gateway")
	}
	if len(got) != 0 {
		t.Fatalf("route set outside a routed Job kick: %v", got)
	}
	agent.ModelOverride = "qwen-large"
	if !m.routeSandboxInferenceLocked(agent, config.SandboxRuntimeJob) {
		t.Fatal("a Job kick on a gateway backend must be routed")
	}
	if len(got) != 1 || got[0] != (routed{"scanner", "vllm", "qwen-large"}) {
		t.Fatalf("route = %v", got)
	}
}

package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// #11761: Claude Code clips its retry status line to one pane row beside the
// "Retrying in Ns" countdown, and the pane-derived ProviderErrorLine inherits
// that truncation. The proxy's full message must win on the agent card.

const clippedPaneLine = `Inference backend rejected credentials (401): Authentication Error, Invalid proxy serv`

const fullProxyError = `inference backend returned 401: {"error":{"message":"Authentication Error, Invalid proxy server token passed. Unable to find token in cache or LiteLLM_VerificationTokenTable","type":"auth_error"}} (upstream: https://gw.example/v1/chat/completions, server: uvicorn)`

func withInferenceLastErrorProvider(t *testing.T, fn func(string) (string, int, time.Time, bool)) {
	t.Helper()
	SetInferenceLastErrorProvider(fn)
	t.Cleanup(func() { SetInferenceLastErrorProvider(nil) })
}

func backendAuthErrorStatuses(cfg *config.Config, lastKick *time.Time) map[string]*agent.AgentProcess {
	return map[string]*agent.AgentProcess{
		"scanner": {
			Name:                      "scanner",
			Config:                    cfg.Agents["scanner"],
			State:                     agent.StateRunning,
			OutputBuffer:              agent.NewRingBuffer(10),
			LastKick:                  lastKick,
			ProviderErrorClass:        "backend_auth_error",
			ProviderErrorLine:         clippedPaneLine,
			ProviderErrorBackoffUntil: time.Now().Add(time.Minute),
			BackendAuth: agent.BackendAuthState{
				Status:    agent.BackendAuthUnreachable,
				Since:     time.Now(),
				LastError: clippedPaneLine,
			},
		},
	}
}

func inferenceLastErrorCfg() *config.Config {
	return &config.Config{
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Backend: "claude", Model: "claude-sonnet", Enabled: true},
			},
		},
	}
}

func TestBuildFrontendStatus_PrefersProxyFullInferenceError(t *testing.T) {
	at := time.Now().Add(-30 * time.Second)
	withInferenceLastErrorProvider(t, func(name string) (string, int, time.Time, bool) {
		if name != "scanner" {
			return "", 0, time.Time{}, false
		}
		return fullProxyError, 401, at, true
	})
	cfg := inferenceLastErrorCfg()
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	payload := BuildFrontendStatus(gov.GetState(), nil, backendAuthErrorStatuses(cfg, nil), cfg, nil, gov, nil, nil, nil, nil)
	a := payload.Agents[0]

	if a.InferenceLastError != fullProxyError || a.InferenceLastErrorAt == "" {
		t.Fatalf("InferenceLastError = %q at %q, want full proxy error with timestamp", a.InferenceLastError, a.InferenceLastErrorAt)
	}
	wantBackend := "Inference backend rejected credentials (401): " + fullProxyError
	if a.BackendAuthLastError != wantBackend {
		t.Fatalf("BackendAuthLastError = %q, want %q (classification prefix kept, clipped detail replaced)", a.BackendAuthLastError, wantBackend)
	}
	if !strings.HasPrefix(a.StatusEvidence, "blocked: inference (backend_auth_error): ") ||
		!strings.Contains(a.StatusEvidence, "LiteLLM_VerificationTokenTable") {
		t.Fatalf("StatusEvidence = %q, want full proxy error after the classification", a.StatusEvidence)
	}
}

// Mid-retry, before the pane poller classifies anything, the error is still
// shown when it happened during the current turn (newer than the last kick).
func TestBuildFrontendStatus_InferenceLastErrorDuringCurrentTurn(t *testing.T) {
	kick := time.Now().Add(-2 * time.Minute)
	withInferenceLastErrorProvider(t, func(string) (string, int, time.Time, bool) {
		return fullProxyError, 401, time.Now().Add(-time.Minute), true
	})
	cfg := inferenceLastErrorCfg()
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name: "scanner", Config: cfg.Agents["scanner"], State: agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10), LastKick: &kick,
		},
	}
	a := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil).Agents[0]
	if a.InferenceLastError != fullProxyError {
		t.Fatalf("InferenceLastError = %q, want the error recorded during the current turn", a.InferenceLastError)
	}
	if a.BackendAuthLastError != "" {
		t.Fatalf("BackendAuthLastError = %q, want empty while no backend-auth state is classified", a.BackendAuthLastError)
	}
}

// Conservative gating: an error from before the last kick on an otherwise
// healthy agent, or one older than inferenceLastErrorMaxAge, is not shown.
func TestBuildFrontendStatus_InferenceLastErrorStaleIsHidden(t *testing.T) {
	cfg := inferenceLastErrorCfg()
	gov := governor.New(cfg.Governor, cfg.Agents, nil)

	kick := time.Now().Add(-time.Minute)
	withInferenceLastErrorProvider(t, func(string) (string, int, time.Time, bool) {
		return fullProxyError, 401, time.Now().Add(-5 * time.Minute), true
	})
	statuses := map[string]*agent.AgentProcess{
		"scanner": {
			Name: "scanner", Config: cfg.Agents["scanner"], State: agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10), LastKick: &kick,
		},
	}
	if a := BuildFrontendStatus(gov.GetState(), nil, statuses, cfg, nil, gov, nil, nil, nil, nil).Agents[0]; a.InferenceLastError != "" {
		t.Fatalf("error older than the last kick on a healthy agent shown: %q", a.InferenceLastError)
	}

	SetInferenceLastErrorProvider(func(string) (string, int, time.Time, bool) {
		return fullProxyError, 401, time.Now().Add(-inferenceLastErrorMaxAge - time.Minute), true
	})
	a := BuildFrontendStatus(gov.GetState(), nil, backendAuthErrorStatuses(cfg, nil), cfg, nil, gov, nil, nil, nil, nil).Agents[0]
	if a.InferenceLastError != "" || a.BackendAuthLastError != clippedPaneLine {
		t.Fatalf("stale proxy error used: InferenceLastError=%q BackendAuthLastError=%q", a.InferenceLastError, a.BackendAuthLastError)
	}
}

// No provider registered (tests, spokes without a proxy) ⇒ unchanged output.
func TestBuildFrontendStatus_NoInferenceLastErrorProviderUnchanged(t *testing.T) {
	SetInferenceLastErrorProvider(nil)
	cfg := inferenceLastErrorCfg()
	gov := governor.New(cfg.Governor, cfg.Agents, nil)
	a := BuildFrontendStatus(gov.GetState(), nil, backendAuthErrorStatuses(cfg, nil), cfg, nil, gov, nil, nil, nil, nil).Agents[0]
	if a.InferenceLastError != "" || a.InferenceLastErrorAt != "" {
		t.Fatalf("InferenceLastError = %q/%q, want empty without a provider", a.InferenceLastError, a.InferenceLastErrorAt)
	}
	if a.BackendAuthLastError != clippedPaneLine {
		t.Fatalf("BackendAuthLastError = %q, want pane line %q", a.BackendAuthLastError, clippedPaneLine)
	}
	if a.StatusEvidence != "blocked: inference (backend_auth_error): "+clippedPaneLine {
		t.Fatalf("StatusEvidence = %q, want pane-derived evidence unchanged", a.StatusEvidence)
	}
}

func TestPreferFullInferenceError(t *testing.T) {
	for _, tc := range []struct{ pane, full, want string }{
		{clippedPaneLine, "", clippedPaneLine},
		{clippedPaneLine, "FULL", "Inference backend rejected credentials (401): FULL"},
		{"Inference backend outage (401): Error querying the da", "FULL", "Inference backend outage (401): FULL"},
		{"API Error: 401 {\"type\":\"error\"… · Retrying in 28s", "FULL", "FULL"},
		{"", "FULL", "FULL"},
	} {
		if got := preferFullInferenceError(tc.pane, tc.full); got != tc.want {
			t.Errorf("preferFullInferenceError(%q, %q) = %q, want %q", tc.pane, tc.full, got, tc.want)
		}
	}
}

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestClassifyProviderError(t *testing.T) {
	cases := []struct {
		name      string
		pane      string
		wantClass string
		want      bool
	}{
		{
			name:      "issue 502 unreachable",
			pane:      `API Error: 502 {"type":"error","error":{"type":"api_error","message":"inference backend unreachable: Post \"https://example.invalid/v1/chat/completions\": dial tcp: lookup example.invalid: no such host"}}`,
			wantClass: "api_error",
			want:      true,
		},
		{
			name:      "claude retry countdown",
			pane:      `502 {"type":"error"} · Retrying in 14s · attempt 6/10`,
			wantClass: "retrying",
			want:      true,
		},
		{
			name:      "rate limit json",
			pane:      `API Error: 429 {"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "auth 401",
			pane:      `API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			wantClass: "auth",
			want:      true,
		},
		{
			name:      "auth 403",
			pane:      `API Error: 403 {"type":"error","error":{"message":"team not allowed to access model"}}`,
			wantClass: "auth",
			want:      true,
		},
		{
			name:      "quota",
			pane:      `Provider returned {"error":{"type":"insufficient_quota","message":"quota exhausted"}}`,
			wantClass: "quota",
			want:      true,
		},
		{
			name: "normal prose",
			pane: "I inspected the repository and will run the tests next.",
			want: false,
		},
		{
			name: "plan without tool calls",
			pane: "Plan:\n1. Inspect the code.\n2. Add tests.\n3. Open a PR.",
			want: false,
		},
		{
			name: "tool result mentioning error identifier in code",
			pane: "⎿  const err = errors.New(\"provider returned error\")\n❯ ",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyProviderError(tc.pane)
			if ok != tc.want {
				t.Fatalf("classifyProviderError ok=%v, want %v (match=%+v)", ok, tc.want, got)
			}
			if tc.want && got.Class != tc.wantClass {
				t.Fatalf("classifyProviderError class=%q, want %q", got.Class, tc.wantClass)
			}
			if tc.want && !strings.Contains(tc.pane, got.Line) {
				t.Fatalf("error line %q was not captured from pane %q", got.Line, tc.pane)
			}
		})
	}
}

func TestNudgeIfKickStalled_ProviderErrorBacksOffInsteadOfActionNudge(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "1s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "4s")
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	pane := "❯ [agent:quality] work\n" +
		`API Error: 502 {"type":"error","error":{"type":"api_error","message":"inference backend unreachable"}}` + "\n" +
		"✻ Worked for 3m 2s\n" +
		"❯ "
	agent.State = StateRunning
	agent.lastInferKickAt = time.Now().Add(-inferenceActionNudgeGrace - time.Minute)
	agent.lastInferKickPane = paneContentHash("pane at kick")
	agent.lastInferKickMarks = 0

	m.nudgeIfKickStalled("vinf", pane)
	if agent.ActionNudges != 0 {
		t.Fatalf("provider error must not count as action nudge, got %d", agent.ActionNudges)
	}
	if agent.ProviderErrorClass != "api_error" {
		t.Fatalf("ProviderErrorClass = %q, want api_error", agent.ProviderErrorClass)
	}
	if agent.LastError == "" || !strings.Contains(agent.LastError, "API Error: 502") {
		t.Fatalf("LastError = %q, want surfaced provider error line", agent.LastError)
	}
	if remaining, class, _, ok := m.ProviderErrorBackoffRemaining("vinf"); !ok || class != "api_error" || remaining <= 0 {
		t.Fatalf("ProviderErrorBackoffRemaining = (%v, %q, ok=%v), want active api_error", remaining, class, ok)
	}
	if err := m.SendKick("vinf", "retry now"); err == nil || !strings.Contains(err.Error(), "blocked: inference (api_error)") {
		t.Fatalf("SendKick during backoff error = %v, want blocked inference", err)
	}
}

func TestNudgeIfKickStalled_IgnoresProviderErrorBeforeKickBaseline(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	stale := `API Error: 502 {"type":"error","error":{"type":"api_error","message":"old outage"}}`
	baseline := stale + "\n❯ [agent:quality] retry after backoff"
	pane := baseline + "\n⏺ I will inspect the repository and then run tests.\n✻ Worked for 26s\n❯ "
	agent.State = StateRunning
	agent.lastInferKickAt = time.Now().Add(-inferenceActionNudgeGrace - time.Minute)
	agent.lastInferKickPane = paneContentHash(baseline)
	agent.lastInferKickVisible = baseline
	agent.lastInferKickMarks = 0

	m.nudgeIfKickStalled("vinf", pane)
	if agent.ProviderErrorClass != "" {
		t.Fatalf("stale provider error re-armed backoff: class=%q line=%q", agent.ProviderErrorClass, agent.ProviderErrorLine)
	}
	if agent.ActionNudges != 1 {
		t.Fatalf("current prose-only response should still get action nudge, got %d", agent.ActionNudges)
	}
}

func TestProviderErrorBackoffDelayUsesEnvAndCaps(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "2s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "5s")
	if got := providerErrorBackoffDelay(1); got != 2*time.Second {
		t.Fatalf("attempt 1 delay = %v, want 2s", got)
	}
	if got := providerErrorBackoffDelay(2); got != 4*time.Second {
		t.Fatalf("attempt 2 delay = %v, want 4s", got)
	}
	if got := providerErrorBackoffDelay(3); got != 5*time.Second {
		t.Fatalf("attempt 3 delay = %v, want capped 5s", got)
	}
}

package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// providerErrorStatusClass maps HTTP status codes to backoff classes. The
// switch arms for 529 and the default were previously uncovered.
func TestProviderErrorStatusClass(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		{"401", "auth"},
		{"403", "auth"},
		{"429", "rate_limit"},
		{"529", "overloaded"},
		{"500", "api_error"},
		{"502", "api_error"},
		{"503", "api_error"},
	}
	for _, tc := range cases {
		if got := providerErrorStatusClass(tc.status); got != tc.want {
			t.Errorf("providerErrorStatusClass(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// classifyProviderError branches for overloaded errors, contextual
// rate-limit prose, and bare HTTP statuses with API context.
func TestClassifyProviderError_StatusAndOverloadBranches(t *testing.T) {
	cases := []struct {
		name      string
		pane      string
		wantClass string
		want      bool
	}{
		{
			name:      "overloaded_error json",
			pane:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "overloaded prose with api context",
			pane:      `inference backend overloaded, shedding load`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "too many requests with api context",
			pane:      `inference backend: too many requests, slow down`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "rate limit prose with api context",
			pane:      `API error: rate limit reached for model`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "api error status 529",
			pane:      `API Error: 529 upstream saturated`,
			wantClass: "overloaded",
			want:      true,
		},
		{
			name:      "api error status 429",
			pane:      `API Error: 429 slow down`,
			wantClass: "rate_limit",
			want:      true,
		},
		{
			name:      "http status with api context",
			pane:      `inference backend returned 503`,
			wantClass: "api_error",
			want:      true,
		},
		{
			name:      "http auth status with api context",
			pane:      `backend rejected request: 401`,
			wantClass: "auth",
			want:      true,
		},
		{
			name: "bare status without api context is not an error",
			pane: "503 lines changed in the diff",
			want: false,
		},
		{
			name: "rate limit prose without api context is not an error",
			pane: "the function should rate limit outbound emails",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyProviderError(tc.pane)
			if ok != tc.want {
				t.Fatalf("classifyProviderError(%q) ok=%v, want %v (match=%+v)", tc.pane, ok, tc.want, got)
			}
			if tc.want && got.Class != tc.wantClass {
				t.Fatalf("classifyProviderError(%q) class=%q, want %q", tc.pane, got.Class, tc.wantClass)
			}
		})
	}
}

// paneAfterKickBaseline trims pre-kick scrollback so stale provider errors are
// not re-detected. The line-prefix fallback and disjoint branches were
// previously uncovered.
func TestPaneAfterKickBaseline(t *testing.T) {
	cases := []struct {
		name     string
		pane     string
		baseline string
		want     string
	}{
		{
			name:     "empty baseline returns pane",
			pane:     "a\nb",
			baseline: "",
			want:     "a\nb",
		},
		{
			name:     "baseline substring returns suffix after last occurrence",
			pane:     "old\nAPI Error: 502\nnew output",
			baseline: "old\nAPI Error: 502",
			want:     "\nnew output",
		},
		{
			name:     "repeated baseline trims after the last occurrence",
			pane:     "kick\nkick\ntail",
			baseline: "kick",
			want:     "\ntail",
		},
		{
			name:     "no substring but shared leading lines are trimmed",
			pane:     "line1\nline2\nfresh error",
			baseline: "line1\nline2\nscrolled away",
			want:     "fresh error",
		},
		{
			name:     "fully disjoint baseline returns whole pane",
			pane:     "completely\nnew",
			baseline: "other\ncontent",
			want:     "completely\nnew",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := paneAfterKickBaseline(tc.pane, tc.baseline); got != tc.want {
				t.Fatalf("paneAfterKickBaseline(%q, %q) = %q, want %q", tc.pane, tc.baseline, got, tc.want)
			}
		})
	}
}

// clearProviderErrorLocked resets backoff state. The early-return and the
// LastError-preservation branches were previously uncovered.
func TestClearProviderErrorLocked(t *testing.T) {
	newAgent := func(t *testing.T) (*Manager, *AgentProcess) {
		t.Helper()
		m := NewManager(map[string]config.AgentConfig{
			"vinf": {Backend: "vllm"},
		}, discardLogger(), ProjectContext{})
		m.mu.RLock()
		agent := m.agents["vinf"]
		m.mu.RUnlock()
		return m, agent
	}

	t.Run("no-op when no provider error is recorded", func(t *testing.T) {
		m, agent := newAgent(t)
		agent.LastError = "unrelated startup failure"
		m.clearProviderErrorLocked(agent)
		if agent.LastError != "unrelated startup failure" {
			t.Fatalf("LastError = %q, want untouched unrelated error", agent.LastError)
		}
	})

	t.Run("clears provider fields and matching LastError", func(t *testing.T) {
		m, agent := newAgent(t)
		agent.ProviderErrorClass = "api_error"
		agent.ProviderErrorLine = "API Error: 502 backend unreachable"
		agent.ProviderErrorBackoffUntil = time.Now().Add(time.Minute)
		agent.providerErrorBackoffAttempt = 3
		agent.LastError = agent.ProviderErrorLine

		m.clearProviderErrorLocked(agent)
		if agent.ProviderErrorClass != "" || agent.ProviderErrorLine != "" {
			t.Fatalf("provider error not cleared: class=%q line=%q", agent.ProviderErrorClass, agent.ProviderErrorLine)
		}
		if !agent.ProviderErrorBackoffUntil.IsZero() || agent.providerErrorBackoffAttempt != 0 {
			t.Fatalf("backoff not reset: until=%v attempt=%d", agent.ProviderErrorBackoffUntil, agent.providerErrorBackoffAttempt)
		}
		if agent.LastError != "" {
			t.Fatalf("LastError = %q, want cleared when it mirrors the provider error", agent.LastError)
		}
	})

	t.Run("preserves a newer unrelated LastError", func(t *testing.T) {
		m, agent := newAgent(t)
		agent.ProviderErrorClass = "rate_limit"
		agent.ProviderErrorLine = "API Error: 429 slow down"
		agent.ProviderErrorBackoffUntil = time.Now().Add(time.Minute)
		agent.providerErrorBackoffAttempt = 1
		agent.LastError = "tmux pane vanished"

		m.clearProviderErrorLocked(agent)
		if agent.ProviderErrorClass != "" || !agent.ProviderErrorBackoffUntil.IsZero() {
			t.Fatalf("provider error not cleared: class=%q until=%v", agent.ProviderErrorClass, agent.ProviderErrorBackoffUntil)
		}
		if agent.LastError != "tmux pane vanished" {
			t.Fatalf("LastError = %q, want unrelated error preserved", agent.LastError)
		}
	})
}

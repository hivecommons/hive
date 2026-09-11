package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func TestClassifyBackendAuthStatus(t *testing.T) {
	cases := []struct {
		name       string
		class      string
		line       string
		wantStatus string
		wantOK     bool
	}{
		{
			name:       "explicit not-licensed text is unlicensed (#6500)",
			class:      "auth",
			line:       "✗ You are not licensed to use Copilot. (Request ID: abc)",
			wantStatus: BackendAuthUnlicensed,
			wantOK:     true,
		},
		{
			name:       "bare 401 auth class is token-expired",
			class:      "auth",
			line:       "API Error: 401 unauthorized",
			wantStatus: BackendAuthTokenExpired,
			wantOK:     true,
		},
		{
			name:       "quota class is quota",
			class:      "quota",
			line:       "insufficient_quota",
			wantStatus: BackendAuthQuota,
			wantOK:     true,
		},
		{
			name:       "api_error class is unreachable",
			class:      "api_error",
			line:       "inference backend unreachable",
			wantStatus: BackendAuthUnreachable,
			wantOK:     true,
		},
		{
			name:   "rate_limit is transient, not a backend-auth verdict",
			class:  "rate_limit",
			line:   "API Error: 429 too many requests",
			wantOK: false,
		},
		{
			name:   "overloaded is transient, not a backend-auth verdict",
			class:  "overloaded",
			line:   "API Error: 529 overloaded",
			wantOK: false,
		},
		{
			name:   "retrying is transient, not a backend-auth verdict",
			class:  "retrying",
			line:   "Retrying in 5s · attempt 1/3",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := classifyBackendAuthStatus(tc.class, tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", status, tc.wantStatus)
			}
		})
	}
}

func newBackendAuthTestAgent(t *testing.T) *AgentProcess {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()
	return agent
}

func TestMarkBackendAuthLocked_FirstObservationSetsSince(t *testing.T) {
	agent := newBackendAuthTestAgent(t)
	now := time.Now()

	agent.markBackendAuthLocked(BackendAuthUnlicensed, "not licensed", now)
	if agent.BackendAuth.Status != BackendAuthUnlicensed {
		t.Fatalf("Status = %q, want %q", agent.BackendAuth.Status, BackendAuthUnlicensed)
	}
	if !agent.BackendAuth.Since.Equal(now) {
		t.Fatalf("Since = %v, want %v", agent.BackendAuth.Since, now)
	}
	if agent.BackendAuth.LastError != "not licensed" {
		t.Fatalf("LastError = %q, want %q", agent.BackendAuth.LastError, "not licensed")
	}
}

func TestMarkBackendAuthLocked_SameStatusPreservesSince(t *testing.T) {
	agent := newBackendAuthTestAgent(t)
	first := time.Now()
	agent.markBackendAuthLocked(BackendAuthUnlicensed, "not licensed v1", first)

	later := first.Add(10 * time.Minute)
	agent.markBackendAuthLocked(BackendAuthUnlicensed, "not licensed v2", later)

	if !agent.BackendAuth.Since.Equal(first) {
		t.Fatalf("Since moved on repeat observation: got %v, want original %v", agent.BackendAuth.Since, first)
	}
	if agent.BackendAuth.LastError != "not licensed v2" {
		t.Fatalf("LastError not updated on repeat observation: %q", agent.BackendAuth.LastError)
	}
}

func TestMarkBackendAuthLocked_StatusChangeResetsSince(t *testing.T) {
	agent := newBackendAuthTestAgent(t)
	first := time.Now()
	agent.markBackendAuthLocked(BackendAuthQuota, "insufficient_quota", first)

	later := first.Add(time.Hour)
	agent.markBackendAuthLocked(BackendAuthUnlicensed, "not licensed", later)

	if agent.BackendAuth.Status != BackendAuthUnlicensed {
		t.Fatalf("Status = %q, want %q", agent.BackendAuth.Status, BackendAuthUnlicensed)
	}
	if !agent.BackendAuth.Since.Equal(later) {
		t.Fatalf("Since not reset on status change: got %v, want %v", agent.BackendAuth.Since, later)
	}
}

func TestClearBackendAuthLocked(t *testing.T) {
	agent := newBackendAuthTestAgent(t)

	// No-op on an already-ok agent: no spurious Since stamp.
	clearAt := time.Now()
	agent.clearBackendAuthLocked(clearAt)
	if agent.BackendAuth.Status != "" {
		t.Fatalf("clearing an ok agent set Status: %q", agent.BackendAuth.Status)
	}

	agent.markBackendAuthLocked(BackendAuthTokenExpired, "API Error: 401", time.Now())
	agent.clearBackendAuthLocked(clearAt)
	if agent.BackendAuth.Status != BackendAuthOK {
		t.Fatalf("Status after clear = %q, want %q", agent.BackendAuth.Status, BackendAuthOK)
	}
	if !agent.BackendAuth.Since.Equal(clearAt) {
		t.Fatalf("Since after clear = %v, want %v", agent.BackendAuth.Since, clearAt)
	}
	if agent.BackendAuth.LastError != "" {
		t.Fatalf("LastError survived clear: %q", agent.BackendAuth.LastError)
	}
}

func TestSafeBackendAuthDetail_TruncatesLongLines(t *testing.T) {
	long := make([]byte, backendAuthErrorLimit+50)
	for i := range long {
		long[i] = 'x'
	}
	got := safeBackendAuthDetail(string(long))
	if len(got) != backendAuthErrorLimit {
		t.Fatalf("len(got) = %d, want %d", len(got), backendAuthErrorLimit)
	}
}

// TestMarkProviderErrorLockedUpdatesBackendAuth verifies the end-to-end wiring
// this issue asked for: classifyProviderError's verdict, once observed by
// markProviderErrorLocked, becomes a BackendAuth transition — including on
// the backoff-still-active early return, where an operator still needs "still
// unlicensed" to read correctly.
func TestMarkProviderErrorLockedUpdatesBackendAuth(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "10s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "40s")
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	now := time.Now()
	match := providerErrorMatch{Class: "auth", Line: "✗ You are not licensed to use Copilot."}
	m.markProviderErrorLocked(agent, match, now)
	if agent.BackendAuth.Status != BackendAuthUnlicensed {
		t.Fatalf("BackendAuth.Status = %q, want %q", agent.BackendAuth.Status, BackendAuthUnlicensed)
	}
	firstSince := agent.BackendAuth.Since

	// Repeat within the active backoff window (early return in
	// markProviderErrorLocked) must still refresh BackendAuth.
	later := now.Add(2 * time.Second)
	m.markProviderErrorLocked(agent, match, later)
	if agent.BackendAuth.Status != BackendAuthUnlicensed {
		t.Fatalf("BackendAuth.Status after repeat = %q, want %q", agent.BackendAuth.Status, BackendAuthUnlicensed)
	}
	if !agent.BackendAuth.Since.Equal(firstSince) {
		t.Fatalf("Since moved on repeat: got %v, want %v", agent.BackendAuth.Since, firstSince)
	}

	// A clear resets it to ok.
	m.clearProviderErrorLocked(agent, later.Add(time.Second))
	if agent.BackendAuth.Status != BackendAuthOK {
		t.Fatalf("BackendAuth.Status after clear = %q, want %q", agent.BackendAuth.Status, BackendAuthOK)
	}
}

// TestMarkProviderErrorLockedTransientDoesNotSetBackendAuth verifies that a
// retryable class (rate_limit/overloaded/retrying) never touches BackendAuth,
// so a transient blip cannot false-positive the fleet-wide auth canary.
func TestMarkProviderErrorLockedTransientDoesNotSetBackendAuth(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "10s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "40s")
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()

	m.markProviderErrorLocked(agent, providerErrorMatch{Class: "rate_limit", Line: "API Error: 429"}, time.Now())
	if agent.BackendAuth.Status != "" {
		t.Fatalf("transient rate_limit set BackendAuth.Status = %q, want empty", agent.BackendAuth.Status)
	}
}

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for the provider-error backoff state lifecycle in manager.go:
// markProviderErrorLocked, clearProviderErrorLocked,
// providerErrorBackoffRemainingLocked, ProviderErrorBackoffRemaining,
// providerErrorStatusClass, providerLineHasAPIContext, and the post-kick
// pane baseline helper paneAfterKickBaseline.

func newProviderStateManager(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"vinf": {Backend: "vllm"},
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["vinf"]
	m.mu.RUnlock()
	return m, agent
}

func TestPaneAfterKickBaseline(t *testing.T) {
	cases := []struct {
		name     string
		pane     string
		baseline string
		want     string
	}{
		{
			name:     "empty baseline returns whole pane",
			pane:     "line1\nline2",
			baseline: "",
			want:     "line1\nline2",
		},
		{
			name:     "baseline still in scrollback returns suffix after it",
			pane:     "old work\n❯ kick delivered\nnew output",
			baseline: "❯ kick delivered",
			want:     "\nnew output",
		},
		{
			name: "last occurrence wins when baseline repeats",
			pane: "marker\nA\nmarker\nB",

			baseline: "marker",
			want:     "\nB",
		},
		{
			name:     "scrolled baseline falls back to common line prefix",
			pane:     "shared1\nshared2\nfresh output",
			baseline: "shared1\nshared2\nscrolled away",
			want:     "fresh output",
		},
		{
			name:     "no overlap at all returns whole pane",
			pane:     "completely\nnew",
			baseline: "gone\nbaseline",
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

func TestMarkProviderErrorLockedSetsStateAndEscalates(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "10s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "40s")
	mgrOwner, agent := newProviderStateManager(t)
	m := providerErrorMatch{Class: "api_error", Line: "API Error: 502 boom"}
	now := time.Now()

	agent.lastInferKickPane = "stale-hash"
	agent.actionNudgeSent = true

	delay := mgrOwner.markProviderErrorLocked(agent, m, now)
	if delay != 10*time.Second {
		t.Fatalf("first attempt delay = %v, want 10s", delay)
	}
	if agent.ProviderErrorClass != "api_error" || agent.ProviderErrorLine != m.Line {
		t.Fatalf("class/line = %q/%q, want api_error/%q", agent.ProviderErrorClass, agent.ProviderErrorLine, m.Line)
	}
	if agent.LastError != m.Line {
		t.Fatalf("LastError = %q, want %q", agent.LastError, m.Line)
	}
	if agent.lastInferKickPane != "" || agent.actionNudgeSent {
		t.Fatalf("kick baseline not reset: pane=%q nudgeSent=%v", agent.lastInferKickPane, agent.actionNudgeSent)
	}
	wantUntil := now.Add(10 * time.Second)
	if !agent.ProviderErrorBackoffUntil.Equal(wantUntil) {
		t.Fatalf("BackoffUntil = %v, want %v", agent.ProviderErrorBackoffUntil, wantUntil)
	}

	// Same class+line while the backoff is still active: no escalation,
	// returns remaining time instead.
	later := now.Add(4 * time.Second)
	if d := mgrOwner.markProviderErrorLocked(agent, m, later); d != 6*time.Second {
		t.Fatalf("repeat within backoff = %v, want remaining 6s", d)
	}
	if agent.providerErrorBackoffAttempt != 1 {
		t.Fatalf("attempt escalated on repeat: %d", agent.providerErrorBackoffAttempt)
	}

	// A different error line escalates to the next attempt (doubled delay).
	other := providerErrorMatch{Class: "api_error", Line: "API Error: 502 different"}
	if d := mgrOwner.markProviderErrorLocked(agent, other, later); d != 20*time.Second {
		t.Fatalf("second attempt delay = %v, want 20s", d)
	}
	if agent.providerErrorBackoffAttempt != 2 {
		t.Fatalf("attempt = %d, want 2", agent.providerErrorBackoffAttempt)
	}
}

func TestMarkProviderErrorLockedReArmsAfterExpiry(t *testing.T) {
	t.Setenv(ProviderErrorBackoffBaseEnv, "10s")
	t.Setenv(ProviderErrorBackoffMaxEnv, "40s")
	mgr, agent := newProviderStateManager(t)
	m := providerErrorMatch{Class: "rate_limit", Line: "API Error: 429 slow down"}
	now := time.Now()

	_ = mgr.markProviderErrorLocked(agent, m, now)
	// Same line but the previous backoff has fully elapsed: escalate.
	after := now.Add(11 * time.Second)
	if d := mgr.markProviderErrorLocked(agent, m, after); d != 20*time.Second {
		t.Fatalf("re-arm after expiry delay = %v, want 20s", d)
	}
}

func TestClearProviderErrorLocked(t *testing.T) {
	mgr, agent := newProviderStateManager(t)
	now := time.Now()

	// No-op when there is nothing to clear.
	agent.LastError = "unrelated error"
	mgr.clearProviderErrorLocked(agent, now)
	if agent.LastError != "unrelated error" {
		t.Fatalf("no-op clear touched LastError: %q", agent.LastError)
	}

	// Full clear resets every backoff field and the surfaced LastError when
	// it was the provider line.
	line := "API Error: 529 overloaded"
	agent.ProviderErrorClass = "overloaded"
	agent.ProviderErrorLine = line
	agent.ProviderErrorBackoffUntil = time.Now().Add(time.Minute)
	agent.providerErrorBackoffAttempt = 3
	agent.LastError = line
	mgr.clearProviderErrorLocked(agent, now)
	if agent.ProviderErrorClass != "" || agent.ProviderErrorLine != "" ||
		!agent.ProviderErrorBackoffUntil.IsZero() || agent.providerErrorBackoffAttempt != 0 {
		t.Fatalf("clear left state: class=%q line=%q until=%v attempt=%d",
			agent.ProviderErrorClass, agent.ProviderErrorLine,
			agent.ProviderErrorBackoffUntil, agent.providerErrorBackoffAttempt)
	}
	if agent.LastError != "" {
		t.Fatalf("LastError not cleared with provider line: %q", agent.LastError)
	}

	// LastError from another source survives the clear.
	agent.ProviderErrorClass = "auth"
	agent.ProviderErrorLine = "API Error: 401"
	agent.LastError = "tmux session lost"
	mgr.clearProviderErrorLocked(agent, now)
	if agent.LastError != "tmux session lost" {
		t.Fatalf("clear overwrote unrelated LastError: %q", agent.LastError)
	}
}

func TestProviderErrorBackoffRemainingLocked(t *testing.T) {
	mgr, agent := newProviderStateManager(t)
	now := time.Now()

	if d := mgr.providerErrorBackoffRemainingLocked(nil, now); d != 0 {
		t.Fatalf("nil agent remaining = %v, want 0", d)
	}
	if d := mgr.providerErrorBackoffRemainingLocked(agent, now); d != 0 {
		t.Fatalf("zero-until remaining = %v, want 0", d)
	}
	agent.ProviderErrorBackoffUntil = now.Add(-time.Second)
	if d := mgr.providerErrorBackoffRemainingLocked(agent, now); d != 0 {
		t.Fatalf("expired remaining = %v, want 0", d)
	}
	agent.ProviderErrorBackoffUntil = now.Add(30 * time.Second)
	if d := mgr.providerErrorBackoffRemainingLocked(agent, now); d != 30*time.Second {
		t.Fatalf("active remaining = %v, want 30s", d)
	}
}

func TestProviderErrorBackoffRemainingPublic(t *testing.T) {
	mgr, agent := newProviderStateManager(t)

	if _, _, _, ok := mgr.ProviderErrorBackoffRemaining("no-such-agent"); ok {
		t.Fatal("unknown agent reported an active backoff")
	}

	// Expired backoff: not active, but the last class/line are still surfaced.
	agent.ProviderErrorClass = "auth"
	agent.ProviderErrorLine = "API Error: 401 unauthorized"
	agent.ProviderErrorBackoffUntil = time.Now().Add(-time.Minute)
	remaining, class, line, ok := mgr.ProviderErrorBackoffRemaining("vinf")
	if ok || remaining != 0 {
		t.Fatalf("expired backoff = (%v, ok=%v), want inactive", remaining, ok)
	}
	if class != "auth" || !strings.Contains(line, "401") {
		t.Fatalf("expired backoff class/line = %q/%q, want surfaced auth/401", class, line)
	}

	agent.ProviderErrorBackoffUntil = time.Now().Add(time.Minute)
	remaining, class, _, ok = mgr.ProviderErrorBackoffRemaining("vinf")
	if !ok || remaining <= 0 || class != "auth" {
		t.Fatalf("active backoff = (%v, %q, ok=%v), want active auth", remaining, class, ok)
	}
}

func TestProviderErrorStatusClass(t *testing.T) {
	cases := map[string]string{
		"401": "auth",
		"403": "auth",
		"429": "rate_limit",
		"529": "overloaded",
		"500": "api_error",
		"502": "api_error",
		"":    "api_error",
	}
	for status, want := range cases {
		if got := providerErrorStatusClass(status); got != want {
			t.Fatalf("providerErrorStatusClass(%q) = %q, want %q", status, got, want)
		}
	}
}

func TestProviderLineHasAPIContext(t *testing.T) {
	positives := []string{
		"api error: 502",
		`{"type":"api_error"}`,
		"inference backend unreachable",
		"backend timeout",
		"quota exceeded",
		"401 unauthorized",
		"403 forbidden",
	}
	for _, line := range positives {
		if !providerLineHasAPIContext(line) {
			t.Fatalf("providerLineHasAPIContext(%q) = false, want true", line)
		}
	}
	negatives := []string{
		"compiling package",
		"error: file not found",
		"HTTP 502 from wiki mirror",
	}
	for _, line := range negatives {
		if providerLineHasAPIContext(line) {
			t.Fatalf("providerLineHasAPIContext(%q) = true, want false", line)
		}
	}
}

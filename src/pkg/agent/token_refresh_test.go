package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// syncedMinter is a race-safe fake for tests where the refresh loop mints
// from its own goroutine while the test polls.
type syncedMinter struct {
	mu    sync.Mutex
	calls int
}

func (s *syncedMinter) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil
}

func (s *syncedMinter) snapshotCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ---------------------------------------------------------------------------
// #4072: per-agent scoped token caches must keep refreshing for the whole
// lifetime of an agent session, and the refresh machinery must be reachable
// even when GitHub App auth is wired after boot.
// ---------------------------------------------------------------------------

func TestAgentTokenRefreshInterval_Default(t *testing.T) {
	t.Setenv(AgentTokenRefreshIntervalEnv, "")
	if got := agentTokenRefreshInterval(); got != defaultAgentTokenRefreshInterval {
		t.Errorf("expected default %v, got %v", defaultAgentTokenRefreshInterval, got)
	}
}

func TestAgentTokenRefreshInterval_EnvOverride(t *testing.T) {
	t.Setenv(AgentTokenRefreshIntervalEnv, "15m")
	if got := agentTokenRefreshInterval(); got != 15*time.Minute {
		t.Errorf("expected 15m from env override, got %v", got)
	}
}

func TestAgentTokenRefreshInterval_InvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		t.Setenv(AgentTokenRefreshIntervalEnv, bad)
		if got := agentTokenRefreshInterval(); got != defaultAgentTokenRefreshInterval {
			t.Errorf("env %q: expected default %v, got %v", bad, defaultAgentTokenRefreshInterval, got)
		}
	}
}

// TestRefreshAgentTokens_Exported proves the exported immediate-refresh entry
// point (used by the late App-auth wiring paths in main: heartbeat delivery,
// config API reinit, config reload) mints tokens for running agents.
func TestRefreshAgentTokens_Exported(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"runner": {Backend: "claude"},
		"idler":  {Backend: "claude"},
	}, discardLogger(), ProjectContext{ACMMLevel: 5})

	fm := &fakeMinter{}
	m.SetAppAuth(fm)
	m.mu.Lock()
	m.agents["runner"].State = StateRunning
	m.agents["runner"].UID = 2001
	m.agents["idler"].State = StateStopped
	m.agents["idler"].UID = 2002
	m.mu.Unlock()

	m.RefreshAgentTokens(context.Background())
	if fm.calls != 1 {
		t.Errorf("expected exactly the running agent to be refreshed (1 mint), got %d", fm.calls)
	}
}

// TestStartAgentTokenRefresh_TicksWithEnvInterval proves the loop actually
// rewrites running agents' caches on the (env-configured) interval — the
// production failure was this loop never being started, so token caches were
// written once at agent start and then rotted until gh 401'd.
func TestStartAgentTokenRefresh_TicksWithEnvInterval(t *testing.T) {
	t.Setenv(AgentTokenRefreshIntervalEnv, "10ms")

	m := NewManager(map[string]config.AgentConfig{"runner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	fm := &syncedMinter{}
	m.SetAppAuth(fm)
	m.mu.Lock()
	m.agents["runner"].State = StateRunning
	m.agents["runner"].UID = 2001
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.StartAgentTokenRefresh(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for fm.snapshotCalls() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("refresh loop never minted a token within 2s at a 10ms interval")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("StartAgentTokenRefresh did not return after cancel")
	}
}

func TestRefreshAgentTokenFor_Success(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"guide": {Backend: "claude"}}, discardLogger(), ProjectContext{ACMMLevel: 5})
	fm := &fakeMinter{}
	m.SetAppAuth(fm)
	m.mu.Lock()
	m.agents["guide"].UID = 2003
	m.mu.Unlock()

	if err := m.RefreshAgentTokenFor(context.Background(), "guide"); err != nil {
		t.Fatalf("RefreshAgentTokenFor: %v", err)
	}
	if fm.calls != 1 {
		t.Errorf("expected 1 mint, got %d", fm.calls)
	}
}

func TestRefreshAgentTokenFor_Errors(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"guide": {Backend: "claude"}}, discardLogger(), ProjectContext{})

	// No App auth wired.
	if err := m.RefreshAgentTokenFor(context.Background(), "guide"); err == nil {
		t.Error("expected error when no app auth is wired")
	}

	fm := &fakeMinter{}
	m.SetAppAuth(fm)

	// Unknown agent.
	if err := m.RefreshAgentTokenFor(context.Background(), "ghost"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected not-found error for unknown agent, got %v", err)
	}

	// No dedicated UID (zero value) — nothing to scope to.
	if err := m.RefreshAgentTokenFor(context.Background(), "guide"); err == nil || !strings.Contains(err.Error(), "no dedicated UID") {
		t.Errorf("expected no-UID error, got %v", err)
	}

	// Mint failure is wrapped.
	m.mu.Lock()
	m.agents["guide"].UID = 2004
	m.mu.Unlock()
	fm.fail = true
	if err := m.RefreshAgentTokenFor(context.Background(), "guide"); err == nil || !strings.Contains(err.Error(), "re-caching scoped token") {
		t.Errorf("expected wrapped mint error, got %v", err)
	}
}

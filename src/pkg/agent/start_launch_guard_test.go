package agent

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests cover the per-agent launch guard that lets Manager.Start run its
// NFS/exec launch work with m.mu RELEASED (so a stalled /data write or a hung
// MITM-proxy token mint during launch cannot block AllStatuses()/the heartbeat
// collect() and flap /api/livez). The guard serializes concurrent
// Start(sameName) that m.mu used to serialize implicitly.

// TestStart_LaunchGuardRejectsConcurrentSameAgent proves a second Start of an
// agent whose launch is already in progress is refused fast rather than racing
// the first launch's tmux setup and guarded-field writes.
func TestStart_LaunchGuardRejectsConcurrentSameAgent(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Simulate a launch already in flight (as if another goroutine is in
	// Phase 2/3 with m.mu released).
	m.mu.Lock()
	m.agents["cxa"].launching = true
	m.mu.Unlock()

	err := m.Start(context.Background(), "cxa")
	if err == nil {
		t.Fatal("expected an error when a launch is already in progress, got nil")
	}
	if got := err.Error(); got != "agent cxa launch already in progress" {
		t.Fatalf("unexpected error: %q, want the launch-already-in-progress guard error", got)
	}

	// The guard flag must be untouched by the rejected call (still owned by the
	// in-flight launch), not cleared by the loser's deferred clear.
	m.mu.RLock()
	stillLaunching := m.agents["cxa"].launching
	m.mu.RUnlock()
	if !stillLaunching {
		t.Error("rejected Start cleared the launching guard — it must leave the in-flight launch's guard intact")
	}
}

// TestStart_RunningAgentStillRejectedBeforeGuard proves the StateRunning check
// still precedes the guard (an already-running agent is rejected without ever
// touching launching), so the guard governs only genuine launch attempts.
func TestStart_RunningAgentStillRejectedBeforeGuard(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["cxa"].State = StateRunning
	m.mu.Unlock()

	err := m.Start(context.Background(), "cxa")
	if err == nil || err.Error() != "agent cxa already running" {
		t.Fatalf("expected already-running error, got %v", err)
	}

	m.mu.RLock()
	launching := m.agents["cxa"].launching
	m.mu.RUnlock()
	if launching {
		t.Error("already-running Start set the launching guard — it must return before claiming it")
	}
}

// TestStart_LaunchGuard_UnknownAgentReturnsError keeps the not-found path
// covered under the restructured Phase-1 critical section (it now unlocks
// explicitly). Named distinctly from manager_test.go's TestStart_UnknownAgentReturnsError.
func TestStart_LaunchGuard_UnknownAgentReturnsError(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	if err := m.Start(context.Background(), "nope"); err == nil {
		t.Fatal("expected an error for an unknown agent, got nil")
	}
}

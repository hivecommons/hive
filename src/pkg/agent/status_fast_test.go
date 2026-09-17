package agent

import (
	"testing"
	"time"
)

// A restart holds the global m.mu in write mode across the whole tmux relaunch
// (measured at 11.25s on a live spoke). Every settings dialog calls the agent
// config endpoint, whose first action was GetStatus -> m.mu.RLock(), so one
// agent relaunching froze the settings dialog for ALL agents (#7417).
// GetStatusFast must never wait on that lock.

func TestGetStatusFastDoesNotBlockOnHeldWriteLock(t *testing.T) {
	m := testManager(5)
	m.agents["scanner"] = &AgentProcess{
		Name:            "scanner",
		State:           StateRunning,
		BackendOverride: "copilot",
		ModelOverride:   "claude-opus-5",
		LastKickMessage: "kick one",
	}

	// Prime the snapshot the way a real dialog open would.
	if _, err := m.GetStatusFast("scanner"); err != nil {
		t.Fatalf("priming GetStatusFast: %v", err)
	}

	// Simulate the restart: hold the write lock the way restartWithReason does.
	released := make(chan struct{})
	held := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(held)
		<-released
		m.mu.Unlock()
	}()
	<-held
	defer close(released)

	done := make(chan *AgentProcess, 1)
	go func() {
		proc, err := m.GetStatusFast("scanner")
		if err != nil {
			done <- nil
			return
		}
		done <- proc
	}()

	select {
	case proc := <-done:
		if proc == nil {
			t.Fatal("GetStatusFast returned an error while the write lock was held; " +
				"it must fall back to the cached snapshot")
		}
		if proc.ModelOverride != "claude-opus-5" || proc.BackendOverride != "copilot" {
			t.Fatalf("stale snapshot lost display fields: backend=%q model=%q",
				proc.BackendOverride, proc.ModelOverride)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetStatusFast blocked while a restart held m.mu — this is the #7417 stall")
	}

	// GetStatus, by contrast, is expected to block. Prove the distinction so a
	// future refactor cannot quietly make GetStatusFast just an alias.
	blocked := make(chan struct{})
	go func() {
		_, _ = m.GetStatus("scanner")
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("GetStatus did not block on a held write lock; the test no longer " +
			"reproduces the condition GetStatusFast exists to survive")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestGetStatusFastReturnsErrorForUnknownAgentWhenLockIsFree(t *testing.T) {
	m := testManager(5)
	if _, err := m.GetStatusFast("nope"); err == nil {
		t.Fatal("expected an error for an unknown agent")
	}
}

// With no cached snapshot and the lock held, the caller must get an error
// rather than a zero-valued AgentProcess: handleAgentConfigGet guards every
// field use with err == nil and degrades to configured values, so an error is
// correct and a zero struct would render a fabricated config (#7405).
func TestGetStatusFastErrorsWhenUncachedAndLocked(t *testing.T) {
	m := testManager(5)
	m.agents["quality"] = &AgentProcess{Name: "quality", ModelOverride: "claude-opus-5"}

	released := make(chan struct{})
	held := make(chan struct{})
	go func() {
		m.mu.Lock()
		close(held)
		<-released
		m.mu.Unlock()
	}()
	<-held
	defer close(released)

	done := make(chan error, 1)
	go func() {
		_, err := m.GetStatusFast("quality")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when no snapshot has been cached yet")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetStatusFast blocked instead of returning the uncached error")
	}
}

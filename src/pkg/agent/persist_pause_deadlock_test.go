package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// These are the v4-idiom equivalents of v2's
// TestResume_PersistenceCallbackCanReenterManager (added with the #1980→#1988
// mutex re-entrancy fix). They pin the persistPauseCallback invocation
// contract: Pause and Resume must NEVER invoke the persistence callback while
// m.mu is held. The callback does config disk I/O and is allowed to re-enter
// the manager (the wired production callback persists to the PVC; a re-entrant
// callback — e.g. one that refreshes dashboard snapshots via AllStatuses — is
// within its contract). m.mu is a non-reentrant sync.RWMutex, so a locked
// invocation deadlocks the pause path and wedges every operation queued behind
// m.mu (heartbeat AllStatuses, SendKick, terminal ResolveAgent) — the same
// failure class as the mint-issuer deadlock fixed in ca5f0f00 (see
// mint_deadlock_test.go).
//
// Positive control: reverting the fix — re-inlining the callback invocation
// under m.mu in Resume (the old `m.persistPauseCallback(name, false)` between
// the Paused-field writes) or in Pause (the old call under the deferred
// unlock) — makes the matching test below fail at its timeout, because the
// callback's AllStatuses blocks on m.mu.RLock while the same goroutine holds
// m.mu.Lock.

// TestResume_PersistenceCallbackCanReenterManager asserts Resume invokes the
// persistence callback without holding m.mu: the callback re-enters the
// manager via AllStatuses (m.mu.RLock) and Resume must still return. The
// re-entrant call runs in a goroutine guarded by select+time.After so a
// regression fails the test instead of hanging CI.
func TestResume_PersistenceCallbackCanReenterManager(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	// Paused flags set, but State stays idle so Resume takes the
	// no-relaunch path (no tmux needed in a unit test).
	m.agents["worker"].Paused = true
	m.agents["worker"].Config.Paused = true

	callbackCalled := make(chan struct{}, 1)
	m.SetPersistPauseCallback(func(name string, paused bool) {
		if name != "worker" || paused {
			t.Errorf("persist callback = (%q, %v), want (worker, false)", name, paused)
		}
		_ = m.AllStatuses() // re-enter the manager: takes m.mu.RLock
		callbackCalled <- struct{}{}
	})

	done := make(chan error, 1)
	go func() {
		done <- m.Resume(context.Background(), "worker", "test", "test resume")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume() error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Resume() deadlocked while persistence callback re-entered manager " +
			"(persistPauseCallback invoked under m.mu — see its field docs)")
	}

	select {
	case <-callbackCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("resume persistence callback was not called")
	}
}

// TestPause_PersistenceCallbackCanReenterManager is the Pause-side twin: v4
// invoked the callback under Pause's deferred m.mu.Unlock too, so the same
// re-entrancy guarantee is pinned for the pause direction.
func TestPause_PersistenceCallbackCanReenterManager(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})

	callbackCalled := make(chan struct{}, 1)
	m.SetPersistPauseCallback(func(name string, paused bool) {
		if name != "worker" || !paused {
			t.Errorf("persist callback = (%q, %v), want (worker, true)", name, paused)
		}
		_ = m.AllStatuses() // re-enter the manager: takes m.mu.RLock
		callbackCalled <- struct{}{}
	})

	done := make(chan error, 1)
	go func() {
		// Agent is idle (not StateRunning), so Pause skips the tmux
		// interrupt and exercises only the state flip + callback.
		done <- m.Pause("worker", "test", "test pause")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pause() error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Pause() deadlocked while persistence callback re-entered manager " +
			"(persistPauseCallback invoked under m.mu — see its field docs)")
	}

	select {
	case <-callbackCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("pause persistence callback was not called")
	}
}

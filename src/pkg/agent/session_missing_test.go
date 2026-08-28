package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

func sessionMissingManager(agents map[string]*AgentProcess) *Manager {
	return &Manager{
		agents:   agents,
		idToName: make(map[string]string),
		logger:   slog.Default(),
	}
}

// stubTmuxProbe replaces the tmux session probe and records how many times it
// was consulted, restoring the real one afterwards.
//
// Every test in this file installs it. Without it, a SessionMissing call that
// reached the probe would run a REAL `tmux has-session` against whatever tmux
// server happens to exist on the machine, making the result depend on the
// environment instead of the code.
func stubTmuxProbe(t *testing.T, exists bool) *int {
	t.Helper()
	calls := 0
	old := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool {
		calls++
		return exists
	}
	t.Cleanup(func() { tmuxSessionExists = old })
	return &calls
}

// SessionMissing reports the "zombie" condition: the manager believes an agent
// is running but its tmux session is gone. The hub turns a true here into an
// operator-visible alert, so every case that is NOT a zombie has to return
// false without consulting tmux at all.
//
// The paused case is the one that matters most. A paused agent legitimately
// has no session — the operator stopped it — and reporting that as a zombie
// would light the inactivity facet on every deliberately parked agent, which
// is precisely the "facet that lights up constantly gets ignored" failure the
// classifier is written to avoid.
func TestSessionMissingIsFalseWithoutConsultingTmux(t *testing.T) {
	cases := []struct {
		name  string
		agent *AgentProcess
	}{
		{
			// Paused: no session is expected, so its absence is not a fault.
			name:  "paused agent",
			agent: &AgentProcess{Name: "a", State: StateRunning, Paused: true},
		},
		{
			// Paused before Start() ran, so State still holds the old value
			// while the Paused flag is already set.
			name:  "paused flag set while state lags",
			agent: &AgentProcess{Name: "a", State: StateRunning, Paused: true},
		},
		{
			name:  "stopped agent",
			agent: &AgentProcess{Name: "a", State: StateStopped},
		},
		{
			name:  "failed agent",
			agent: &AgentProcess{Name: "a", State: StateFailed},
		},
		{
			// Never started: a missing session is the correct state, not a
			// zombie.
			name:  "idle agent",
			agent: &AgentProcess{Name: "a", State: StateIdle},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The stub reports the session as ABSENT. If one of these cases
			// were to fall through to the probe it would therefore return
			// true, so a lost early return fails loudly rather than depending
			// on whatever tmux is running on the machine.
			calls := stubTmuxProbe(t, false)
			m := sessionMissingManager(map[string]*AgentProcess{"a": tc.agent})

			if m.SessionMissing("a") {
				t.Errorf("SessionMissing = true for %s; only a RUNNING, unpaused agent can be a zombie", tc.name)
			}
			if *calls != 0 {
				t.Errorf("%s consulted tmux %d times; it must short-circuit before the probe", tc.name, *calls)
			}
		})
	}
}

// The actual zombie: running, not paused, and the session really is gone.
// This is the one case that must return true, and it is what makes the
// negative cases above meaningful.
func TestSessionMissingDetectsARealZombie(t *testing.T) {
	calls := stubTmuxProbe(t, false) // session absent
	m := sessionMissingManager(map[string]*AgentProcess{
		"a": {Name: "a", State: StateRunning},
	})

	if !m.SessionMissing("a") {
		t.Error("a running, unpaused agent whose tmux session is gone is a zombie")
	}
	if *calls != 1 {
		t.Errorf("probe consulted %d times, want exactly 1", *calls)
	}
}

// A healthy running agent whose session is present is not a zombie.
func TestSessionMissingIsFalseWhenSessionIsPresent(t *testing.T) {
	stubTmuxProbe(t, true) // session exists
	m := sessionMissingManager(map[string]*AgentProcess{
		"a": {Name: "a", State: StateRunning},
	})

	if m.SessionMissing("a") {
		t.Error("a running agent with a live session must not be reported as a zombie")
	}
}

// An agent the manager does not know about is not a zombie — it is a lookup
// miss. Returning true would alert on every stale name the hub still has in a
// cached summary.
func TestSessionMissingIsFalseForUnknownAgent(t *testing.T) {
	calls := stubTmuxProbe(t, false)
	m := sessionMissingManager(map[string]*AgentProcess{})

	if m.SessionMissing("does-not-exist") {
		t.Error("an unknown agent must not be reported as a missing session")
	}
	if *calls != 0 {
		t.Errorf("a lookup miss consulted tmux %d times; it must short-circuit", *calls)
	}
}

// The lock must be released before the tmux exec. Holding a manager lock
// across a subprocess is how the startup path has deadlocked before, so this
// asserts the lock is actually free once the call returns — including on the
// early-return paths, where an unbalanced RUnlock would be easy to introduce.
func TestSessionMissingReleasesTheLock(t *testing.T) {
	stubTmuxProbe(t, false)
	m := sessionMissingManager(map[string]*AgentProcess{
		"paused": {Name: "paused", State: StateRunning, Paused: true},
	})

	m.SessionMissing("paused")
	m.SessionMissing("unknown")

	// A write lock can only be taken if every reader released. If an early
	// return leaked an RLock this blocks forever and the test times out.
	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.mu.Unlock() //nolint:staticcheck // SA2001: the empty critical section IS the test — it proves no reader lock leaked (see comment above).
		close(done)
	}()
	<-done
}

// SendKick's guards run before anything touches tmux. Each produces a distinct
// operator-facing message: the whole point of notRunningReason is that "paused"
// and "failed to start" must not read identically during an incident.
func TestSendKickRefusesNonRunningAgentsWithASpecificReason(t *testing.T) {
	cases := []struct {
		name  string
		agent *AgentProcess
		want  string
		deny  string
	}{
		{
			name: "paused agent names the trigger",
			agent: &AgentProcess{
				Name: "a", State: StatePaused, Paused: true,
				PausedTrigger: "dashboard-api",
			},
			want: "paused",
			deny: "failed to start",
		},
		{
			name:  "failed agent reports the launch error",
			agent: &AgentProcess{Name: "a", State: StateFailed, LastError: "boom"},
			want:  "failed to start",
			deny:  "paused",
		},
		{
			name:  "stopped agent",
			agent: &AgentProcess{Name: "a", State: StateStopped},
			want:  "stopped",
			deny:  "failed to start",
		},
		{
			name:  "never started",
			agent: &AgentProcess{Name: "a", State: StateIdle},
			want:  "not been started",
			deny:  "failed to start",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubTmuxProbe(t, true)
			m := sessionMissingManager(map[string]*AgentProcess{"a": tc.agent})

			err := m.SendKick("a", "hello")
			if err == nil {
				t.Fatal("a non-running agent must not be kickable")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain the state (want %q)", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.deny) {
				t.Errorf("error %q reintroduces the ambiguity %q", err, tc.deny)
			}
			if *calls != 0 {
				t.Errorf("refused before running, yet consulted tmux %d times", *calls)
			}
		})
	}
}

func TestSendKickRejectsUnknownAgent(t *testing.T) {
	calls := stubTmuxProbe(t, true)
	m := sessionMissingManager(map[string]*AgentProcess{})

	err := m.SendKick("ghost", "hello")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
	if *calls != 0 {
		t.Errorf("a lookup miss consulted tmux %d times", *calls)
	}
}

// A running agent whose tmux session has vanished must fail with the session
// error rather than proceeding to type into nothing.
func TestSendKickRejectsMissingTmuxSession(t *testing.T) {
	stubTmuxProbe(t, false) // session gone
	m := sessionMissingManager(map[string]*AgentProcess{
		"a": {Name: "a", State: StateRunning, tmuxSession: "hive-a"},
	})

	err := m.SendKick("a", "hello")
	if err == nil || !strings.Contains(err.Error(), "tmux session") {
		t.Fatalf("expected a missing-session error, got %v", err)
	}
	if !strings.Contains(err.Error(), "hive-a") {
		t.Errorf("error %q does not name the session an operator would look for", err)
	}
}

// RestartThenSendKick must not silently swallow a failed restart: reporting
// success for a kick that never landed is worse than reporting the failure.
func TestRestartThenSendKickFailsWhenAgentUnknown(t *testing.T) {
	stubTmuxProbe(t, true)
	m := sessionMissingManager(map[string]*AgentProcess{})

	err := m.RestartThenSendKick(context.Background(), "ghost", "hello")
	if err == nil {
		t.Fatal("restarting an unknown agent must be an error")
	}
}

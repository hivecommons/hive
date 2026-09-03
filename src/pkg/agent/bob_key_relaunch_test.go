package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// newRelaunchManager builds a bare Manager with the given agents registered.
// It deliberately does not touch tmux: these tests cover the selection rules
// and the key gate, which is where the risk lives.
func newRelaunchManager(agents map[string]*AgentProcess) *Manager {
	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   discardLogger(),
	}
	for name, a := range agents {
		a.Name = name
		m.agents[name] = a
	}
	return m
}

// parkedBobAgent returns an agent in exactly the state launchInTmux leaves a
// bob agent in when no API key is configured.
func parkedBobAgent() *AgentProcess {
	return &AgentProcess{
		Config:         config.AgentConfig{Backend: bobBackend},
		State:          StateFailed,
		awaitingBobKey: true,
	}
}

// TestLaunchParksBobAgentAwaitingKey pins the marker half of the fix: the
// missing-key branch must set awaitingBobKey, because StateFailed alone is
// ambiguous (missing binary, copilot auth timeout, hung diagnostic).
func TestLaunchParksBobAgentAwaitingKey(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	m.SetBobAPIKeyResolver(func() string { return "" })

	agent := &AgentProcess{
		Name:   "scanner",
		Config: config.AgentConfig{Backend: bobBackend},
	}
	// launchInTmux bails on the key check before any tmux work when the
	// backend binary resolves; if bob is absent it bails even earlier, in
	// which case the marker must stay false rather than be wrongly set.
	if _, err := backendBinary(bobBackend); err != nil {
		t.Skipf("bob binary not present in this environment: %v", err)
	}
	if err := m.launchInTmux(context.Background(), agent); err != nil {
		t.Fatalf("launchInTmux returned error, want nil so one agent never aborts the fleet: %v", err)
	}
	if agent.State != StateFailed {
		t.Errorf("State = %q, want %q", agent.State, StateFailed)
	}
	if !agent.awaitingBobKey {
		t.Error("awaitingBobKey = false, want true — the key-save relaunch cannot find this agent without the marker")
	}
}

// TestBobAgentsAwaitingKeySelection is the core of the change: exactly the
// agents parked for a missing key are eligible, and nothing else ever is.
func TestBobAgentsAwaitingKeySelection(t *testing.T) {
	tests := []struct {
		name  string
		agent *AgentProcess
		want  bool
	}{
		{
			name:  "bob parked for missing key is selected",
			agent: parkedBobAgent(),
			want:  true,
		},
		{
			name: "healthy running bob agent is not disturbed",
			agent: &AgentProcess{
				Config: config.AgentConfig{Backend: bobBackend},
				State:  StateRunning,
			},
			want: false,
		},
		{
			name: "running bob agent with a stale marker is still not disturbed",
			agent: &AgentProcess{
				Config:         config.AgentConfig{Backend: bobBackend},
				State:          StateRunning,
				awaitingBobKey: true,
			},
			want: false,
		},
		{
			name: "operator-paused bob agent is not resumed by a key save",
			agent: &AgentProcess{
				Config:         config.AgentConfig{Backend: bobBackend, Paused: true},
				State:          StatePaused,
				Paused:         true,
				PausedTrigger:  "dashboard-api",
				awaitingBobKey: true,
			},
			want: false,
		},
		{
			name: "claude agent is never touched",
			agent: &AgentProcess{
				Config: config.AgentConfig{Backend: "claude"},
				State:  StateFailed,
			},
			want: false,
		},
		{
			name: "failed claude agent with a spurious marker is rejected by the backend check",
			agent: &AgentProcess{
				Config:         config.AgentConfig{Backend: "claude"},
				State:          StateFailed,
				awaitingBobKey: true,
			},
			want: false,
		},
		{
			name: "bob agent failed for another reason is not selected",
			agent: &AgentProcess{
				Config:    config.AgentConfig{Backend: bobBackend},
				State:     StateFailed,
				LastError: "backend binary not found",
			},
			want: false,
		},
		{
			name: "agent overridden away from bob after parking is rejected",
			agent: &AgentProcess{
				Config:          config.AgentConfig{Backend: bobBackend},
				BackendOverride: "claude",
				State:           StateFailed,
				awaitingBobKey:  true,
			},
			want: false,
		},
		{
			name: "agent overridden TO bob and parked is selected",
			agent: &AgentProcess{
				Config:          config.AgentConfig{Backend: "claude"},
				BackendOverride: bobBackend,
				State:           StateFailed,
				awaitingBobKey:  true,
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newRelaunchManager(map[string]*AgentProcess{"subject": tc.agent})
			got := m.bobAgentsAwaitingKey()
			if tc.want && len(got) != 1 {
				t.Fatalf("bobAgentsAwaitingKey() = %v, want [subject]", got)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("bobAgentsAwaitingKey() = %v, want no candidates", got)
			}
		})
	}
}

// TestRelaunchBobAgentsAwaitingKeyRequiresResolvedKey covers the clear path and
// the not-yet-configured path: with no key, relaunching would just re-park
// every agent on the same branch, so it must be a no-op.
func TestRelaunchBobAgentsAwaitingKeyRequiresResolvedKey(t *testing.T) {
	tests := []struct {
		name    string
		inject  bool
		key     string
		wantNil bool
	}{
		{name: "no resolver injected", inject: false, wantNil: true},
		{name: "key cleared", inject: true, key: "", wantNil: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newRelaunchManager(map[string]*AgentProcess{"scanner": parkedBobAgent()})
			if tc.inject {
				key := tc.key
				m.SetBobAPIKeyResolver(func() string { return key })
			}
			if got := m.RelaunchBobAgentsAwaitingKey(context.Background()); len(got) != 0 {
				t.Errorf("RelaunchBobAgentsAwaitingKey() = %v, want none — clearing a key must never launch anything", got)
			}
			// The agent must remain parked and eligible for a later real save.
			if !m.agents["scanner"].awaitingBobKey {
				t.Error("awaitingBobKey was cleared without a launch; a later key save would miss this agent")
			}
		})
	}
}

// TestRelaunchIsIdempotentOnDoubleSave pins that saving a key twice cannot
// double-launch. The first pass clears the marker (launchInTmux does this on
// every launch attempt), so the second pass finds no candidates.
func TestRelaunchIsIdempotentOnDoubleSave(t *testing.T) {
	agent := parkedBobAgent()
	m := newRelaunchManager(map[string]*AgentProcess{"scanner": agent})
	m.SetBobAPIKeyResolver(func() string { return "bob-key-abc" })

	if got := m.bobAgentsAwaitingKey(); len(got) != 1 {
		t.Fatalf("first save: got %v candidates, want 1", got)
	}

	// Simulate what launchInTmux does at the top of every launch attempt.
	agent.awaitingBobKey = false
	agent.State = StateRunning

	if got := m.bobAgentsAwaitingKey(); len(got) != 0 {
		t.Errorf("second save: got %v, want none — a double save must not double-launch", got)
	}
}

// TestRelaunchNoDeadlockUnderConcurrency exercises the lock discipline that
// incident #1980→#1988 turned into a hard rule: m.mu is a NON-reentrant
// RWMutex, and Start takes m.mu.Lock() itself, so candidate collection must
// release the read lock before any launch. This test hammers the selection
// path against concurrent writers holding the write lock; a re-entrant
// acquisition anywhere in the path hangs here instead of in production.
func TestRelaunchNoDeadlockUnderConcurrency(t *testing.T) {
	const (
		// concurrentWorkers is the number of goroutines per role (readers and
		// writers). Enough to interleave lock acquisitions reliably without
		// making the test slow.
		concurrentWorkers = 8
		// iterationsPerWorker is how many times each goroutine takes the lock.
		iterationsPerWorker = 50
		// deadlockTimeout bounds the run. The contended work below finishes in
		// milliseconds, so anything approaching this means the lock is held,
		// not that the machine is slow.
		deadlockTimeout = 30 * time.Second
	)

	agents := map[string]*AgentProcess{
		"parked":  parkedBobAgent(),
		"running": {Config: config.AgentConfig{Backend: bobBackend}, State: StateRunning},
		"claude":  {Config: config.AgentConfig{Backend: "claude"}, State: StateFailed},
	}
	m := newRelaunchManager(agents)
	m.SetBobAPIKeyResolver(func() string { return "bob-key-abc" })

	var wg sync.WaitGroup
	for i := 0; i < concurrentWorkers; i++ {
		wg.Add(2)
		// Reader: the selection path under test.
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerWorker; j++ {
				m.bobAgentsAwaitingKey()
			}
		}()
		// Writer: contends for the write lock the way Start/Pause do.
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerWorker; j++ {
				m.mu.Lock()
				m.agents["running"].RestartCount++
				m.mu.Unlock()
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(deadlockTimeout):
		t.Fatal("deadlock: selection path did not complete under lock contention")
	}
}

// TestKillSessionForRelaunchClearsRunningState pins the reason the relaunch
// recreates the tmux session instead of just re-typing the launch command.
//
// BOBSHELL_API_KEY is Secret, so buildEnvPrefix omits it from the typed command
// line and tmux set-environment is its only delivery path — and that is
// inherited only by shells created afterwards. The pane's bash predates the
// key, and reapAgentCLI kills the bob CLI but not that bash, so relaunching
// into the same session hands bob a shell with no key (verified live on
// hosted-available-vllmd-01: key present in the tmux session env, absent from
// every bob /proc/<pid>/environ).
//
// The state reset matters too: Start refuses to launch an agent it thinks is
// running, so leaving StateRunning after killing its session would make the
// relaunch a silent no-op.
func TestKillSessionForRelaunchClearsRunningState(t *testing.T) {
	tests := []struct {
		name  string
		state ProcessState
		want  ProcessState
	}{
		{name: "running is reset so Start will relaunch", state: StateRunning, want: StateStopped},
		{name: "failed is left alone", state: StateFailed, want: StateFailed},
		{name: "stopped is left alone", state: StateStopped, want: StateStopped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := parkedBobAgent()
			agent.State = tc.state
			// No tmuxSession set: tmuxCmd runs a kill-session that simply
			// fails, which the helper ignores by design ("already gone" is a
			// satisfied precondition, not an error).
			m := newRelaunchManager(map[string]*AgentProcess{"scanner": agent})
			if err := m.killSessionForRelaunch("scanner"); err != nil {
				t.Fatalf("killSessionForRelaunch() error = %v, want nil", err)
			}
			if agent.State != tc.want {
				t.Errorf("State = %q, want %q", agent.State, tc.want)
			}
		})
	}
}

// TestKillSessionForRelaunchUnknownAgent ensures a vanished agent surfaces an
// error rather than panicking the key-save request.
func TestKillSessionForRelaunchUnknownAgent(t *testing.T) {
	m := newRelaunchManager(nil)
	if err := m.killSessionForRelaunch("ghost"); err == nil {
		t.Error("killSessionForRelaunch() on an unknown agent = nil, want an error")
	}
}

// TestRelaunchHandlesEmptyFleet guards the nil/empty cases the dashboard can
// hit before any agent is registered.
func TestRelaunchHandlesEmptyFleet(t *testing.T) {
	m := newRelaunchManager(nil)
	m.SetBobAPIKeyResolver(func() string { return "bob-key-abc" })
	if got := m.RelaunchBobAgentsAwaitingKey(context.Background()); len(got) != 0 {
		t.Errorf("RelaunchBobAgentsAwaitingKey() on empty fleet = %v, want none", got)
	}
}

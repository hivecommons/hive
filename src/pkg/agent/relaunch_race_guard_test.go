package agent

// Tests for the post-mint re-verify guards added with the #3962 relaunch-mint
// fix (#3967): every relaunch path releases m.mu for the outbound mint, so a
// concurrent Stop/Remove (or a racing launch) can change the world before the
// path re-locks. Each guard must be honoured — relaunching a REMOVED agent
// would resurrect a ghost tmux session no map entry owns, and double-launching
// after a lost race would put two CLIs in one pane.
//
// The blocking minter makes the race deterministic: WriteAgentToken parks in
// the unlocked mint window until the test has mutated the manager, so the
// re-verify always observes the mutation. No sleeps, no timing guesses.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// blockingMinter parks WriteAgentToken until released, signalling entry so the
// test knows the relaunch path is inside its UNLOCKED mint window.
type blockingMinter struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func newBlockingMinter() *blockingMinter {
	return &blockingMinter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingMinter) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// raceGuardManager builds a manager with one claude agent carrying a per-agent
// UID (the mint step is gated on UID>0) and the blocking minter attached.
func raceGuardManager(t *testing.T) (*Manager, *blockingMinter) {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["worker"].UID = 2101
	m.mu.Unlock()
	bm := newBlockingMinter()
	m.SetAppAuth(bm)
	return m, bm
}

// waitEntered fails the test if the relaunch path never reaches its mint
// window — which would mean the mint step itself regressed (see
// relaunch_mint_test.go for the dedicated tests on that).
func waitEntered(t *testing.T, bm *blockingMinter, what string) {
	t.Helper()
	select {
	case <-bm.entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never reached the unlocked mint window", what)
	}
}

// TestRestart_RelaunchesRunningAgent is the regression pin for the guard's
// FORM: the lost-race check must compare launchGen, not agent.State. Restart
// is routinely called ON a running agent (dashboard restart, SendKick's
// crashed-CLI recovery) and nothing on the restart path clears the
// pre-restart StateRunning — so a `State == StateRunning` guard reads the
// agent's OWN old state as "another path won the launch race" and returns
// nil AFTER killing the CLI and its session: every restart of a running
// agent becomes a silent kill-without-relaunch.
func TestRestart_RelaunchesRunningAgent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["worker"].State = StateRunning
	m.mu.Unlock()

	if err := m.Restart(context.Background(), "worker"); err != nil {
		t.Fatalf("Restart(running agent): %v", err)
	}
	defer cleanupAgent(t, m, "worker")

	m.mu.RLock()
	agent := m.agents["worker"]
	relaunched := agent.HasLaunched
	gen := agent.launchGen
	state := agent.State
	m.mu.RUnlock()
	if !relaunched || gen != 1 {
		t.Fatalf("HasLaunched=%v launchGen=%d — Restart of a RUNNING agent killed the CLI and never relaunched it (the launch-race guard misread the agent's own pre-restart state)", relaunched, gen)
	}
	if state != StateRunning {
		t.Errorf("State = %s after restart, want %s", state, StateRunning)
	}
}

// TestRestart_AgentRemovedDuringMintWindowFails: Restart must refuse to
// relaunch an agent that was removed while the mint had m.mu released. Before
// the guard, launchInTmux would run for a map-orphaned *AgentProcess.
func TestRestart_AgentRemovedDuringMintWindowFails(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, bm := raceGuardManager(t)

	errCh := make(chan error, 1)
	go func() { errCh <- m.Restart(context.Background(), "worker") }()
	waitEntered(t, bm, "Restart")

	// The concurrent removal the guard exists for.
	m.mu.Lock()
	agent := m.agents["worker"]
	delete(m.agents, "worker")
	m.mu.Unlock()
	close(bm.release)

	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "removed during restart") {
		t.Fatalf("Restart err = %v, want 'removed during restart'", err)
	}
	// The removed agent must NOT have been relaunched.
	m.mu.Lock()
	launched := agent.State == StateRunning
	m.mu.Unlock()
	if launched {
		t.Error("removed agent was relaunched anyway — the re-verify guard did not stop launchInTmux")
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
}

// TestResume_AgentRemovedDuringMintWindowFails: the same guard on the Resume
// relaunch path (paused → relaunch), which minted nothing at all before #3962
// and therefore never had to re-verify.
func TestResume_AgentRemovedDuringMintWindowFails(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, bm := raceGuardManager(t)
	m.mu.Lock()
	m.agents["worker"].Paused = true
	m.agents["worker"].State = StatePaused
	m.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- m.Resume(context.Background(), "worker", "test", "race guard") }()
	waitEntered(t, bm, "Resume")

	m.mu.Lock()
	agent := m.agents["worker"]
	delete(m.agents, "worker")
	m.mu.Unlock()
	close(bm.release)

	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "removed during resume") {
		t.Fatalf("Resume err = %v, want 'removed during resume'", err)
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
}

// TestResume_LaunchRaceLostReturnsWithoutSecondLaunch: when another path wins
// the launch race during Resume's mint window (agent already StateRunning on
// re-lock), Resume must return success WITHOUT launching a second CLI into the
// same pane.
func TestResume_LaunchRaceLostReturnsWithoutSecondLaunch(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, bm := raceGuardManager(t)
	m.mu.Lock()
	m.agents["worker"].Paused = true
	m.agents["worker"].State = StatePaused
	m.mu.Unlock()

	errCh := make(chan error, 1)
	go func() { errCh <- m.Resume(context.Background(), "worker", "test", "race guard") }()
	waitEntered(t, bm, "Resume")

	// Simulate the racing path having already launched this agent.
	m.mu.Lock()
	agent := m.agents["worker"]
	agent.State = StateRunning
	m.mu.Unlock()
	close(bm.release)

	if err := <-errCh; err != nil {
		t.Fatalf("Resume after lost launch race err = %v, want nil (the other path owns the launch)", err)
	}
	// launchInTmux marks HasLaunched; the loser must not have gone through it.
	m.mu.Lock()
	hasLaunched := agent.HasLaunched
	m.mu.Unlock()
	if hasLaunched {
		t.Error("Resume launched a second CLI after losing the launch race")
	}
	cleanupAgent(t, m, "worker")
}

// TestRestartWithBootstrap_AgentRemovedDuringUnlockedWindowFails: the same
// removed-agent guard on RestartWithBootstrap, whose unlocked window is its
// pre-existing session-ready sleep. RestartCount is incremented under the
// FIRST lock, so observing it (under the lock) proves the goroutine has
// released m.mu and is inside the unlocked window — the removal below then
// deterministically lands before the re-lock.
func TestRestartWithBootstrap_AgentRemovedDuringUnlockedWindowFails(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})

	errCh := make(chan error, 1)
	go func() { errCh <- m.RestartWithBootstrap(context.Background(), "worker", "bootstrap prompt") }()

	deadline := time.Now().Add(20 * time.Second)
	var agent *AgentProcess
	for {
		if time.Now().After(deadline) {
			t.Fatal("RestartWithBootstrap never entered its unlocked window")
		}
		m.mu.Lock()
		a, ok := m.agents["worker"]
		if ok && a.RestartCount == 1 {
			agent = a
			delete(m.agents, "worker")
			m.mu.Unlock()
			break
		}
		m.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "removed during restart") {
		t.Fatalf("RestartWithBootstrap err = %v, want 'removed during restart'", err)
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
}

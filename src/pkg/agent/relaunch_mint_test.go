package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin the #3962 fix: EVERY path that (re)launches an agent —
// Start, Resume, Restart, RestartWithBootstrap — must mint the per-agent
// scoped GitHub token first. Before the fix only Start minted, so an agent
// relaunched via Resume/Restart ran with an empty token cache (gh + git push
// dead) until the whole hive process restarted.
//
// The minter below doubles as a lock-hygiene probe: WriteAgentToken re-enters
// the manager via AllStatuses (m.mu.RLock). The mint makes an outbound call
// that production incidents showed can hang, so it MUST run with m.mu
// released — if a relaunch path regresses to minting under m.mu.Lock, the
// re-entry self-deadlocks and the test fails at its timeout instead of
// hanging CI (same technique as persist_pause_deadlock_test.go).
//
// Positive control: the fresh-launch test (Start) pins that the original mint
// path is unchanged, so a failure in the relaunch tests can only mean the
// relaunch paths lost their mint step, not that minting broke globally.

// relaunchProbeMinter records WriteAgentToken calls and re-enters the manager
// to prove m.mu is not held during the mint.
type relaunchProbeMinter struct {
	m  *Manager
	mu sync.Mutex

	agents []string
	uids   []int
	tiers  []string
}

func (p *relaunchProbeMinter) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	// Re-enter the manager: deadlocks (→ test timeout) if the caller holds m.mu.
	_ = p.m.AllStatuses()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents = append(p.agents, agentName)
	p.uids = append(p.uids, agentUID)
	p.tiers = append(p.tiers, tier)
	return nil
}

func (p *relaunchProbeMinter) calls() ([]string, []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.agents...), append([]int(nil), p.uids...)
}

// installSuExecStub writes a passthrough su-exec into the stub bin dir already
// on PATH (see TestMain), so UID>0 tmux commands work on hosts without a real
// su-exec. It does not mutate PATH (avoids racing leaked launch goroutines'
// env reads). The relaunch-mint tests need UID>0 because the mint step is
// gated on a real per-agent UID.
func installSuExecStub(t *testing.T) {
	t.Helper()
	p := filepath.Join(stubBinDir, "su-exec")
	script := "#!/bin/sh\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("writing su-exec stub: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(p) })
}

// relaunchMintManager builds a manager with one claude-backed agent carrying a
// per-agent UID and the probe minter attached.
func relaunchMintManager(t *testing.T, name string) (*Manager, *relaunchProbeMinter) {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		name: makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents[name].UID = 2101
	m.mu.Unlock()
	pm := &relaunchProbeMinter{m: m}
	m.SetAppAuth(pm)
	return m, pm
}

// runWithDeadline runs fn on a goroutine and fails the test if it does not
// return within d — the deadlock signature of a mint re-inlined under m.mu.
func runWithDeadline(t *testing.T, d time.Duration, what string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s error: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s did not return — mint step likely runs under m.mu (see mintAgentTokenUnlocked docs)", what)
	}
}

func assertMinted(t *testing.T, pm *relaunchProbeMinter, agent string, uid, wantCalls int) {
	t.Helper()
	agents, uids := pm.calls()
	if len(agents) != wantCalls {
		t.Fatalf("WriteAgentToken calls = %d (%v), want %d", len(agents), agents, wantCalls)
	}
	for i := range agents {
		if agents[i] != agent || uids[i] != uid {
			t.Errorf("mint call %d = (%q, uid %d), want (%q, uid %d)", i, agents[i], uids[i], agent, uid)
		}
	}
}

// TestStart_MintsPerAgentToken is the positive control: a FRESH launch minted
// before the fix and must keep minting exactly once, with m.mu released.
func TestStart_MintsPerAgentToken(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, pm := relaunchMintManager(t, "worker")
	runWithDeadline(t, 30*time.Second, "Start", func() error {
		return m.Start(context.Background(), "worker")
	})
	defer cleanupAgent(t, m, "worker")
	assertMinted(t, pm, "worker", 2101, 1)
}

// TestResume_MintsPerAgentToken: an agent relaunched from StatePaused via
// Resume must get a token minted (before the fix: zero mint calls — the
// resumed agent's cache file stayed 0 bytes forever).
func TestResume_MintsPerAgentToken(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, pm := relaunchMintManager(t, "worker")
	m.mu.Lock()
	m.agents["worker"].Paused = true
	m.agents["worker"].State = StatePaused
	m.mu.Unlock()

	runWithDeadline(t, 30*time.Second, "Resume", func() error {
		return m.Resume(context.Background(), "worker", "dashboard-api", "manual resume")
	})
	defer cleanupAgent(t, m, "worker")

	assertMinted(t, pm, "worker", 2101, 1)
	if ap, _ := m.GetStatus("worker"); ap == nil || ap.State != StateRunning {
		t.Errorf("resumed agent should be running after relaunch")
	}
}

// TestRestart_MintsPerAgentToken: Restart relaunches via launchInTmux directly
// and must mint in its unlocked window.
func TestRestart_MintsPerAgentToken(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, pm := relaunchMintManager(t, "worker")
	runWithDeadline(t, 30*time.Second, "Restart", func() error {
		return m.Restart(context.Background(), "worker")
	})
	defer cleanupAgent(t, m, "worker")
	assertMinted(t, pm, "worker", 2101, 1)
}

// TestRestartWithBootstrap_MintsPerAgentToken: same guarantee for the
// bootstrap-override restart path (it already had an unlocked window for its
// session-ready sleep; the mint now rides it).
func TestRestartWithBootstrap_MintsPerAgentToken(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, pm := relaunchMintManager(t, "worker")
	runWithDeadline(t, 30*time.Second, "RestartWithBootstrap", func() error {
		return m.RestartWithBootstrap(context.Background(), "worker", "bootstrap prompt")
	})
	defer cleanupAgent(t, m, "worker")
	assertMinted(t, pm, "worker", 2101, 1)
}

// TestRestart_PausedAgentDoesNotMint: a restart that PRESERVES the paused
// state performs no relaunch, so it must not mint either — the mint belongs
// to the relaunch, not to the restart request.
func TestRestart_PausedAgentDoesNotMint(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	installSuExecStub(t)
	m, pm := relaunchMintManager(t, "worker")
	m.mu.Lock()
	m.agents["worker"].Paused = true
	m.mu.Unlock()
	runWithDeadline(t, 30*time.Second, "Restart(paused)", func() error {
		return m.Restart(context.Background(), "worker")
	})
	defer cleanupAgent(t, m, "worker")
	if agents, _ := pm.calls(); len(agents) != 0 {
		t.Errorf("paused restart minted %v, want no mint (no relaunch happened)", agents)
	}
}

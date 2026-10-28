package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/internal/testutil"
)

type recordingSharedSessionPool struct {
	mu        sync.Mutex
	acquired  []string
	released  []string
	renewed   []string
	acquireFn func(ctx context.Context, agent string) (string, error)
	renewFn   func(ctx context.Context, id string) error
}

func (p *recordingSharedSessionPool) Acquire(ctx context.Context, agent string) (string, error) {
	p.mu.Lock()
	p.acquired = append(p.acquired, agent)
	fn := p.acquireFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, agent)
	}
	return "lease-" + agent, nil
}

func (p *recordingSharedSessionPool) Renew(ctx context.Context, id string) error {
	p.mu.Lock()
	p.renewed = append(p.renewed, id)
	fn := p.renewFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}

func (p *recordingSharedSessionPool) acquiredNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.acquired...)
}

func (p *recordingSharedSessionPool) Release(_ context.Context, id string) error {
	p.mu.Lock()
	p.released = append(p.released, id)
	p.mu.Unlock()
	return nil
}

func TestSharedPoolEvictionStopsCLIAndReleasesLease(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateRunning
	a.poolLeaseID = "lease-1"

	m.evictSharedPoolSession("scanner", "lease-1")

	if a.State != StateStopped {
		t.Fatalf("agent state = %q, want stopped", a.State)
	}
	if a.poolLeaseID != "" {
		t.Fatalf("agent retained lease %q", a.poolLeaseID)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-1" {
		t.Fatalf("released leases = %v, want [lease-1]", pool.released)
	}
}

func TestSharedPoolStopReleasesLease(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateRunning
	a.poolLeaseID = "lease-stop"

	if err := m.Stop("scanner"); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if a.State != StateStopped {
		t.Fatalf("agent state = %q, want stopped", a.State)
	}
	if a.poolLeaseID != "" {
		t.Fatalf("agent retained lease %q", a.poolLeaseID)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-stop" {
		t.Fatalf("released leases = %v, want [lease-stop]", pool.released)
	}
}

func TestSharedPoolKickOutcomeEvictsSession(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.LastKick = &now
	a.poolLeaseID = "lease-outcome"

	outcome := KickOutcome{
		Kind:   KickOutcomeEnded,
		At:     now,
		KickAt: now,
	}
	a.KickOutcome = outcome

	m.notifyKickOutcome("scanner", outcome)

	testutil.EventuallyEvery(t, 2*time.Second, 10*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return a.State == StateStopped && a.poolLeaseID == ""
	}, "session was not evicted after kick outcome")
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-outcome" {
		t.Fatalf("released leases = %v, want [lease-outcome]", pool.released)
	}
}

func TestWithSharedSessionPoolOption(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManagerWithOptions(map[string]config.AgentConfig{"scanner": {Backend: "claude"}},
		discardLogger(),
		ProjectContext{},
		WithSharedSessionPool(pool),
	)
	if m.sharedPool != pool {
		t.Fatal("sharedPool option was not applied")
	}
}

// stopSharedPoolWatcher cancels the lease-renewal goroutine so it cannot
// outlive the test.
func stopSharedPoolWatcher(t *testing.T, m *Manager, name string) {
	t.Helper()
	t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if a := m.agents[name]; a != nil && a.poolLeaseCancel != nil {
			a.poolLeaseCancel()
			a.poolLeaseCancel = nil
		}
	})
}

// A transient Hub failure must not tear down a live CLI session: the watcher
// keeps retrying until the grace window since the last successful renewal
// has elapsed, and only then evicts.
func TestSharedPoolRenewFailureToleratedUntilGraceElapses(t *testing.T) {
	pool := &recordingSharedSessionPool{
		renewFn: func(context.Context, string) error { return fmt.Errorf("hub rolling") },
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.LastKick = &now // mid-turn: never idle-evictable
	a.poolLeaseID = "lease-grace"

	lastRenewed := time.Now()
	if done := m.sharedPoolLeaseTick("scanner", "lease-grace", &lastRenewed); done {
		t.Fatal("first renewal failure inside the grace window must not stop the watcher")
	}
	m.mu.RLock()
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateRunning || lease != "lease-grace" {
		t.Fatalf("after one failed renew: state=%q lease=%q, want running with lease retained", state, lease)
	}

	lastRenewed = time.Now().Add(-sharedPoolRenewGrace - time.Second)
	if done := m.sharedPoolLeaseTick("scanner", "lease-grace", &lastRenewed); !done {
		t.Fatal("renewal failure past the grace window must evict and stop the watcher")
	}
	m.mu.RLock()
	state, lease = a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateStopped || lease != "" {
		t.Fatalf("after grace elapsed: state=%q lease=%q, want stopped with lease released", state, lease)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-grace" {
		t.Fatalf("released leases = %v, want [lease-grace]", pool.released)
	}
}

func TestSharedPoolRenewSuccessAdvancesLastRenewed(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.LastKick = &now
	a.poolLeaseID = "lease-ok"

	stale := time.Now().Add(-10 * time.Minute)
	lastRenewed := stale
	if done := m.sharedPoolLeaseTick("scanner", "lease-ok", &lastRenewed); done {
		t.Fatal("successful renewal must keep the watcher running")
	}
	if !lastRenewed.After(stale) {
		t.Fatal("successful renewal did not advance lastRenewed")
	}
	if done := m.sharedPoolLeaseTick("scanner", "other-lease", &lastRenewed); !done {
		t.Fatal("watcher for a superseded lease must stop")
	}
}

// Regression for the auto-start error path: SendKick releases m.mu around
// Start and must re-acquire it on EVERY return, or its deferred Unlock panics
// with "sync: Unlock of unlocked RWMutex" (a non-recoverable fatal). The
// governor hits this whenever the Hub reports the pool busy.
func TestSendKickAutoStartBusyPoolReturnsErrorWithoutUnlockPanic(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy },
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude", LaunchCmd: "cat"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	t.Cleanup(func() { _ = testTmuxCommand("kill-session", "-t", "hive-scanner").Run() })

	err := m.SendKick("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "shared session pool is busy") {
		t.Fatalf("SendKick error = %v, want busy-pool auto-start failure", err)
	}
	// The lock must be balanced: a second call would deadlock or panic otherwise.
	if _, err := m.GetStatus("scanner"); err != nil {
		t.Fatalf("GetStatus after failed auto-start should still work: %v", err)
	}
	if got := pool.acquiredNames(); len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("acquired = %v, want one attempt for scanner", got)
	}
	m.mu.RLock()
	queued := m.agents["scanner"].startupLaunchQueued
	m.mu.RUnlock()
	if queued {
		t.Fatal("failed auto-start left startupLaunchQueued armed")
	}
}

// A kick to a pool-evicted (stopped) agent relaunches it and is delivered by
// the readiness-gated startup path instead of being typed into a booting pane.
func TestSendKickAutoStartDefersKickUntilCLIReady(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", LaunchCmd: "printf 'goose is ready\\n'; cat"},
	}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	t.Cleanup(func() { _ = testTmuxCommand("kill-session", "-t", "hive-scanner").Run() })
	stopSharedPoolWatcher(t, m, "scanner")

	if err := m.SendKick("scanner", "resume after eviction"); err != nil {
		t.Fatalf("SendKick to evicted pooled agent should auto-start and defer, got: %v", err)
	}
	if got := pool.acquiredNames(); len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("acquired = %v, want [scanner]", got)
	}
	testutil.EventuallyEvery(t, 15*time.Second, 50*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		a := m.agents["scanner"]
		return a.State == StateRunning && a.poolLeaseID == "lease-scanner" &&
			a.LastKick != nil && a.LastKickMessage == "resume after eviction"
	}, "deferred auto-start kick was not delivered")
	m.mu.RLock()
	history := len(m.agents["scanner"].KickHistory)
	m.mu.RUnlock()
	if history != 1 {
		t.Fatalf("KickHistory length = %d, want exactly the deferred kick", history)
	}
	cleanupAgent(t, m, "scanner")
}

// Restart relaunches via launchInTmux directly; a stopped pooled agent must
// still take a slot from the Hub or the global cap is bypassed.
func TestRestartOfStoppedPooledAgentAcquiresLease(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude", LaunchCmd: "cat"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	t.Cleanup(func() { _ = testTmuxCommand("kill-session", "-t", "hive-scanner").Run() })
	stopSharedPoolWatcher(t, m, "scanner")

	if err := m.RestartWithReason(context.Background(), "scanner", "operator"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	defer cleanupAgent(t, m, "scanner")
	if got := pool.acquiredNames(); len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("acquired = %v, want [scanner]", got)
	}
	m.mu.RLock()
	a := m.agents["scanner"]
	lease, watcher := a.poolLeaseID, a.poolLeaseCancel != nil
	m.mu.RUnlock()
	if lease != "lease-scanner" || !watcher {
		t.Fatalf("after restart: lease=%q watcher=%v, want lease-scanner with renewal watcher", lease, watcher)
	}

	// A second restart of the now-leased agent must not acquire again.
	if err := m.RestartWithReason(context.Background(), "scanner", "operator"); err != nil {
		t.Fatalf("second Restart: %v", err)
	}
	if got := pool.acquiredNames(); len(got) != 1 {
		t.Fatalf("acquired after second restart = %v, want still one acquisition", got)
	}
}

func TestRestartOfStoppedPooledAgentBusyPoolFails(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy },
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude", LaunchCmd: "cat"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	t.Cleanup(func() { _ = testTmuxCommand("kill-session", "-t", "hive-scanner").Run() })

	err := m.RestartWithReason(context.Background(), "scanner", "operator")
	if err == nil || !strings.Contains(err.Error(), "shared session pool is busy") {
		t.Fatalf("Restart error = %v, want busy-pool acquire failure", err)
	}
	m.mu.RLock()
	state, lease := m.agents["scanner"].State, m.agents["scanner"].poolLeaseID
	m.mu.RUnlock()
	if state == StateRunning || lease != "" {
		t.Fatalf("after failed acquire: state=%q lease=%q, want not running and no lease", state, lease)
	}
}

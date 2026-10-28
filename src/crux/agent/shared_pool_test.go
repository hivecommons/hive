package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
	renewFn   func(id string) error
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

func (p *recordingSharedSessionPool) Renew(_ context.Context, id string) error {
	p.mu.Lock()
	p.renewed = append(p.renewed, id)
	fn := p.renewFn
	p.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	return nil
}

func (p *recordingSharedSessionPool) renewCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.renewed)
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

// stoppedPoolAgent builds a manager whose single claude agent is parked in
// StateStopped (the post-eviction shape) with the recording pool attached.
func stoppedPoolAgent(t *testing.T, pool *recordingSharedSessionPool, launchCmd string) *Manager {
	t.Helper()
	cfg := config.AgentConfig{Backend: "claude"}
	if launchCmd != "" {
		cfg.LaunchCmd = launchCmd
	}
	m := NewManager(map[string]config.AgentConfig{"scanner": cfg}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	return m
}

// TestSendKickAutoStartFailureKeepsLockBalanced pins the lock discipline of
// SendKick's auto-start branch: when Start fails, SendKick must re-acquire
// m.mu before returning or its deferred Unlock fatals the process ("sync:
// Unlock of unlocked RWMutex"). The launch guard makes Start fail in its
// phase-1 critical section, so no tmux is needed. It also pins that the kick
// marks the launch startup-owned BEFORE calling Start.
func TestSendKickAutoStartFailureKeepsLockBalanced(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := stoppedPoolAgent(t, pool, "")
	m.mu.Lock()
	m.agents["scanner"].launching = true
	m.mu.Unlock()

	err := m.SendKick("scanner", "do work")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("SendKick error = %v, want auto-start failure", err)
	}
	// Survives only if SendKick returned with m.mu held for its deferred Unlock.
	m.mu.Lock()
	queued := m.agents["scanner"].startupLaunchQueued
	m.mu.Unlock()
	if !queued {
		t.Fatal("auto-start kick did not mark the launch startup-owned before Start")
	}
	if n := len(pool.acquired); n != 0 {
		t.Fatalf("pool.Acquire called %d times before the launch guard, want 0", n)
	}
}

// TestDeliverKickAsyncAutoStartFailureReturnsError is the async twin of the
// lock-discipline test above.
func TestDeliverKickAsyncAutoStartFailureReturnsError(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := stoppedPoolAgent(t, pool, "")
	m.mu.Lock()
	m.agents["scanner"].launching = true
	m.mu.Unlock()

	err := m.deliverKickAsync("scanner", "do work")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("deliverKickAsync error = %v, want auto-start failure", err)
	}
	m.mu.Lock()
	queued := m.agents["scanner"].startupLaunchQueued
	m.mu.Unlock()
	if !queued {
		t.Fatal("async auto-start kick did not mark the launch startup-owned before Start")
	}
}

// TestSendKickAutoStartPoolBusyReturnsError drives the real Start path up to
// the Hub lease: a busy pool must surface as a SendKick error (the governor
// treats it as benign) and leave the agent stopped with no lease.
func TestSendKickAutoStartPoolBusyReturnsError(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy },
	}
	m := stoppedPoolAgent(t, pool, "")
	forceSharedUID(t, m, "scanner")
	t.Cleanup(func() { cleanupAgent(t, m, "scanner") })

	err := m.SendKick("scanner", "do work")
	if err == nil || !errors.Is(err, errSharedSessionPoolBusy) {
		t.Fatalf("SendKick error = %v, want wrapped pool-busy", err)
	}
	ap, _ := m.GetStatus("scanner")
	if ap.State == StateRunning {
		t.Fatal("agent reported running after the pool refused a lease")
	}
	m.mu.RLock()
	lease := m.agents["scanner"].poolLeaseID
	m.mu.RUnlock()
	if lease != "" {
		t.Fatalf("agent holds lease %q after a refused acquire", lease)
	}
}

// TestSendKickAutoStartDefersKickUntilCLIReady pins that a dashboard/request
// kick which auto-starts an evicted agent is handed to the startup-kick drain
// (readiness-gated) instead of being typed into a CLI-less pane and tripping
// restartForKick on the session it just launched.
func TestSendKickAutoStartDefersKickUntilCLIReady(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := stoppedPoolAgent(t, pool, "printf 'goose is ready\\n'; cat")
	forceSharedUID(t, m, "scanner")
	t.Cleanup(func() { cleanupAgent(t, m, "scanner") })

	if err := m.SendKick("scanner", "go drain the queue"); err != nil {
		t.Fatalf("SendKick auto-start: %v", err)
	}
	m.mu.RLock()
	agent := m.agents["scanner"]
	state, lease, restarts := agent.State, agent.poolLeaseID, agent.RestartCount
	m.mu.RUnlock()
	if state != StateRunning {
		t.Fatalf("state after auto-start = %q, want running", state)
	}
	if lease != "lease-scanner" {
		t.Fatalf("poolLeaseID = %q, want lease-scanner", lease)
	}
	if restarts != 0 {
		t.Fatalf("RestartCount = %d after auto-start kick, want 0 (kick must not restart the fresh CLI)", restarts)
	}

	testutil.EventuallyEvery(t, 15*time.Second, 100*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return agent.LastKick != nil
	}, "deferred auto-start kick was never delivered")
	m.mu.RLock()
	msg, history, restarts := agent.LastKickMessage, len(agent.KickHistory), agent.RestartCount
	m.mu.RUnlock()
	if msg != "go drain the queue" {
		t.Fatalf("LastKickMessage = %q, want the auto-start kick", msg)
	}
	if history != 1 {
		t.Fatalf("KickHistory length = %d, want exactly the deferred kick", history)
	}
	if restarts != 0 {
		t.Fatalf("RestartCount = %d after delivery, want 0", restarts)
	}
}

func runningPoolAgent(t *testing.T, pool *recordingSharedSessionPool, leaseID string) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.poolLeaseID = leaseID
	return m, a
}

func TestWatchSharedPoolLeaseToleratesTransientRenewFailure(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	pool := &recordingSharedSessionPool{renewFn: func(string) error {
		if fail.Load() {
			return errors.New("503 hub rolling")
		}
		return nil
	}}
	m, a := runningPoolAgent(t, pool, "lease-grace")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchSharedPoolLeaseEvery(ctx, "scanner", "lease-grace", 10*time.Millisecond, time.Hour)

	// Several failed renewals inside the grace window must not evict.
	testutil.EventuallyEvery(t, 5*time.Second, 5*time.Millisecond, func() bool {
		return pool.renewCount() >= 5
	}, "watcher stopped renewing")
	m.mu.RLock()
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateRunning || lease != "lease-grace" {
		t.Fatalf("agent evicted on transient renewal failure: state=%q lease=%q", state, lease)
	}

	// Once the Hub answers again the watcher simply carries on.
	fail.Store(false)
	before := pool.renewCount()
	testutil.EventuallyEvery(t, 5*time.Second, 5*time.Millisecond, func() bool {
		return pool.renewCount() >= before+3
	}, "watcher did not keep renewing after recovery")
	m.mu.RLock()
	state = a.State
	m.mu.RUnlock()
	if state != StateRunning {
		t.Fatalf("agent state after recovery = %q, want running", state)
	}
}

func TestWatchSharedPoolLeaseEvictsAfterGraceWindow(t *testing.T) {
	pool := &recordingSharedSessionPool{renewFn: func(string) error { return errors.New("connection refused") }}
	m, a := runningPoolAgent(t, pool, "lease-expired")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const grace = 150 * time.Millisecond
	started := time.Now()
	go m.watchSharedPoolLeaseEvery(ctx, "scanner", "lease-expired", 10*time.Millisecond, grace)

	testutil.EventuallyEvery(t, 5*time.Second, 5*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return a.State == StateStopped && a.poolLeaseID == ""
	}, "agent was not evicted after the renewal grace window")
	if elapsed := time.Since(started); elapsed < grace {
		t.Fatalf("evicted after %v, before the %v grace window elapsed", elapsed, grace)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-expired" {
		t.Fatalf("released leases = %v, want [lease-expired]", pool.released)
	}
}

func TestWatchSharedPoolLeaseEvictsImmediatelyWhenLeaseUnknown(t *testing.T) {
	pool := &recordingSharedSessionPool{renewFn: func(string) error { return errSharedSessionLeaseNotFound }}
	m, a := runningPoolAgent(t, pool, "lease-gone")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchSharedPoolLeaseEvery(ctx, "scanner", "lease-gone", 10*time.Millisecond, time.Hour)

	testutil.EventuallyEvery(t, 5*time.Second, 5*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return a.State == StateStopped && a.poolLeaseID == ""
	}, "agent kept its session after the Hub reported the lease unknown")
	if n := pool.renewCount(); n != 1 {
		t.Fatalf("renew attempts before eviction = %d, want 1", n)
	}
}

// TestRestartAcquiresSharedPoolLease pins that the dashboard/recovery restart
// path re-enters the pool: an evicted agent (stopped, no lease) must not be
// relaunched without a lease, or the fleet-wide cap is bypassed.
func TestRestartAcquiresSharedPoolLease(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := stoppedPoolAgent(t, pool, "")
	forceSharedUID(t, m, "scanner")
	t.Cleanup(func() { cleanupAgent(t, m, "scanner") })

	if err := m.Restart(context.Background(), "scanner"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	m.mu.RLock()
	a := m.agents["scanner"]
	state, lease, watcher := a.State, a.poolLeaseID, a.poolLeaseCancel != nil
	m.mu.RUnlock()
	if state != StateRunning {
		t.Fatalf("state after restart = %q, want running", state)
	}
	if lease != "lease-scanner" || !watcher {
		t.Fatalf("restart bound lease=%q watcher=%v, want lease-scanner with a renewal watcher", lease, watcher)
	}
	pool.mu.Lock()
	acquired := append([]string(nil), pool.acquired...)
	pool.mu.Unlock()
	if len(acquired) != 1 || acquired[0] != "scanner" {
		t.Fatalf("pool.Acquire calls = %v, want [scanner]", acquired)
	}
}

// TestRestartRefusedWhenSharedPoolBusy: a restart that cannot obtain a slot
// must fail instead of running an unleased CLI session.
func TestRestartRefusedWhenSharedPoolBusy(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy },
	}
	m := stoppedPoolAgent(t, pool, "")
	forceSharedUID(t, m, "scanner")
	t.Cleanup(func() { cleanupAgent(t, m, "scanner") })

	err := m.Restart(context.Background(), "scanner")
	if err == nil || !errors.Is(err, errSharedSessionPoolBusy) {
		t.Fatalf("Restart error = %v, want wrapped pool-busy", err)
	}
	m.mu.RLock()
	state, lease := m.agents["scanner"].State, m.agents["scanner"].poolLeaseID
	m.mu.RUnlock()
	if state == StateRunning || lease != "" {
		t.Fatalf("refused restart left state=%q lease=%q", state, lease)
	}
}

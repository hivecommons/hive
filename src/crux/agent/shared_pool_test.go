package agent

import (
	"context"
	"errors"
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
	p.mu.Unlock()
	return nil
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

// assertManagerLockBalanced fails (via the deadline) when a previous call left
// m.mu in an unbalanced state. The unbalanced-Unlock variant is a runtime
// fatal, so reaching this helper at all after the call under test already
// proves the deferred Unlock found the lock held.
func assertManagerLockBalanced(t *testing.T, m *Manager) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("m.mu is still held after the call under test returned")
	}
}

// TestSendKickAutoStartFailureKeepsLockBalanced pins the review fix for the
// shared-pool auto-start branch of SendKick: when Start fails (here: another
// launch already owns the guard, the fast Phase-1 refusal that needs no tmux)
// the error path must re-take m.mu before returning, or the deferred Unlock
// fatals with "sync: Unlock of unlocked RWMutex".
func TestSendKickAutoStartFailureKeepsLockBalanced(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	a := m.agents["scanner"]
	a.State = StateStopped
	a.launching = true
	m.mu.Unlock()

	err := m.SendKick("scanner", "hello")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("SendKick error = %v, want auto-start failure", err)
	}
	if !strings.Contains(err.Error(), "launch already in progress") {
		t.Fatalf("SendKick error = %v, want the Start guard error wrapped", err)
	}
	assertManagerLockBalanced(t, m)
	m.mu.RLock()
	queued := a.startupLaunchQueued
	m.mu.RUnlock()
	if !queued {
		t.Fatal("SendKick must mark the launch queued before auto-starting so the kick is deferred until the CLI is ready")
	}
}

// TestDeliverKickAsyncAutoStartFailureKeepsLockBalanced is the async twin:
// deliverKickAsync must hold no lock on return from its auto-start error path.
func TestDeliverKickAsyncAutoStartFailureKeepsLockBalanced(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	a := m.agents["scanner"]
	a.State = StateStopped
	a.launching = true
	m.mu.Unlock()

	err := m.deliverKickAsync("scanner", "hello")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("deliverKickAsync error = %v, want auto-start failure", err)
	}
	assertManagerLockBalanced(t, m)
	m.mu.RLock()
	queued := a.startupLaunchQueued
	m.mu.RUnlock()
	if !queued {
		t.Fatal("deliverKickAsync must mark the launch queued before auto-starting")
	}
}

// TestSendKickAutoStartPoolBusyIsNotFatal drives the auto-start branch all the
// way into Start's lease acquisition (needs tmux for ensureTmuxSession) and
// has the pool report busy — the exact error the governor treats as benign
// and that crashed the process before the re-lock fix. It also proves the
// launch was marked queued BEFORE the slot was requested.
func TestSendKickAutoStartPoolBusyIsNotFatal(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "scanner")
	queuedAtAcquire := make(chan bool, 1)
	pool := &recordingSharedSessionPool{
		acquireFn: func(_ context.Context, name string) (string, error) {
			m.mu.RLock()
			queuedAtAcquire <- m.agents[name].startupLaunchQueued
			m.mu.RUnlock()
			return "", errSharedSessionPoolBusy
		},
	}
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	defer cleanupAgent(t, m, "scanner")

	err := m.SendKick("scanner", "hello")
	if err == nil || !errors.Is(err, errSharedSessionPoolBusy) {
		t.Fatalf("SendKick error = %v, want wrapped pool-busy error", err)
	}
	assertManagerLockBalanced(t, m)
	select {
	case queued := <-queuedAtAcquire:
		if !queued {
			t.Fatal("startupLaunchQueued was false when the pool slot was requested")
		}
	default:
		t.Fatal("pool Acquire was never reached")
	}
	m.mu.RLock()
	a := m.agents["scanner"]
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state == StateRunning || lease != "" {
		t.Fatalf("after busy pool: state=%q lease=%q, want not running with no lease", state, lease)
	}
}

// TestRestartRelaunchAcquiresSharedPoolLease pins that an evicted pool agent
// (StateStopped, no lease) relaunched via Restart re-acquires a slot and gets
// a lease watcher, instead of running outside the fleet-wide cap.
func TestRestartRelaunchAcquiresSharedPoolLease(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "scanner")
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()
	defer cleanupAgent(t, m, "scanner")
	defer func() {
		m.mu.Lock()
		cancel := m.agents["scanner"].poolLeaseCancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}()

	if err := m.Restart(context.Background(), "scanner"); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}

	pool.mu.Lock()
	acquired := append([]string(nil), pool.acquired...)
	pool.mu.Unlock()
	if len(acquired) != 1 || acquired[0] != "scanner" {
		t.Fatalf("pool acquisitions = %v, want [scanner]", acquired)
	}
	m.mu.RLock()
	a := m.agents["scanner"]
	lease, watcher := a.poolLeaseID, a.poolLeaseCancel != nil
	m.mu.RUnlock()
	if lease != "lease-scanner" {
		t.Fatalf("poolLeaseID = %q, want lease-scanner", lease)
	}
	if !watcher {
		t.Fatal("relaunch bound a lease without starting its renewal watcher")
	}
}

// TestRestartRelaunchKeepsExistingLease: a restart of an agent that already
// holds a lease (the kick path's crashed-CLI recovery) must not request a
// second slot.
func TestRestartRelaunchKeepsExistingLease(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	m.mu.Lock()
	m.agents["scanner"].poolLeaseID = "lease-held"
	m.mu.Unlock()

	leaseID, err := m.acquireSharedPoolLeaseForRelaunchUnlocked(context.Background(), "scanner")
	if err != nil || leaseID != "" {
		t.Fatalf("acquire for relaunch = (%q, %v), want no new lease", leaseID, err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.acquired) != 0 {
		t.Fatalf("pool acquisitions = %v, want none while a lease is held", pool.acquired)
	}
}

// TestSharedPoolRenewalGraceWindow pins that a failing renewal evicts only once
// the lease has gone unrenewed for the grace window, not on the first error.
func TestSharedPoolRenewalGraceWindow(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	last := time.Now()
	if m.sharedPoolRenewalExhausted(last, last.Add(sharedPoolRenewInterval)) {
		t.Fatal("first failed renewal (20s after success) must not evict")
	}
	if m.sharedPoolRenewalExhausted(last, last.Add(2*sharedPoolRenewInterval)) {
		t.Fatal("second failed renewal (40s after success) must not evict")
	}
	if !m.sharedPoolRenewalExhausted(last, last.Add(sharedPoolRenewGraceAfter)) {
		t.Fatal("renewal failures past the grace window must evict")
	}
	if sharedPoolRenewGraceAfter >= 90*time.Second {
		t.Fatalf("grace window %v must end before the Hub's 90s lease TTL", sharedPoolRenewGraceAfter)
	}
}

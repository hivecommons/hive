package agent

import (
	"context"
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

// sharedPoolStoppedAgentManager returns a manager whose only agent is a
// pool-evicted (StateStopped) agent that Start refuses fast, with no tmux
// I/O, because a launch is already marked in progress. That makes the
// auto-start error branch of SendKick/deliverKickAsync reachable hermetically.
func sharedPoolStoppedAgentManager(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = &recordingSharedSessionPool{}
	a := m.agents["scanner"]
	a.State = StateStopped
	a.launching = true
	return m, a
}

// assertManagerLockBalanced fails if m.mu was left locked or was unlocked
// twice by the caller's code path. A double Unlock is a runtime fatal, so
// reaching this helper at all already proves half the contract; the
// bounded Lock proves the other half.
func assertManagerLockBalanced(t *testing.T, m *Manager) {
	t.Helper()
	locked := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("m.mu still held after the auto-start error path returned")
	}
}

func TestSendKickSharedPoolAutoStartErrorKeepsLockBalanced(t *testing.T) {
	m, a := sharedPoolStoppedAgentManager(t)

	err := m.SendKick("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("SendKick error = %v, want auto-start failure", err)
	}
	assertManagerLockBalanced(t, m)
	m.mu.RLock()
	queued := a.startupLaunchQueued
	m.mu.RUnlock()
	if !queued {
		t.Fatal("auto-start kick did not arm the startup-kick deferral before Start")
	}
}

func TestDeliverKickAsyncSharedPoolAutoStartErrorKeepsLockBalanced(t *testing.T) {
	m, a := sharedPoolStoppedAgentManager(t)

	err := m.deliverKickAsync("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("deliverKickAsync error = %v, want auto-start failure", err)
	}
	assertManagerLockBalanced(t, m)
	m.mu.RLock()
	queued := a.startupLaunchQueued
	m.mu.RUnlock()
	if !queued {
		t.Fatal("auto-start kick did not arm the startup-kick deferral before Start")
	}
}

func TestSendKickSharedPoolDoesNotAutoStartOperatorStoppedAgent(t *testing.T) {
	m, a := sharedPoolStoppedAgentManager(t)
	a.launching = false
	a.OperatorStopped = true
	pool := m.sharedPool.(*recordingSharedSessionPool)

	err := m.SendKick("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "cannot be kicked") {
		t.Fatalf("SendKick error = %v, want refusal", err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.acquired) != 0 {
		t.Fatalf("operator-stopped agent acquired a lease: %v", pool.acquired)
	}
}

func TestSharedPoolRenewFailureGrace(t *testing.T) {
	now := time.Now()
	if renewFailureExceedsGrace(now.Add(-sharedPoolRenewInterval), now) {
		t.Fatal("a single missed renewal must not evict a live session")
	}
	if renewFailureExceedsGrace(now.Add(-sharedPoolRenewFailureGrace+time.Second), now) {
		t.Fatal("renewal failures inside the grace window must not evict")
	}
	if !renewFailureExceedsGrace(now.Add(-sharedPoolRenewFailureGrace), now) {
		t.Fatal("renewal failures past the grace window must evict")
	}
	if sharedPoolRenewFailureGrace >= defaultSharedAgentLeaseTTLForTest {
		t.Fatalf("grace %v must stay below the Hub lease TTL %v", sharedPoolRenewFailureGrace, defaultSharedAgentLeaseTTLForTest)
	}
}

// defaultSharedAgentLeaseTTLForTest mirrors hub.defaultSharedAgentLeaseTTL;
// the agent package must not import pkg/hub.
const defaultSharedAgentLeaseTTLForTest = 90 * time.Second

func TestSettleRelaunchLeaseLocked(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]

	// Fresh lease + failed relaunch: handed back, never bound.
	m.mu.Lock()
	m.settleRelaunchLeaseLocked(a, "scanner", "lease-fail", true, false)
	m.mu.Unlock()
	testutil.EventuallyEvery(t, 2*time.Second, 10*time.Millisecond, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.released) == 1 && pool.released[0] == "lease-fail"
	}, "fresh lease was not released after a failed relaunch")
	m.mu.RLock()
	if a.poolLeaseID != "" || a.poolLeaseCancel != nil {
		m.mu.RUnlock()
		t.Fatalf("failed relaunch bound lease %q", a.poolLeaseID)
	}
	m.mu.RUnlock()

	// Fresh lease + successful relaunch: bound, watcher armed.
	m.mu.Lock()
	m.settleRelaunchLeaseLocked(a, "scanner", "lease-ok", true, true)
	bound, watcher := a.poolLeaseID, a.poolLeaseCancel
	m.mu.Unlock()
	if bound != "lease-ok" || watcher == nil {
		t.Fatalf("successful relaunch: lease=%q watcher=%v, want lease-ok with watcher", bound, watcher != nil)
	}
	m.mu.Lock()
	cancel := a.poolLeaseCancel
	a.poolLeaseCancel = nil
	m.mu.Unlock()
	cancel()
}

func TestAcquireRelaunchLeaseUnlockedReusesHeldLease(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.poolLeaseID = "lease-held"

	leaseID, fresh, err := m.acquireRelaunchLeaseUnlocked(context.Background(), "scanner")
	if err != nil || leaseID != "lease-held" || fresh {
		t.Fatalf("held lease: (%q, fresh=%v, %v), want (lease-held, false, nil)", leaseID, fresh, err)
	}

	a.poolLeaseID = ""
	leaseID, fresh, err = m.acquireRelaunchLeaseUnlocked(context.Background(), "scanner")
	if err != nil || leaseID != "lease-scanner" || !fresh {
		t.Fatalf("evicted agent: (%q, fresh=%v, %v), want (lease-scanner, true, nil)", leaseID, fresh, err)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.acquired) != 1 {
		t.Fatalf("acquire calls = %v, want exactly one", pool.acquired)
	}
}

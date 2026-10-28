package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// A pool-evicted agent is auto-started by SendKick. When Start fails (here:
// a launch already in progress), SendKick must return the error with m.mu
// held exactly as its deferred Unlock expects — the original branch returned
// unlocked and the deferred Unlock crashed the process.
func TestSharedPoolSendKickAutoStartFailureReturnsError(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateStopped
	a.launching = true

	err := m.SendKick("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("SendKick error = %v, want auto-start failure", err)
	}
	// The lock must be free again and the agent marked startup-queued so the
	// in-progress launch arms the readiness-gated startup kick path.
	m.mu.Lock()
	queued := a.startupLaunchQueued
	m.mu.Unlock()
	if !queued {
		t.Fatal("auto-start did not mark the agent startup-queued before Start")
	}
	if err := m.SendKick("scanner", "again"); err == nil {
		t.Fatal("second SendKick on a still-launching agent should fail, not hang or panic")
	}
}

func TestSharedPoolDeliverKickAsyncAutoStartFailureReturnsError(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateStopped
	a.launching = true

	err := m.deliverKickAsync("scanner", "do the thing")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("deliverKickAsync error = %v, want auto-start failure", err)
	}
	m.mu.Lock()
	queued := a.startupLaunchQueued
	m.mu.Unlock()
	if !queued {
		t.Fatal("auto-start did not mark the agent startup-queued before Start")
	}
}

// Auto-start branches must not fire for agents an operator stopped or paused.
func TestSharedPoolAutoStartEligibility(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	a := m.agents["scanner"]
	a.State = StateStopped
	if m.sharedPoolAutoStartEligibleLocked(a) {
		t.Fatal("eligible without a shared pool")
	}
	m.sharedPool = &recordingSharedSessionPool{}
	if !m.sharedPoolAutoStartEligibleLocked(a) {
		t.Fatal("stopped agent with pool should be eligible")
	}
	a.OperatorStopped = true
	if m.sharedPoolAutoStartEligibleLocked(a) {
		t.Fatal("operator-stopped agent must not auto-start")
	}
	a.OperatorStopped = false
	a.Paused = true
	if m.sharedPoolAutoStartEligibleLocked(a) {
		t.Fatal("paused agent must not auto-start")
	}
}

func TestSharedPoolRenewalShouldEvict(t *testing.T) {
	now := time.Now()
	if sharedPoolRenewalShouldEvict(nil, now, now) {
		t.Fatal("successful renewal must not evict")
	}
	transient := errors.New("shared session pool returned 503 Service Unavailable")
	if sharedPoolRenewalShouldEvict(transient, now.Add(-20*time.Second), now) {
		t.Fatal("first transient failure must be tolerated")
	}
	if sharedPoolRenewalShouldEvict(transient, now.Add(-40*time.Second), now) {
		t.Fatal("second transient failure inside the grace window must be tolerated")
	}
	if !sharedPoolRenewalShouldEvict(transient, now.Add(-sharedPoolRenewGrace), now) {
		t.Fatal("failures past the grace window must evict")
	}
	gone := fmt.Errorf("renew: %w", errSharedSessionPoolLeaseGone)
	if !sharedPoolRenewalShouldEvict(gone, now, now) {
		t.Fatal("a lease the Hub no longer knows must evict immediately")
	}
}

func TestSharedPoolClientRenewMapsLeaseNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "lease not found", http.StatusNotFound)
	}))
	defer srv.Close()
	client, err := NewSharedSessionPoolClient(srv.URL, "hive-1", "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Renew(context.Background(), "lease-x"); !errors.Is(err, errSharedSessionPoolLeaseGone) {
		t.Fatalf("Renew error = %v, want errSharedSessionPoolLeaseGone", err)
	}
}

// Restart of an evicted agent (no lease) must take a pool slot; when the pool
// refuses, the restart fails instead of relaunching outside the cap.
func TestSharedPoolRestartAcquiresLease(t *testing.T) {
	pool := &recordingSharedSessionPool{acquireFn: func(context.Context, string) (string, error) {
		return "", errSharedSessionPoolBusy
	}}
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateStopped

	lease, err := m.acquireSharedPoolLeaseUnlocked(context.Background(), a, "scanner")
	if !errors.Is(err, errSharedSessionPoolBusy) || lease.leaseID != "" {
		t.Fatalf("acquire = (%+v, %v), want busy error", lease, err)
	}

	pool.mu.Lock()
	pool.acquireFn = nil
	pool.mu.Unlock()
	lease, err = m.acquireSharedPoolLeaseUnlocked(context.Background(), a, "scanner")
	if err != nil || lease.leaseID != "lease-scanner" || lease.hasWatcher {
		t.Fatalf("acquire = (%+v, %v), want fresh lease", lease, err)
	}

	// An agent that already holds a lease keeps it and acquires nothing new.
	m.mu.Lock()
	a.poolLeaseID = "lease-kept"
	m.mu.Unlock()
	lease, err = m.acquireSharedPoolLeaseUnlocked(context.Background(), a, "scanner")
	if err != nil || lease.leaseID != "lease-kept" {
		t.Fatalf("acquire with held lease = (%+v, %v)", lease, err)
	}
	pool.mu.Lock()
	acquired := len(pool.acquired)
	pool.mu.Unlock()
	if acquired != 2 {
		t.Fatalf("pool.Acquire calls = %d, want 2", acquired)
	}

	// A lease the relaunch never bound is returned to the Hub.
	m.mu.Lock()
	m.releaseSharedPoolLeaseIfUnboundLocked("scanner", sharedPoolLease{leaseID: "lease-orphan"})
	m.releaseSharedPoolLeaseIfUnboundLocked("scanner", sharedPoolLease{leaseID: "lease-kept"})
	m.mu.Unlock()
	testutil.EventuallyEvery(t, 2*time.Second, 10*time.Millisecond, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.released) == 1 && pool.released[0] == "lease-orphan"
	}, "unbound lease was not released (or the bound one was)")
}

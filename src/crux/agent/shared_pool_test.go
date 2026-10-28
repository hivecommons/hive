package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestSharedPoolRenewFailureTolerance(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	now := time.Now()
	transient := errors.New("dial tcp: connection refused")
	if m.sharedPoolRenewFailureEvicts(transient, now.Add(-20*time.Second), now) {
		t.Fatal("a single transient renewal failure must not evict a live CLI session")
	}
	if m.sharedPoolRenewFailureEvicts(transient, now.Add(-40*time.Second), now) {
		t.Fatal("two missed renewals must not evict a live CLI session")
	}
	if !m.sharedPoolRenewFailureEvicts(transient, now.Add(-sharedPoolRenewGrace), now) {
		t.Fatal("renewal failures past the grace window must evict")
	}
	if !m.sharedPoolRenewFailureEvicts(fmt.Errorf("renew: %w", ErrSharedSessionLeaseNotFound), now.Add(-time.Second), now) {
		t.Fatal("a Hub that no longer knows the lease must evict immediately")
	}
}

func TestSharedPoolClientRenewReportsLeaseNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "lease not found", http.StatusNotFound)
	}))
	defer srv.Close()
	pool, err := NewSharedSessionPoolClient(srv.URL, "school", "token")
	if err != nil {
		t.Fatal(err)
	}
	err = pool.Renew(context.Background(), "lease-gone")
	if !errors.Is(err, ErrSharedSessionLeaseNotFound) {
		t.Fatalf("Renew error = %v, want ErrSharedSessionLeaseNotFound", err)
	}
}

func TestSharedPoolSendKickAutoStartFailureKeepsLockBalanced(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{acquireFn: func(context.Context, string) (string, error) {
		return "", errors.New("shared session pool is busy")
	}}
	m := NewManager(map[string]config.AgentConfig{"scanner": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	defer cleanupAgent(t, m, "scanner")
	m.mu.Lock()
	m.agents["scanner"].State = StateStopped
	m.mu.Unlock()

	// Before the fix the error return left m.mu unlocked for the deferred
	// Unlock, which is a non-recoverable runtime fatal.
	if err := m.SendKick("scanner", "hello"); err == nil {
		t.Fatal("SendKick succeeded although the pool refused a slot")
	}
	m.mu.Lock()
	m.mu.Unlock()
}

func TestSharedPoolRestartAcquiresLeaseForEvictedAgent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	defer cleanupAgent(t, m, "worker")

	if err := m.Start(context.Background(), "worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.evictSharedPoolSession("worker", "lease-worker")
	m.mu.RLock()
	state, lease := m.agents["worker"].State, m.agents["worker"].poolLeaseID
	m.mu.RUnlock()
	if state != StateStopped || lease != "" {
		t.Fatalf("after eviction state=%q lease=%q", state, lease)
	}

	if err := m.Restart(context.Background(), "worker"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	m.mu.RLock()
	lease = m.agents["worker"].poolLeaseID
	watcher := m.agents["worker"].poolLeaseCancel != nil
	m.mu.RUnlock()
	if lease != "lease-worker" || !watcher {
		t.Fatalf("restarted agent lease=%q watcher=%v, want leased with renewal watcher", lease, watcher)
	}
	pool.mu.Lock()
	acquired := append([]string(nil), pool.acquired...)
	pool.mu.Unlock()
	if len(acquired) != 2 {
		t.Fatalf("acquired = %v, want Start and Restart each to take a lease", acquired)
	}
}

func TestSharedPoolRestartRefusedWhenPoolBusy(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	defer cleanupAgent(t, m, "worker")

	if err := m.Start(context.Background(), "worker"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.evictSharedPoolSession("worker", "lease-worker")
	pool.mu.Lock()
	pool.acquireFn = func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy }
	pool.mu.Unlock()

	if err := m.Restart(context.Background(), "worker"); err == nil {
		t.Fatal("Restart relaunched an evicted agent without a shared pool lease")
	}
	m.mu.RLock()
	state, lease := m.agents["worker"].State, m.agents["worker"].poolLeaseID
	m.mu.RUnlock()
	if state == StateRunning || lease != "" {
		t.Fatalf("after refused restart state=%q lease=%q, want not running and unleased", state, lease)
	}
}

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

// cancelLeaseWatcher stops a relaunch-armed renewal watcher so it does not
// outlive the test.
func cancelLeaseWatcher(m *Manager, a *AgentProcess) {
	m.mu.Lock()
	cancel := a.poolLeaseCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (p *recordingSharedSessionPool) setRenewFn(fn func(id string) error) {
	p.mu.Lock()
	p.renewFn = fn
	p.mu.Unlock()
}

// SendKick's auto-start branch releases m.mu around Start and must re-lock on
// the error path too: the deferred Unlock otherwise faults with
// "sync: Unlock of unlocked RWMutex". A busy pool is the common trigger — the
// governor treats that error as benign and still calls sendKick.
func TestSharedPoolSendKickAutoStartErrorRelocks(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) {
			return "", fmt.Errorf("shared session pool is busy")
		},
	}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["worker"]
	forceSharedUID(t, m, "worker")
	a.State = StateStopped
	// Start may have created the tmux session before the lease acquisition
	// failed; tear it down.
	defer cleanupAgent(t, m, "worker")

	err := m.SendKick("worker", "hello")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("SendKick error = %v, want auto-start failure", err)
	}
	// The manager must remain usable: a second call would deadlock if the
	// branch returned with the lock held, and would have panicked above if it
	// returned with it unlocked.
	if err := m.SendKick("worker", "again"); err == nil {
		t.Fatal("second SendKick succeeded against a stopped agent with a busy pool")
	}
	m.mu.RLock()
	queued := a.startupLaunchQueued
	m.mu.RUnlock()
	if queued {
		t.Fatal("startupLaunchQueued must be cleared after a failed auto-start")
	}
	m.mu.RLock()
	state := a.State
	m.mu.RUnlock()
	if state == StateRunning {
		t.Fatalf("agent state = %q after failed auto-start", state)
	}
}

// deliverKickAsync's auto-start branch: Start failure must settle the kick
// as an error without leaving m.mu held.
func TestSharedPoolDeliverKickAsyncAutoStartErrorUnlocks(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) {
			return "", fmt.Errorf("shared session pool is busy")
		},
	}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["worker"]
	forceSharedUID(t, m, "worker")
	a.State = StateStopped
	// Start may have created the tmux session before the lease acquisition
	// failed; tear it down.
	defer cleanupAgent(t, m, "worker")

	err := m.deliverKickAsync("worker", "hello")
	if err == nil || !strings.Contains(err.Error(), "auto-start stopped agent for kick") {
		t.Fatalf("deliverKickAsync error = %v, want auto-start failure", err)
	}
	// Lock must be free again.
	m.mu.Lock()
	m.mu.Unlock()
}

// A single failed renewal must not evict a live session: the Hub lease TTL
// is far longer than one renewal interval, so eviction waits for the grace
// window to lapse.
func TestSharedPoolRenewalFailureToleratedUntilGrace(t *testing.T) {
	oldInterval, oldGrace := sharedPoolRenewInterval, sharedPoolRenewGrace
	sharedPoolRenewInterval, sharedPoolRenewGrace = 5*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { sharedPoolRenewInterval, sharedPoolRenewGrace = oldInterval, oldGrace })

	pool := &recordingSharedSessionPool{}
	pool.setRenewFn(func(string) error { return fmt.Errorf("hub unavailable") })
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.poolLeaseID = "lease-renew"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchSharedPoolLease(ctx, "scanner", "lease-renew")

	// Several renewals fail inside the grace window; the session must survive.
	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.renewed) >= 3
	}, "renewals were not attempted")
	m.mu.RLock()
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateRunning || lease != "lease-renew" {
		t.Fatalf("session evicted inside grace window: state=%q lease=%q", state, lease)
	}

	// Once the grace window lapses with no successful renewal, evict.
	testutil.EventuallyEvery(t, 3*time.Second, 5*time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return a.State == StateStopped && a.poolLeaseID == ""
	}, "session was not evicted after the grace window lapsed")
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.released) != 1 || pool.released[0] != "lease-renew" {
		t.Fatalf("released leases = %v, want [lease-renew]", pool.released)
	}
}

// A successful renewal resets the grace clock, so intermittent failures
// never accumulate into an eviction.
func TestSharedPoolRenewalSuccessResetsGrace(t *testing.T) {
	oldInterval, oldGrace := sharedPoolRenewInterval, sharedPoolRenewGrace
	sharedPoolRenewInterval, sharedPoolRenewGrace = 5*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { sharedPoolRenewInterval, sharedPoolRenewGrace = oldInterval, oldGrace })

	pool := &recordingSharedSessionPool{}
	var calls int
	pool.setRenewFn(func(string) error {
		calls++
		if calls%2 == 0 {
			return nil
		}
		return fmt.Errorf("flaky hub")
	})
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	now := time.Now()
	a.State = StateRunning
	a.StartedAt = &now
	a.poolLeaseID = "lease-flaky"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchSharedPoolLease(ctx, "scanner", "lease-flaky")

	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.renewed) >= 30
	}, "renewals were not attempted")
	m.mu.RLock()
	defer m.mu.RUnlock()
	if a.State != StateRunning || a.poolLeaseID != "lease-flaky" {
		t.Fatalf("alternating renewal failures evicted the session: state=%q lease=%q", a.State, a.poolLeaseID)
	}
}

// Restart paths relaunch via launchInTmux directly; an evicted agent (no
// lease) restarted from the dashboard must acquire a slot like Start does.
func TestSharedPoolRestartAcquiresLeaseWhenNoneHeld(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["worker"]
	forceSharedUID(t, m, "worker")
	defer cleanupAgent(t, m, "worker")
	a.State = StateStopped

	if err := m.RestartWithReason(context.Background(), "worker", "operator"); err != nil {
		t.Fatalf("RestartWithReason: %v", err)
	}
	defer cancelLeaseWatcher(m, a)
	m.mu.RLock()
	lease, watcher := a.poolLeaseID, a.poolLeaseCancel != nil
	m.mu.RUnlock()
	if lease != "lease-worker" {
		t.Fatalf("restarted agent lease = %q, want lease-worker", lease)
	}
	if !watcher {
		t.Fatal("restarted agent has no lease renewal watcher")
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.acquired) != 1 {
		t.Fatalf("acquired = %v, want one acquisition", pool.acquired)
	}
}

// A restart whose lease acquisition fails parks the agent StateStopped so the
// governor / kick auto-start retry it, instead of leaving a dead StateRunning.
func TestSharedPoolRestartWithoutSlotParksStopped(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{
		acquireFn: func(context.Context, string) (string, error) {
			return "", errSharedSessionPoolBusy
		},
	}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["worker"]
	forceSharedUID(t, m, "worker")
	defer cleanupAgent(t, m, "worker")
	a.State = StateRunning

	err := m.RestartWithReason(context.Background(), "worker", "operator")
	if err == nil || !strings.Contains(err.Error(), "acquire shared CLI session slot") {
		t.Fatalf("RestartWithReason error = %v, want lease acquisition failure", err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if a.State != StateStopped {
		t.Fatalf("agent state = %q, want stopped", a.State)
	}
	if a.poolLeaseID != "" {
		t.Fatalf("agent holds lease %q after failed acquisition", a.poolLeaseID)
	}
}

// Restarting an agent that already holds a lease must not acquire another.
func TestSharedPoolRestartKeepsExistingLease(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := &recordingSharedSessionPool{}
	m := NewManager(map[string]config.AgentConfig{"worker": makeAgentConfig("claude", "sonnet")}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["worker"]
	forceSharedUID(t, m, "worker")
	defer cleanupAgent(t, m, "worker")
	a.State = StateRunning
	a.poolLeaseID = "lease-held"

	if err := m.RestartWithReason(context.Background(), "worker", "operator"); err != nil {
		t.Fatalf("RestartWithReason: %v", err)
	}
	defer cancelLeaseWatcher(m, a)
	m.mu.RLock()
	lease := a.poolLeaseID
	m.mu.RUnlock()
	if lease != "lease-held" {
		t.Fatalf("lease after restart = %q, want lease-held", lease)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.acquired) != 0 {
		t.Fatalf("restart acquired a second lease: %v", pool.acquired)
	}
}

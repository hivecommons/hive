package agent

import (
	"context"
	"encoding/json"
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
	renewFn   func(ctx context.Context, id string) error
	releaseFn func(ctx context.Context, id string) error
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

func (p *recordingSharedSessionPool) Release(ctx context.Context, id string) error {
	p.mu.Lock()
	p.released = append(p.released, id)
	fn := p.releaseFn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return nil
}

func (p *recordingSharedSessionPool) snapshot() (acquired, renewed, released []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.acquired...), append([]string(nil), p.renewed...), append([]string(nil), p.released...)
}

func (p *recordingSharedSessionPool) setRenewFn(fn func(ctx context.Context, id string) error) {
	p.mu.Lock()
	p.renewFn = fn
	p.mu.Unlock()
}

// fastSharedPoolRenew shrinks m's lease watcher tick. It must run before any
// watcher on m starts; it never touches package state, so watchers leaked by
// other tests (e.g. a restarted tmux agent) keep their own tick.
func fastSharedPoolRenew(m *Manager) {
	m.sharedPoolRenewInterval = 5 * time.Millisecond
}

// newSharedPoolManager returns a manager with one "scanner" agent that is
// marked running (no tmux) and bound to the given pool.
func newSharedPoolManager(t *testing.T, pool SharedSessionPool) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.sharedPool = pool
	a := m.agents["scanner"]
	a.State = StateRunning
	return m, a
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
		if a.State != StateStopped || a.poolLeaseID != "" {
			return false
		}
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return len(pool.released) > 0
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
	if !m.mu.TryLock() {
		t.Fatal("m.mu still held after SendKick returned an error")
	}
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

func TestSharedPoolWatcherRenewsWhileTurnInFlight(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m, a := newSharedPoolManager(t, pool)
	fastSharedPoolRenew(m)
	now := time.Now()
	a.StartedAt = &now
	a.LastKick = &now // unsettled outcome: the agent is mid-turn, never idle
	a.poolLeaseID = "lease-busy"
	m.mu.Lock()
	m.startSharedPoolLeaseWatcherLocked(a, "scanner", "lease-busy")
	// A second call must not spawn a second watcher.
	first := a.poolLeaseCancel
	m.startSharedPoolLeaseWatcherLocked(a, "scanner", "lease-busy")
	second := a.poolLeaseCancel
	m.mu.Unlock()
	if first == nil || fmt.Sprintf("%p", first) != fmt.Sprintf("%p", second) {
		t.Fatal("watcher cancel was not installed exactly once")
	}

	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		_, renewed, _ := pool.snapshot()
		return len(renewed) >= 3
	}, "watcher did not renew the lease")
	m.mu.RLock()
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateRunning || lease != "lease-busy" {
		t.Fatalf("state=%q lease=%q, want running with lease intact", state, lease)
	}

	// Stopping releases the lease and cancels the watcher; renewals cease.
	if err := m.Stop("scanner"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_, _, released := pool.snapshot()
	if len(released) != 1 || released[0] != "lease-busy" {
		t.Fatalf("released = %v, want [lease-busy]", released)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if a.poolLeaseID != "" || a.poolLeaseCancel != nil {
		t.Fatalf("Stop left lease=%q watcher=%v", a.poolLeaseID, a.poolLeaseCancel != nil)
	}
}

func TestSharedPoolWatcherToleratesTransientRenewFailures(t *testing.T) {
	pool := &recordingSharedSessionPool{renewFn: func(context.Context, string) error {
		return errors.New("dial tcp: connection refused")
	}}
	m, a := newSharedPoolManager(t, pool)
	fastSharedPoolRenew(m)
	now := time.Now()
	a.StartedAt = &now
	a.LastKick = &now
	a.poolLeaseID = "lease-blip"
	m.mu.Lock()
	m.startSharedPoolLeaseWatcherLocked(a, "scanner", "lease-blip")
	m.mu.Unlock()

	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		_, renewed, _ := pool.snapshot()
		return len(renewed) >= 4
	}, "watcher did not keep retrying renewals")
	m.mu.RLock()
	state, lease := a.State, a.poolLeaseID
	m.mu.RUnlock()
	if state != StateRunning || lease != "lease-blip" {
		t.Fatalf("transient failures evicted: state=%q lease=%q", state, lease)
	}

	// A definitive "lease not found" evicts on the next tick.
	pool.setRenewFn(func(context.Context, string) error {
		return fmt.Errorf("renew: %w", ErrSharedSessionLeaseNotFound)
	})
	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return a.State == StateStopped && a.poolLeaseID == "" && a.poolLeaseCancel == nil
	}, "lease-not-found did not evict the session")
	// Eviction clears the agent's lease under m.mu and only then calls
	// Release with the lock dropped, so wait for the Hub call as well.
	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		_, _, released := pool.snapshot()
		return len(released) > 0
	}, "evicted lease was not released")
	_, _, released := pool.snapshot()
	if len(released) != 1 || released[0] != "lease-blip" {
		t.Fatalf("released = %v, want [lease-blip]", released)
	}
}

func TestSharedPoolWatcherExitsWhenLeaseChanges(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m, a := newSharedPoolManager(t, pool)
	fastSharedPoolRenew(m)
	now := time.Now()
	a.LastKick = &now
	a.poolLeaseID = "lease-new"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.watchSharedPoolLease(ctx, "scanner", "lease-old")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher for a superseded lease did not exit")
	}
	if _, renewed, _ := pool.snapshot(); len(renewed) != 0 {
		t.Fatalf("superseded watcher renewed %v", renewed)
	}

	// An unknown agent exits the same way; a cancelled context exits before
	// the first tick.
	done = make(chan struct{})
	go func() {
		defer close(done)
		m.watchSharedPoolLease(ctx, "ghost", "lease-x")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher for an unknown agent did not exit")
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	m.watchSharedPoolLease(cancelled, "scanner", "lease-new")
}

func TestSharedPoolWatcherIdleWithoutPromptKeepsRenewing(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m, a := newSharedPoolManager(t, pool)
	fastSharedPoolRenew(m)
	started := time.Now().Add(-2 * sharedPoolIdleEvictionDelay)
	a.StartedAt = &started
	a.poolLeaseID = "lease-idle"
	// Idle past the delay, but with no tmux pane showing an input prompt the
	// watcher must not evict; it falls through to a renewal.
	m.mu.Lock()
	m.startSharedPoolLeaseWatcherLocked(a, "scanner", "lease-idle")
	m.mu.Unlock()
	testutil.EventuallyEvery(t, 2*time.Second, time.Millisecond, func() bool {
		_, renewed, _ := pool.snapshot()
		return len(renewed) >= 2
	}, "idle agent without a prompt was not renewed")
	m.mu.RLock()
	state := a.State
	cancel := a.poolLeaseCancel
	m.mu.RUnlock()
	if state != StateRunning {
		t.Fatalf("state = %q, want running", state)
	}
	cancel()
}

func TestSharedPoolEvictionGuards(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m, a := newSharedPoolManager(t, pool)
	now := time.Now()
	a.StartedAt = &now
	a.LastKick = &now
	a.poolLeaseID = "lease-guard"
	a.KickOutcome = KickOutcome{Kind: KickOutcomeEnded, At: now, KickAt: now}

	m.evictSharedPoolSession("ghost", "lease-guard")
	m.evictSharedPoolSession("scanner", "lease-other")
	stale := KickOutcome{Kind: KickOutcomeEnded, At: now.Add(-time.Minute), KickAt: now.Add(-time.Minute)}
	m.evictSharedPoolSessionAfterTurn("scanner", "lease-guard", stale)
	a.kickDelivering.Store(true)
	m.evictSharedPoolSessionAfterTurn("scanner", "lease-guard", a.KickOutcome)
	a.kickDelivering.Store(false)
	if a.State != StateRunning || a.poolLeaseID != "lease-guard" {
		t.Fatalf("guarded eviction fired: state=%q lease=%q", a.State, a.poolLeaseID)
	}
	if _, _, released := pool.snapshot(); len(released) != 0 {
		t.Fatalf("released = %v, want none", released)
	}

	pool.releaseFn = func(context.Context, string) error { return errors.New("hub down") }
	m.evictSharedPoolSessionAfterTurn("scanner", "lease-guard", a.KickOutcome)
	if a.State != StateStopped || a.poolLeaseID != "" {
		t.Fatalf("matching outcome did not evict: state=%q lease=%q", a.State, a.poolLeaseID)
	}
	if _, _, released := pool.snapshot(); len(released) != 1 {
		t.Fatalf("released = %v, want one attempt despite Hub error", released)
	}
}

func TestSharedPoolReleaseLeasePaths(t *testing.T) {
	pool := &recordingSharedSessionPool{}
	m, a := newSharedPoolManager(t, pool)
	a.poolLeaseID = "lease-held"
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	a.poolLeaseCancel = watcherCancel

	// Mismatched lease: returned to the Hub, agent binding untouched.
	m.releaseSharedPoolLease("scanner", "lease-stray")
	if a.poolLeaseID != "lease-held" || a.poolLeaseCancel == nil {
		t.Fatal("releasing a stray lease disturbed the agent's own lease")
	}
	// Matching lease: binding cleared and watcher cancelled.
	pool.releaseFn = func(context.Context, string) error { return errors.New("hub down") }
	m.releaseSharedPoolLease("scanner", "lease-held")
	if a.poolLeaseID != "" || a.poolLeaseCancel != nil {
		t.Fatalf("lease binding not cleared: lease=%q cancel=%v", a.poolLeaseID, a.poolLeaseCancel != nil)
	}
	select {
	case <-watcherCtx.Done():
	default:
		t.Fatal("watcher context was not cancelled")
	}
	_, _, released := pool.snapshot()
	if len(released) != 2 || released[0] != "lease-stray" || released[1] != "lease-held" {
		t.Fatalf("released = %v", released)
	}
}

func TestSharedPoolAcquireLeaseUnlockedPaths(t *testing.T) {
	m, a := newSharedPoolManager(t, nil)
	ctx := context.Background()
	if lease, fresh, err := m.acquireSharedPoolLeaseUnlocked(ctx, a, "scanner"); lease != "" || fresh || err != nil {
		t.Fatalf("no pool: lease=%q fresh=%v err=%v", lease, fresh, err)
	}

	pool := &recordingSharedSessionPool{}
	m.sharedPool = pool
	a.poolLeaseID = "lease-kept"
	if lease, fresh, err := m.acquireSharedPoolLeaseUnlocked(ctx, a, "scanner"); lease != "lease-kept" || fresh || err != nil {
		t.Fatalf("existing lease: lease=%q fresh=%v err=%v", lease, fresh, err)
	}

	a.poolLeaseID = ""
	lease, fresh, err := m.acquireSharedPoolLeaseUnlocked(ctx, a, "scanner")
	if lease != "lease-scanner" || !fresh || err != nil {
		t.Fatalf("fresh lease: lease=%q fresh=%v err=%v", lease, fresh, err)
	}
	// A fresh lease for a launch that will not happen goes back to the Hub.
	m.mu.Lock()
	m.abandonFreshSharedPoolLeaseLocked("scanner", "", true)
	m.abandonFreshSharedPoolLeaseLocked("scanner", lease, false)
	m.abandonFreshSharedPoolLeaseLocked("scanner", lease, true)
	m.mu.Unlock()
	if _, _, released := pool.snapshot(); len(released) != 1 || released[0] != lease {
		t.Fatalf("released = %v, want [%s]", released, lease)
	}

	pool.acquireFn = func(context.Context, string) (string, error) { return "", errSharedSessionPoolBusy }
	if _, _, err := m.acquireSharedPoolLeaseUnlocked(ctx, a, "scanner"); !errors.Is(err, errSharedSessionPoolBusy) {
		t.Fatalf("busy pool err = %v, want errSharedSessionPoolBusy", err)
	}
}

func TestNewSharedSessionPoolClientValidation(t *testing.T) {
	for _, tc := range [][3]string{{"", "h", "t"}, {"http://x", "", "t"}, {"http://x", "h", ""}} {
		if _, err := NewSharedSessionPoolClient(tc[0], tc[1], tc[2]); err == nil {
			t.Fatalf("NewSharedSessionPoolClient(%q,%q,%q) succeeded", tc[0], tc[1], tc[2])
		}
	}
	pool, err := NewSharedSessionPoolClient(" http://hub.example/ ", "school", "token")
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.(*sharedSessionPoolClient).baseURL; got != "http://hub.example" {
		t.Fatalf("baseURL = %q, want trimmed", got)
	}
}

type sharedPoolHubStub struct {
	mu       sync.Mutex
	requests []sharedPoolRequest
	methods  []string
	handle   func(n int, w http.ResponseWriter, r *http.Request, body sharedPoolRequest)
}

func (s *sharedPoolHubStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body sharedPoolRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.requests = append(s.requests, body)
	s.methods = append(s.methods, r.Method)
	n := len(s.requests)
	s.mu.Unlock()
	if r.URL.Path != sharedSessionPoolPath+"/leases" || r.Header.Get("Authorization") != "Bearer token" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.handle(n, w, r, body)
}

func newSharedPoolClientForStub(t *testing.T, stub *sharedPoolHubStub) SharedSessionPool {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	pool, err := NewSharedSessionPoolClient(srv.URL, "school", "token")
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestSharedPoolClientAcquireRetriesWhileBusy(t *testing.T) {
	stub := &sharedPoolHubStub{handle: func(n int, w http.ResponseWriter, _ *http.Request, body sharedPoolRequest) {
		if body.HiveID != "school" || body.AgentName != "scanner" {
			http.Error(w, "wrong tenant", http.StatusBadRequest)
			return
		}
		if n == 1 {
			http.Error(w, "busy", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(sharedPoolResponse{LeaseID: "lease-42"})
	}}
	pool := newSharedPoolClientForStub(t, stub)
	lease, err := pool.Acquire(context.Background(), "scanner")
	if err != nil || lease != "lease-42" {
		t.Fatalf("Acquire = %q, %v; want lease-42", lease, err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.requests) != 2 {
		t.Fatalf("requests = %d, want one busy retry then success", len(stub.requests))
	}
}

func TestSharedPoolClientAcquireGivesUpAtDeadlineAndCancelsWait(t *testing.T) {
	stub := &sharedPoolHubStub{handle: func(_ int, w http.ResponseWriter, r *http.Request, body sharedPoolRequest) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "busy", http.StatusConflict)
	}}
	pool := newSharedPoolClientForStub(t, stub)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := pool.Acquire(ctx, "scanner"); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire err = %v, want deadline exceeded", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	last := stub.requests[len(stub.requests)-1]
	if stub.methods[len(stub.methods)-1] != http.MethodDelete || !last.CancelWait || last.HiveID != "school" {
		t.Fatalf("final request = %s %+v, want DELETE with cancel_wait", stub.methods[len(stub.methods)-1], last)
	}
}

func TestSharedPoolClientAcquireErrors(t *testing.T) {
	status := http.StatusOK
	stub := &sharedPoolHubStub{handle: func(_ int, w http.ResponseWriter, _ *http.Request, _ sharedPoolRequest) {
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		_ = json.NewEncoder(w).Encode(sharedPoolResponse{})
	}}
	pool := newSharedPoolClientForStub(t, stub)
	if _, err := pool.Acquire(context.Background(), "scanner"); err == nil || !strings.Contains(err.Error(), "empty lease ID") {
		t.Fatalf("empty lease err = %v", err)
	}
	status = http.StatusForbidden
	if _, err := pool.Acquire(context.Background(), "scanner"); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("403 err = %v, want status and body", err)
	}
	// A 404 on Acquire (no lease ID in the body) is a plain HTTP error, not
	// ErrSharedSessionLeaseNotFound.
	status = http.StatusNotFound
	if _, err := pool.Acquire(context.Background(), "scanner"); err == nil || errors.Is(err, ErrSharedSessionLeaseNotFound) {
		t.Fatalf("404 acquire err = %v, want generic error", err)
	}
	down, err := NewSharedSessionPoolClient("http://127.0.0.1:1", "school", "token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := down.Acquire(context.Background(), "scanner"); err == nil {
		t.Fatal("Acquire against a closed port succeeded")
	}
}

func TestSharedPoolClientRenewAndRelease(t *testing.T) {
	stub := &sharedPoolHubStub{handle: func(_ int, w http.ResponseWriter, r *http.Request, body sharedPoolRequest) {
		if body.LeaseID != "lease-7" || body.HiveID != "school" {
			http.Error(w, "wrong lease", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusNoContent) // empty body path
		case http.MethodDelete:
			_, _ = w.Write([]byte("{}"))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}}
	pool := newSharedPoolClientForStub(t, stub)
	if err := pool.Renew(context.Background(), "lease-7"); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if err := pool.Release(context.Background(), "lease-7"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := pool.Release(context.Background(), "lease-other"); err == nil {
		t.Fatal("Release with the wrong lease succeeded")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.methods) != 3 || stub.methods[0] != http.MethodPut || stub.methods[1] != http.MethodDelete {
		t.Fatalf("methods = %v", stub.methods)
	}
}

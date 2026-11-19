package agent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

const (
	kqTestPoll    = time.Millisecond
	kqTestWait    = 5 * time.Second
	kqTestAgent   = "kq-agent"
	kqTestSession = "hive-kq-agent"
)

// newKickQueueManager builds a manager whose queue pump delivers through a
// recorder and polls fast. idle decides the agent's idleness.
func newKickQueueManager(t *testing.T, idle func(string) bool, deliver func(string, string) error) *Manager {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{kqTestAgent: {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.kickQueuePollInterval = kqTestPoll
	m.kickQueueIdleFn = idle
	m.kickQueueDeliverFn = deliver
	// Empty the FIFOs at the end so a pump parked on a never-idle agent exits
	// instead of outliving the test.
	t.Cleanup(func() {
		m.kickQueue.mu.Lock()
		for name := range m.kickQueue.pending {
			m.kickQueue.pending[name] = nil
		}
		m.kickQueue.mu.Unlock()
	})
	return m
}

func waitKickPhase(t *testing.T, m *Manager, id, phase string) crux.KickStatus {
	t.Helper()
	deadline := time.Now().Add(kqTestWait)
	for time.Now().Before(deadline) {
		if st, ok := m.KickStatus(id); ok && st.Phase == phase {
			return st
		}
		<-time.After(kqTestPoll)
	}
	st, _ := m.KickStatus(id)
	t.Fatalf("kick %s phase = %q, want %q", id, st.Phase, phase)
	return st
}

type kickRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *kickRecorder) deliver(_ string, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
	return nil
}

func (r *kickRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

func TestEnqueueKick_RejectsUnknownAndPausedAgents(t *testing.T) {
	m := newKickQueueManager(t, func(string) bool { return true }, nil)
	if _, err := m.EnqueueKick("ghost", "", "hi"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown agent err = %v, want not found", err)
	}
	m.mu.Lock()
	m.agents[kqTestAgent].Paused = true
	m.mu.Unlock()
	if _, err := m.EnqueueKick(kqTestAgent, "", "hi"); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("paused agent err = %v, want paused", err)
	}
	if n := m.KickQueueDepth(kqTestAgent); n != 0 {
		t.Fatalf("depth = %d after rejected enqueues, want 0", n)
	}
}

func TestEnqueueKick_ValidatesAndDedupesIDs(t *testing.T) {
	m := newKickQueueManager(t, func(string) bool { return false }, nil)
	for _, bad := range []string{"has space", "semi;colon", strings.Repeat("a", maxKickIDLen+1)} {
		if _, err := m.EnqueueKick(kqTestAgent, bad, "hi"); !errors.Is(err, crux.ErrInvalidKickID) {
			t.Errorf("id %q err = %v, want ErrInvalidKickID", bad, err)
		}
	}
	id, err := m.EnqueueKick(kqTestAgent, "meeting-1:r.1", "hi")
	if err != nil || id != "meeting-1:r.1" {
		t.Fatalf("caller id: id=%q err=%v", id, err)
	}
	if _, err := m.EnqueueKick(kqTestAgent, "meeting-1:r.1", "again"); !errors.Is(err, crux.ErrKickIDInUse) {
		t.Fatalf("duplicate id err = %v, want ErrKickIDInUse", err)
	}
	gen, err := m.EnqueueKick(kqTestAgent, "", "hi")
	if err != nil || !strings.HasPrefix(gen, generatedKickIDPrefix) || len(gen) != len(generatedKickIDPrefix)+2*generatedKickIDBytes {
		t.Fatalf("generated id = %q err = %v", gen, err)
	}
	st, ok := m.KickStatus(gen)
	if !ok || st.Phase != crux.KickPhaseQueued || st.Agent != kqTestAgent || st.QueuedAt.IsZero() {
		t.Fatalf("status = %+v ok=%v, want queued for %s", st, ok, kqTestAgent)
	}
	if _, ok := m.KickStatus("nope"); ok {
		t.Fatal("unknown kick id reported a status")
	}
}

func TestEnqueueKick_BoundedQueue(t *testing.T) {
	t.Setenv(kickQueueSizeEnv, "2")
	m := newKickQueueManager(t, func(string) bool { return false }, nil)
	for i := 0; i < 2; i++ {
		if _, err := m.EnqueueKick(kqTestAgent, "", "hi"); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if _, err := m.EnqueueKick(kqTestAgent, "", "hi"); !errors.Is(err, crux.ErrKickQueueFull) {
		t.Fatalf("third enqueue err = %v, want ErrKickQueueFull", err)
	}
	if n := m.KickQueueDepth(kqTestAgent); n != 2 {
		t.Fatalf("depth = %d, want 2", n)
	}
}

func TestKickQueue_DeliversFIFOOneTurnAtATime(t *testing.T) {
	rec := &kickRecorder{}
	var mu sync.Mutex
	idle := true
	m := newKickQueueManager(t, func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		return idle
	}, func(name, msg string) error {
		mu.Lock()
		idle = false // the delivered kick starts a turn
		mu.Unlock()
		return rec.deliver(name, msg)
	})
	kickAt := time.Now()
	m.mu.Lock()
	m.agents[kqTestAgent].LastKick = &kickAt
	m.mu.Unlock()

	first, _ := m.EnqueueKick(kqTestAgent, "first", "one")
	second, _ := m.EnqueueKick(kqTestAgent, "second", "two")

	st := waitKickPhase(t, m, first, crux.KickPhaseDelivered)
	if st.DeliveredAt.IsZero() {
		t.Fatal("delivered kick has no DeliveredAt")
	}
	if got := m.InFlightKickID(kqTestAgent); got != first {
		t.Fatalf("in-flight = %q, want %q", got, first)
	}
	if s, _ := m.KickStatus(second); s.Phase != crux.KickPhaseQueued {
		t.Fatalf("second phase = %q while first turn runs, want queued", s.Phase)
	}

	// An outcome for an EARLIER turn must not complete the in-flight kick.
	m.kickQueue.turnEnded(kqTestAgent, KickOutcome{Kind: KickOutcomeEnded, KickAt: kickAt.Add(-time.Minute)}, time.Now())
	if got := m.InFlightKickID(kqTestAgent); got != first {
		t.Fatalf("stale outcome cleared in-flight kick: %q", got)
	}

	m.notifyKickOutcome(kqTestAgent, KickOutcome{Kind: KickOutcomeNoOp, KickAt: kickAt})
	done := waitKickPhase(t, m, first, crux.KickPhaseCompleted)
	if done.Outcome != KickOutcomeNoOp || done.SettledAt.IsZero() {
		t.Fatalf("completed status = %+v", done)
	}
	mu.Lock()
	idle = true
	mu.Unlock()
	waitKickPhase(t, m, second, crux.KickPhaseDelivered)
	if got := rec.got(); fmt.Sprint(got) != "[one two]" {
		t.Fatalf("delivery order = %v, want [one two]", got)
	}
	if n := m.KickQueueDepth(kqTestAgent); n != 0 {
		t.Fatalf("depth = %d after draining, want 0", n)
	}
	if got := m.InFlightKickID("ghost"); got != "" {
		t.Fatalf("in-flight for unknown agent = %q", got)
	}
}

func TestKickQueue_BusyRequeuesAndFailureSettles(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	m := newKickQueueManager(t, func(string) bool { return true }, func(_, msg string) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if msg == "busy-once" && calls == 1 {
			return fmt.Errorf("%w: typing", crux.ErrAgentBusy)
		}
		if msg == "broken" {
			return errors.New("tmux exploded")
		}
		return nil
	})
	busy, _ := m.EnqueueKick(kqTestAgent, "", "busy-once")
	waitKickPhase(t, m, busy, crux.KickPhaseDelivered)
	mu.Lock()
	if calls != 2 {
		t.Fatalf("deliver calls = %d, want a retry after the busy refusal", calls)
	}
	mu.Unlock()

	// Complete the in-flight one so the injected idle check is the only gate.
	m.kickQueue.turnEnded(kqTestAgent, KickOutcome{Kind: KickOutcomeEnded}, time.Now())
	broken, _ := m.EnqueueKick(kqTestAgent, "", "broken")
	st := waitKickPhase(t, m, broken, crux.KickPhaseFailed)
	if st.Error != "tmux exploded" {
		t.Fatalf("failure error = %q", st.Error)
	}
	if got := m.InFlightKickID(kqTestAgent); got != "" {
		t.Fatalf("failed kick left in-flight id %q", got)
	}
}

func TestKickQueue_WaitTimeoutFails(t *testing.T) {
	t.Setenv(kickQueueWaitTimeoutEnv, "1ms")
	m := newKickQueueManager(t, func(string) bool { return false }, nil)
	id, _ := m.EnqueueKick(kqTestAgent, "", "never")
	st := waitKickPhase(t, m, id, crux.KickPhaseFailed)
	if !strings.Contains(st.Error, "did not become idle") {
		t.Fatalf("error = %q, want idle timeout", st.Error)
	}
}

func TestKickQueueAgentIdle_RealChecks(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{kqTestAgent: {Backend: "claude"}}, discardLogger(), ProjectContext{})
	now := time.Now()
	set := func(fn func(a *AgentProcess)) {
		m.mu.Lock()
		fn(m.agents[kqTestAgent])
		m.mu.Unlock()
	}

	if m.kickQueueAgentIdle("ghost", now) {
		t.Error("unknown agent reported idle")
	}
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("never-started agent reported idle")
	}
	set(func(a *AgentProcess) { a.State = StateRunning; a.tmuxSession = kqTestSession })
	if !m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("running agent with no kick reported busy")
	}

	kickAt := now.Add(-time.Second)
	set(func(a *AgentProcess) { a.LastKick = &kickAt })
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("agent mid-turn reported idle")
	}
	set(func(a *AgentProcess) { a.KickOutcome = KickOutcome{Kind: KickOutcomeEnded, KickAt: kickAt} })
	if !m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("agent whose turn ended reported busy")
	}

	set(func(a *AgentProcess) { a.kickDelivering.Store(true) })
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("agent being typed into reported idle")
	}
	set(func(a *AgentProcess) { a.kickDelivering.Store(false) })

	if _, fresh := m.kickDispatches.begin(kqTestAgent); !fresh {
		t.Fatal("setup: dispatch begin not fresh")
	}
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("agent with a pending async dispatch reported idle")
	}
	m.kickDispatches.settle(kqTestAgent, KickPhaseDelivered, "")

	set(func(a *AgentProcess) { a.Paused = true })
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("paused agent reported idle")
	}
	set(func(a *AgentProcess) { a.Paused = false })

	// An in-flight queued kick holds the agent busy until its turn timeout,
	// then is failed and released.
	t.Setenv(kickQueueTurnTimeoutEnv, "1m")
	m.kickQueue.mu.Lock()
	m.kickQueue.initLocked()
	m.kickQueue.inflight[kqTestAgent] = &queuedKick{id: "slow", agent: kqTestAgent}
	m.kickQueue.recordLocked(&crux.KickStatus{ID: "slow", Agent: kqTestAgent, Phase: crux.KickPhaseDelivered, DeliveredAt: now})
	m.kickQueue.mu.Unlock()
	if m.kickQueueAgentIdle(kqTestAgent, now) {
		t.Error("agent running a queued turn reported idle")
	}
	if !m.kickQueueAgentIdle(kqTestAgent, now.Add(2*time.Minute)) {
		t.Error("agent past the turn timeout reported busy")
	}
	st, _ := m.KickStatus("slow")
	if st.Phase != crux.KickPhaseFailed || !strings.Contains(st.Error, "turn did not end") {
		t.Errorf("timed-out turn status = %+v", st)
	}
}

func TestDeliverQueuedKick_Preconditions(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{kqTestAgent: {Backend: "claude"}}, discardLogger(), ProjectContext{})
	if err := m.deliverQueuedKick("ghost", "hi"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown agent err = %v", err)
	}
	if err := m.deliverQueuedKick(kqTestAgent, "hi"); err == nil || !strings.Contains(err.Error(), "cannot be kicked") {
		t.Fatalf("idle agent err = %v, want cannot be kicked", err)
	}

	m.mu.Lock()
	m.agents[kqTestAgent].State = StateRunning
	m.agents[kqTestAgent].tmuxSession = kqTestSession
	m.mu.Unlock()
	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })
	if _, fresh := m.kickDispatches.begin(kqTestAgent); !fresh {
		t.Fatal("setup: dispatch begin not fresh")
	}
	if err := m.deliverQueuedKick(kqTestAgent, "hi"); !errors.Is(err, crux.ErrAgentBusy) {
		t.Fatalf("in-flight dispatch err = %v, want ErrAgentBusy", err)
	}
}

func TestKickQueue_RetentionEvictsOldestTerminal(t *testing.T) {
	var q kickQueue
	q.initLocked()
	now := time.Now()
	q.recordLocked(&crux.KickStatus{ID: "live", Phase: crux.KickPhaseQueued})
	for i := 0; i < maxRetainedKickStatuses; i++ {
		q.recordLocked(&crux.KickStatus{ID: fmt.Sprintf("done-%d", i), Phase: crux.KickPhaseCompleted, SettledAt: now})
	}
	if len(q.statuses) != maxRetainedKickStatuses {
		t.Fatalf("retained %d statuses, want %d", len(q.statuses), maxRetainedKickStatuses)
	}
	if _, ok := q.statuses["live"]; !ok {
		t.Fatal("a non-terminal status was evicted")
	}
	if _, ok := q.statuses["done-0"]; ok {
		t.Fatal("the oldest terminal status was not evicted")
	}
	// Settling an unknown or terminal id is a no-op.
	q.settleLocked("missing", crux.KickPhaseFailed, "x", "", now)
	q.settleLocked("done-1", crux.KickPhaseFailed, "x", "", now)
	if q.statuses["done-1"].Phase != crux.KickPhaseCompleted {
		t.Fatal("a terminal status was overwritten")
	}
}

func TestKickQueueEnvDefaults(t *testing.T) {
	t.Setenv(kickQueueSizeEnv, "nope")
	t.Setenv(kickQueueWaitTimeoutEnv, "-1s")
	t.Setenv(kickQueueTurnTimeoutEnv, "")
	if kickQueueSize() != defaultKickQueueSize {
		t.Error("invalid size did not fall back to the default")
	}
	if kickQueueWaitTimeout() != defaultKickQueueWaitTimeout {
		t.Error("negative wait timeout did not fall back to the default")
	}
	if kickQueueTurnTimeout() != defaultKickQueueTurnTimeout {
		t.Error("empty turn timeout did not fall back to the default")
	}
	m := &Manager{}
	if m.kickQueuePollEvery() != defaultKickQueuePollInterval {
		t.Error("zero poll interval did not fall back to the default")
	}
}

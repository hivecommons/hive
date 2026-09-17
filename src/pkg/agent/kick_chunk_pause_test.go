package agent

// Tests for #7417: the inter-chunk pause during kick delivery must not hold
// the manager lock.
//
// A kick carrying a large ${PR_LIST} runs to tens of kilobytes, and at
// chunkSize per chunkDelay that is minutes of typing. Measured on the
// projectbluefin spoke with max_prs: 350, the lock was held for 209 seconds,
// during which the dashboard was unresponsive, the hub heartbeat timed out and
// shipped LAST-GOOD stale stats, and the LLM proxy dropped other agents'
// in-flight requests.

import (
	"testing"
	"time"
)

// The lock is genuinely released for the duration of the pause: a second
// goroutine must be able to take m.mu while a chunk pause is in flight. This
// is the whole point of the change — if this regresses, the fleet-wide stall
// is back and nothing else in this file will notice.
func TestKickChunkPause_ReleasesManagerLock(t *testing.T) {
	m := testManager(2)
	agent := &AgentProcess{Name: "scanner"}
	m.agents["scanner"] = agent

	m.mu.Lock()

	// Launched while this goroutine already holds m.mu, so the contender's
	// Lock cannot possibly succeed until the chunk pause releases it. That
	// ordering is the setup — no timing margin is needed to establish it.
	started := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(started)
		m.mu.Lock()
		close(acquired)
		m.mu.Unlock()
	}()
	<-started

	ok := m.kickChunkPauseUnlocked(agent, agent.kickEpoch, 120*time.Millisecond)

	// Checked BEFORE releasing the lock, which is what makes this a real
	// assertion: if the pause held m.mu throughout, the contender is still
	// blocked right now and this channel is still open. Checking after the
	// unlock would pass either way.
	select {
	case <-acquired:
	default:
		m.mu.Unlock()
		<-acquired
		t.Fatal("another goroutine could not take m.mu during the chunk pause — the lock is still held across delivery (#7417)")
	}

	m.mu.Unlock()
	<-acquired

	if !ok {
		t.Fatal("pause reported the agent changed, but nothing changed")
	}
}

// The contract every caller depends on: the lock is held again on return, on
// both the true and the false path, so deliverKickLocked keeps its `Locked`
// suffix honest.
func TestKickChunkPause_ReturnsHoldingTheLock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(m *Manager, a *AgentProcess)
		wantOK  bool
		wantWhy string
	}{
		{
			name:    "unchanged",
			mutate:  func(*Manager, *AgentProcess) {},
			wantOK:  true,
			wantWhy: "nothing changed",
		},
		{
			name:    "restarted",
			mutate:  func(_ *Manager, a *AgentProcess) { a.kickEpoch++ },
			wantOK:  false,
			wantWhy: "the agent restarted, so the remaining chunks belong to a dead session",
		},
		{
			name:    "removed",
			mutate:  func(m *Manager, _ *AgentProcess) { delete(m.agents, "scanner") },
			wantOK:  false,
			wantWhy: "the agent is gone",
		},
		{
			// A replacement AgentProcess starts its kickEpoch at zero, so an
			// epoch-only comparison would alias it as unchanged and keep typing
			// into a pane belonging to a different run.
			name: "replaced with a fresh object at the same epoch",
			mutate: func(m *Manager, _ *AgentProcess) {
				m.agents["scanner"] = &AgentProcess{Name: "scanner"}
			},
			wantOK:  false,
			wantWhy: "a different AgentProcess now owns this name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testManager(2)
			agent := &AgentProcess{Name: "scanner"}
			m.agents["scanner"] = agent
			epoch := agent.kickEpoch

			m.mu.Lock()

			// Same ordering as above: the mutator is launched while m.mu is
			// held, so its Lock is satisfied only once the pause releases the
			// mutex. The mutation therefore lands inside the pause window
			// without needing a timing margin to arrange it.
			started := make(chan struct{})
			done := make(chan struct{})
			go func() {
				close(started)
				m.mu.Lock()
				tc.mutate(m, agent)
				m.mu.Unlock()
				close(done)
			}()
			<-started

			got := m.kickChunkPauseUnlocked(agent, epoch, 80*time.Millisecond)
			// If the lock were not held here this would deadlock or panic on
			// unlock of an unlocked mutex.
			m.mu.Unlock()
			<-done

			if got != tc.wantOK {
				t.Errorf("pause = %v, want %v — %s", got, tc.wantOK, tc.wantWhy)
			}
		})
	}
}

// Concurrent delivery to one pane must be refused. Releasing m.mu across the
// pauses makes two typists for the same agent reachable for the first time;
// interleaved chunks would produce a single corrupt prompt.
func TestKickDelivering_ClaimIsExclusive(t *testing.T) {
	agent := &AgentProcess{Name: "scanner"}

	if !agent.kickDelivering.CompareAndSwap(false, true) {
		t.Fatal("first claim was refused on an idle pane")
	}
	if agent.kickDelivering.CompareAndSwap(false, true) {
		t.Error("a second delivery claimed a pane that was already being typed into")
	}

	agent.kickDelivering.Store(false)
	if !agent.kickDelivering.CompareAndSwap(false, true) {
		t.Error("the pane was not reclaimable after delivery finished")
	}
}

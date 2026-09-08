package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for the slow half of the asynchronous kick path (#5325):
// deliverKickAsync's re-validation branches and SendKickAsync's synchronous
// rejection branches that kick_async_test.go does not reach.
//
// deliverKickAsync runs on its own goroutine in production, AFTER the
// preconditions passed — so every branch here is a state change that happened
// between queueing and delivery (agent removed, paused, provider backoff
// raised) or a genuine delivery failure (CLI never reached its prompt). Those
// are exactly the outcomes the dashboard renders from KickDispatchState, so
// each one must return a distinct, accurate error.

// newKickAsyncManager builds a manager with one named agent pinned to the
// shared test tmux socket so tmux exec routing is hermetic on live hive hosts.
func newKickAsyncManager(t *testing.T, name string) *Manager {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		name: {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, name)
	return m
}

// setAgentRunning transitions the test agent to StateRunning and binds it to
// the given tmux session, mirroring what Launch would have recorded.
func setAgentRunning(t *testing.T, m *Manager, name, session string) *AgentProcess {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		t.Fatalf("agent %q not found", name)
	}
	agent.State = StateRunning
	agent.tmuxSession = session
	return agent
}

// TestDeliverKickAsync_UnknownAgent covers the first re-check: the agent was
// removed between SendKickAsync queueing the dispatch and the delivery
// goroutine starting. The delivery must fail with "not found", not panic on a
// nil map entry.
func TestDeliverKickAsync_UnknownAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	err := m.deliverKickAsync("ghost", "hello")
	if err == nil {
		t.Fatal("deliverKickAsync for an unknown agent returned no error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to say the agent was not found", err)
	}
}

// TestDeliverKickAsync_NotRunningAgent covers the liveness re-check: the agent
// was stopped after the kick was queued. The error must carry
// notRunningReason's explanation so the dashboard shows WHY, not just that it
// failed.
func TestDeliverKickAsync_NotRunningAgent(t *testing.T) {
	m := newKickAsyncManager(t, "kasync-stopped")

	m.mu.Lock()
	m.agents["kasync-stopped"].State = StateStopped
	m.mu.Unlock()

	err := m.deliverKickAsync("kasync-stopped", "hello")
	if err == nil {
		t.Fatal("deliverKickAsync for a stopped agent returned no error")
	}
	if !strings.Contains(err.Error(), "cannot be kicked") || !strings.Contains(err.Error(), "it is stopped") {
		t.Errorf("error = %q, want the notRunningReason explanation", err)
	}
}

// TestDeliverKickAsync_ProviderBackoffBlocksDelivery covers the backoff
// re-check at delivery time: a provider error was classified between queueing
// and delivery. Typing a kick into a CLI that just surfaced a provider error
// would burn the probe budget, so delivery must refuse with the backoff
// details.
func TestDeliverKickAsync_ProviderBackoffBlocksDelivery(t *testing.T) {
	m := newKickAsyncManager(t, "kasync-backoff")

	m.mu.Lock()
	agent := m.agents["kasync-backoff"]
	agent.State = StateRunning
	agent.ProviderErrorClass = "overloaded"
	agent.ProviderErrorLine = "API Error: 529 overloaded_error"
	agent.ProviderErrorBackoffUntil = time.Now().Add(10 * time.Minute)
	m.mu.Unlock()

	err := m.deliverKickAsync("kasync-backoff", "hello")
	if err == nil {
		t.Fatal("deliverKickAsync during provider backoff returned no error")
	}
	if !strings.Contains(err.Error(), "blocked: inference (overloaded)") {
		t.Errorf("error = %q, want the provider backoff class and line", err)
	}
}

// TestSendKickAsync_ProviderBackoffFailsSynchronously covers the synchronous
// backoff precondition: a kick during an active provider backoff is a
// definitive, instant failure — it must be reported on the caller's goroutine
// and must NOT leave a dispatch record implying something was queued.
func TestSendKickAsync_ProviderBackoffFailsSynchronously(t *testing.T) {
	m := newKickAsyncManager(t, "kasync-syncbackoff")

	m.mu.Lock()
	agent := m.agents["kasync-syncbackoff"]
	agent.State = StateRunning
	agent.ProviderErrorClass = "rate-limit"
	agent.ProviderErrorLine = "API Error: 429 rate_limited"
	agent.ProviderErrorBackoffUntil = time.Now().Add(10 * time.Minute)
	m.mu.Unlock()

	started, err := m.SendKickAsync("kasync-syncbackoff", "hello")
	if err == nil {
		t.Fatal("SendKickAsync during provider backoff returned no error")
	}
	if started {
		t.Error("SendKickAsync reported a started delivery during provider backoff")
	}
	if !strings.Contains(err.Error(), "blocked: inference (rate-limit)") {
		t.Errorf("error = %q, want the provider backoff class and line", err)
	}
	if _, ok := m.KickDispatchState("kasync-syncbackoff"); ok {
		t.Error("a synchronously rejected kick left a dispatch record")
	}
}

// TestSendKickAsync_MissingTmuxSessionFails covers the tmux precondition: a
// running agent whose session is gone is definitively un-kickable and must
// fail synchronously, naming the missing session.
func TestSendKickAsync_MissingTmuxSessionFails(t *testing.T) {
	m := newKickAsyncManager(t, "kasync-nosession")
	setAgentRunning(t, m, "kasync-nosession", "hive-kasync-does-not-exist")

	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return false }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	started, err := m.SendKickAsync("kasync-nosession", "hello")
	if err == nil {
		t.Fatal("SendKickAsync with no tmux session returned no error")
	}
	if started {
		t.Error("SendKickAsync reported a started delivery with no tmux session")
	}
	if !strings.Contains(err.Error(), "tmux session hive-kasync-does-not-exist not found") {
		t.Errorf("error = %q, want it to name the missing session", err)
	}
	if _, ok := m.KickDispatchState("kasync-nosession"); ok {
		t.Error("a synchronously rejected kick left a dispatch record")
	}
}

// TestSendKickAsync_DedupesWhileInFlight covers the exactly-once guard at the
// SendKickAsync level: a second call while a dispatch is pending must return
// started=false with NO error (the operator's second click is a no-op, not a
// failure) and must not disturb the in-flight dispatch.
func TestSendKickAsync_DedupesWhileInFlight(t *testing.T) {
	m := newKickAsyncManager(t, "kasync-dedupe")
	setAgentRunning(t, m, "kasync-dedupe", "hive-kasync-dedupe")

	origExists := tmuxSessionExists
	tmuxSessionExists = func(*Manager, *AgentProcess) bool { return true }
	t.Cleanup(func() { tmuxSessionExists = origExists })

	// Claim the in-flight slot as a running delivery would have.
	if _, fresh := m.kickDispatches.begin("kasync-dedupe"); !fresh {
		t.Fatal("test setup: initial begin was not fresh")
	}

	started, err := m.SendKickAsync("kasync-dedupe", "hello again")
	if err != nil {
		t.Fatalf("SendKickAsync while in flight returned an error: %v", err)
	}
	if started {
		t.Error("SendKickAsync started a second delivery while one was in flight — the prompt would be typed twice")
	}

	d, ok := m.KickDispatchState("kasync-dedupe")
	if !ok {
		t.Fatal("the in-flight dispatch disappeared")
	}
	if !d.Pending() {
		t.Errorf("the in-flight dispatch was settled by the deduplicated call: phase = %q", d.Phase)
	}
}

// TestDeliverKickAsync_PromptTimeoutFails is the genuine delivery failure: the
// pane shows a CLI marker (so no restart is attempted) but the input prompt
// never appears within inputPromptTimeout. Moving the wait off the request
// path must not launder this into a success — it must surface as a failure
// with the "did not reach input prompt" reason the registry records.
func TestDeliverKickAsync_PromptTimeoutFails(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := newKickAsyncManager(t, "kasync-noprompt")
	setAgentRunning(t, m, "kasync-noprompt", "hive-kasync-noprompt")

	newRawTmuxSession(t, "hive-kasync-noprompt")
	// "Claude" is a cliPaneMarkers entry but NOT a paneShowsInputPrompt
	// marker: the CLI counts as alive (no restart) yet never ready.
	paneInject(t, "hive-kasync-noprompt", "Claude Code is starting")

	err := m.deliverKickAsync("kasync-noprompt", "hello")
	if err == nil {
		t.Fatal("deliverKickAsync with no input prompt returned no error")
	}
	if !strings.Contains(err.Error(), "did not reach input prompt") {
		t.Errorf("error = %q, want the input-prompt timeout reason", err)
	}
}

// TestDeliverKickAsync_AgentDisappearsDuringPromptWait covers the final
// re-check: the agent is deleted while the delivery goroutine is parked in the
// prompt wait. The delivery must notice on relock and fail with "disappeared",
// not type into a pane the manager no longer owns.
//
// Ordering is made deterministic by the pane itself: the prompt marker is only
// injected AFTER the agent has been deleted, so the wait cannot return before
// the deletion has happened.
func TestDeliverKickAsync_AgentDisappearsDuringPromptWait(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := newKickAsyncManager(t, "kasync-vanish")
	setAgentRunning(t, m, "kasync-vanish", "hive-kasync-vanish")

	newRawTmuxSession(t, "hive-kasync-vanish")
	// Alive but not ready: marker without a prompt keeps deliverKickAsync
	// parked in waitForInputPromptForAgent.
	paneInject(t, "hive-kasync-vanish", "Claude Code is starting")

	go func() {
		time.Sleep(1 * time.Second)
		m.mu.Lock()
		delete(m.agents, "kasync-vanish")
		m.mu.Unlock()
		// Only now let the prompt appear. Errors are ignored: if injection
		// fails the wait times out and the test fails on the error text below.
		_ = testTmuxCommand("send-keys", "-t", "hive-kasync-vanish", "-l", ": ❯ ready").Run()
		_ = testTmuxCommand("send-keys", "-t", "hive-kasync-vanish", "Enter").Run()
	}()

	err := m.deliverKickAsync("kasync-vanish", "hello")
	if err == nil {
		t.Fatal("deliverKickAsync for a deleted agent returned no error")
	}
	if !strings.Contains(err.Error(), "disappeared while waiting for input prompt") {
		t.Errorf("error = %q, want the disappeared-during-wait reason", err)
	}
}

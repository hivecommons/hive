package agent

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Tests for the asynchronous kick path (#5325).
//
// The property that matters most is exactly-once delivery. The whole reason
// this path exists is that operators, shown a false 504 failure, clicked Kick
// again — so a fix that returned fast but allowed two deliveries would have
// made the real damage (duplicate advisory comments and beads on a hold-gated
// lane) easier to cause, not harder.

// TestSendKickAsync_RejectsUnknownAgentSynchronously asserts a definitively
// impossible kick still fails on the caller's goroutine. Preconditions that are
// instant and deterministic must not be deferred behind a 202.
func TestSendKickAsync_RejectsUnknownAgentSynchronously(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	started, err := m.SendKickAsync("ghost", "hello")
	if err == nil {
		t.Fatal("SendKickAsync for an unknown agent returned no error")
	}
	if started {
		t.Error("SendKickAsync reported a started delivery for an unknown agent")
	}
	if _, ok := m.KickDispatchState("ghost"); ok {
		t.Error("a rejected kick left a dispatch record; nothing was ever queued")
	}
}

// TestSendKickAsync_RejectsNotRunningAgentSynchronously covers the state the
// dashboard sees most often for a genuinely failed kick: the agent exists but
// is not running. That is still a 400-worthy synchronous error, not a queue.
func TestSendKickAsync_RejectsNotRunningAgentSynchronously(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	started, err := m.SendKickAsync("scanner", "hello")
	if err == nil {
		t.Fatal("SendKickAsync for a non-running agent returned no error")
	}
	if started {
		t.Error("SendKickAsync reported a started delivery for a non-running agent")
	}
}

// TestKickDispatchRegistry_DedupesInFlightDelivery is the exactly-once
// guarantee, tested at the guard itself so it does not need a live tmux pane.
// The second begin() for an agent whose dispatch is still pending must return
// fresh=false, which is what stops handleKick from spawning a second delivery.
func TestKickDispatchRegistry_DedupesInFlightDelivery(t *testing.T) {
	var r kickDispatchRegistry

	first, fresh := r.begin("scanner")
	if !fresh {
		t.Fatal("first begin was not fresh")
	}
	if !first.Pending() {
		t.Fatalf("first dispatch phase = %q, want pending", first.Phase)
	}

	second, fresh := r.begin("scanner")
	if fresh {
		t.Error("a second begin while pending was treated as fresh — the prompt would be delivered twice")
	}
	if second != first {
		t.Error("the deduplicated call did not return the in-flight dispatch")
	}

	// A different agent is independent: dedup is per agent, not global.
	if _, fresh := r.begin("quality"); !fresh {
		t.Error("a kick for a different agent was wrongly deduplicated")
	}
}

// TestKickDispatchRegistry_SettlesAndAllowsNextKick asserts a settled dispatch
// stops being pending, records its outcome, and releases the in-flight slot so
// a legitimate LATER kick is not blocked forever by the previous one.
func TestKickDispatchRegistry_SettlesAndAllowsNextKick(t *testing.T) {
	var r kickDispatchRegistry

	r.begin("scanner")
	r.settle("scanner", KickPhaseDelivered, "")

	got, ok := r.get("scanner")
	if !ok {
		t.Fatal("no dispatch recorded")
	}
	if got.Phase != KickPhaseDelivered {
		t.Errorf("phase = %q, want %q", got.Phase, KickPhaseDelivered)
	}
	if got.Pending() {
		t.Error("a delivered dispatch still reports pending")
	}
	if got.SettledAt.IsZero() {
		t.Error("a settled dispatch has no SettledAt")
	}
	if got.Error != "" {
		t.Errorf("a delivered dispatch carries an error: %q", got.Error)
	}

	if _, fresh := r.begin("scanner"); !fresh {
		t.Error("a kick after the previous one settled was wrongly deduplicated")
	}
}

// TestKickDispatchRegistry_SettleIsIdempotent guards the delivery goroutine's
// only write. A late second settle (say, from a retry path added later) must
// not overwrite a recorded success with a failure — that would resurrect the
// exact symptom in the issue, a succeeded kick reported as failed.
func TestKickDispatchRegistry_SettleIsIdempotent(t *testing.T) {
	var r kickDispatchRegistry

	r.begin("scanner")
	r.settle("scanner", KickPhaseDelivered, "")
	r.settle("scanner", KickPhaseFailed, "late bogus failure")

	got, _ := r.get("scanner")
	if got.Phase != KickPhaseDelivered {
		t.Errorf("phase = %q after a late settle, want %q to survive", got.Phase, KickPhaseDelivered)
	}
	if got.Error != "" {
		t.Errorf("a late settle injected an error onto a delivered kick: %q", got.Error)
	}
}

// TestKickDispatchRegistry_RecordsFailureReason asserts a genuine delivery
// failure — the CLI never reaching its input prompt within inputPromptTimeout —
// is still reported as a failure with its reason intact. Moving the wait off
// the request path must not lose real failures.
func TestKickDispatchRegistry_RecordsFailureReason(t *testing.T) {
	var r kickDispatchRegistry

	r.begin("scanner")
	r.settle("scanner", KickPhaseFailed, "agent scanner CLI did not reach input prompt")

	got, _ := r.get("scanner")
	if got.Phase != KickPhaseFailed {
		t.Fatalf("phase = %q, want %q", got.Phase, KickPhaseFailed)
	}
	if got.Pending() {
		t.Error("a failed dispatch still reports pending")
	}
	if got.Error == "" {
		t.Error("a failed dispatch lost its reason")
	}
}

// TestSendKickAsync_DeliversAndSettlesDelivered is the end-to-end success path
// against a real, ready tmux pane: the call returns promptly, and the delivery
// settles as delivered out of band.
func TestSendKickAsync_DeliversAndSettlesDelivered(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	m := NewManager(map[string]config.AgentConfig{
		"cxa": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["cxa"]
	m.mu.RUnlock()

	session := "hive-asynckick-ready"
	agent.tmuxSession = session
	newRawTmuxSession(t, session)
	paneInject(t, session, "goose is ready")

	m.mu.Lock()
	agent.State = StateRunning
	m.mu.Unlock()

	started, err := m.SendKickAsync("cxa", "do the work")
	if err != nil {
		t.Fatalf("SendKickAsync: %v", err)
	}
	if !started {
		t.Fatal("SendKickAsync did not start a delivery")
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		d, ok := m.KickDispatchState("cxa")
		if ok && !d.Pending() {
			if d.Phase != KickPhaseDelivered {
				t.Fatalf("dispatch phase = %q (%s), want %q", d.Phase, d.Error, KickPhaseDelivered)
			}
			m.mu.RLock()
			lastMsg := agent.LastKickMessage
			m.mu.RUnlock()
			if lastMsg != "do the work" {
				t.Errorf("LastKickMessage = %q", lastMsg)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("kick dispatch never settled")
}

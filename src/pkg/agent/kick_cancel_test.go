package agent

// Tests for #7363: a restart cancels the agent's pending kick instead of
// letting it replay into the relaunched session, and a restart that destroyed
// a producing turn holds the next kick so the restart/kick loop has a breaker.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

// TestKickDispatchRegistry_CancelFailsPending: cancel fails only a PENDING
// dispatch, records the reason, and reports whether anything was pending, so
// the restart path can log the cancellation exactly when one happened.
func TestKickDispatchRegistry_CancelFailsPending(t *testing.T) {
	var r kickDispatchRegistry
	if r.cancel("scanner", "cancelled: nothing") {
		t.Error("cancel with no dispatch reported a cancellation")
	}
	d, _ := r.begin("scanner")
	if !r.cancel("scanner", "cancelled: agent restarted (operator)") {
		t.Fatal("cancel did not report the pending dispatch")
	}
	got, _ := r.get("scanner")
	if got.Phase != KickPhaseFailed || !strings.Contains(got.Error, "restarted") || got.SettledAt.IsZero() {
		t.Errorf("cancelled dispatch = %+v, want failed with the restart reason", got)
	}
	if r.cancel("scanner", "again") {
		t.Error("a settled dispatch must not be cancelled twice")
	}
	if d.Pending() || d.Error != "cancelled: agent restarted (operator)" {
		t.Errorf("the original pointer was not the one settled: %+v", d)
	}
	// A new kick after the cancellation is a fresh dispatch, not a dedupe.
	if _, fresh := r.begin("scanner"); !fresh {
		t.Error("begin after cancel was deduplicated against the cancelled dispatch")
	}
}

// TestKickDispatchRegistry_SettleDispatchDoesNotClobberNewer: the goroutine
// behind a cancelled dispatch settles ITS dispatch, never the fresh one a
// later SendKickAsync registered for the same agent.
func TestKickDispatchRegistry_SettleDispatchDoesNotClobberNewer(t *testing.T) {
	var r kickDispatchRegistry
	stale, _ := r.begin("scanner")
	r.cancel("scanner", "cancelled: agent restarted (operator)")
	fresh, ok := r.begin("scanner")
	if !ok {
		t.Fatal("expected a fresh dispatch after cancellation")
	}

	r.settleDispatch(stale, KickPhaseFailed, "agent restarted while waiting")
	got, _ := r.get("scanner")
	if !got.Pending() {
		t.Fatalf("stale goroutine's settle clobbered the live dispatch: %+v", got)
	}

	r.settleDispatch(fresh, KickPhaseDelivered, "")
	got, _ = r.get("scanner")
	if got.Phase != KickPhaseDelivered {
		t.Errorf("live dispatch phase = %q, want delivered", got.Phase)
	}
	r.settleDispatch(nil, KickPhaseFailed, "nil is a no-op")
}

func newHoldTestAgent(t *testing.T) (*Manager, *AgentProcess) {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "worker")
	m.mu.Lock()
	agent := m.agents["worker"]
	agent.State = StateRunning
	agent.tmuxSession = "hive-7363-nonexistent"
	m.mu.Unlock()
	return m, agent
}

// TestInvalidateKicksOnRestart_EpochHoldAndDispatch pins the three effects of
// the restart teardown's kick bookkeeping and the two exemptions the kick
// path's own recovery restart relies on.
func TestInvalidateKicksOnRestart_EpochHoldAndDispatch(t *testing.T) {
	m, agent := newHoldTestAgent(t)
	fixed := time.Date(2026, 9, 17, 12, 15, 22, 0, time.UTC)
	origNow := kickHoldNow
	kickHoldNow = func() time.Time { return fixed }
	t.Cleanup(func() { kickHoldNow = origNow })

	m.kickDispatches.begin("worker")

	// Kick-initiated restart: epoch moves, but the dispatch stays pending and
	// no hold is armed — the caller is about to deliver.
	m.mu.Lock()
	m.invalidateKicksOnRestartLocked(agent, "operator", false, false)
	epoch1, hold1 := agent.kickEpoch, agent.kickHoldUntil
	m.mu.Unlock()
	if epoch1 != 1 {
		t.Errorf("kickEpoch = %d after first restart, want 1", epoch1)
	}
	if !hold1.IsZero() {
		t.Error("a kick-initiated restart must not arm the breaker")
	}
	if d, _ := m.kickDispatches.get("worker"); !d.Pending() {
		t.Error("a kick-initiated restart must not fail the caller's own dispatch")
	}

	// Operator restart of a producing turn: epoch moves again, the pending
	// dispatch is failed with a reason, and the hold is armed.
	m.mu.Lock()
	m.invalidateKicksOnRestartLocked(agent, "operator", true, true)
	epoch2, holdUntil, holdReason := agent.kickEpoch, agent.kickHoldUntil, agent.kickHoldReason
	m.mu.Unlock()
	if epoch2 != 2 {
		t.Errorf("kickEpoch = %d after second restart, want 2", epoch2)
	}
	if !holdUntil.Equal(fixed.Add(restartInterruptedKickHold)) || holdReason != "operator" {
		t.Errorf("hold = (%v, %q), want (%v, operator)", holdUntil, holdReason, fixed.Add(restartInterruptedKickHold))
	}
	d, _ := m.kickDispatches.get("worker")
	if d.Phase != KickPhaseFailed || !strings.Contains(d.Error, "cancelled: agent restarted (operator)") {
		t.Errorf("pending dispatch after operator restart = %+v, want failed/cancelled", d)
	}

	// The breaker refuses inside the window and is silent after it.
	m.mu.Lock()
	inside := m.restartKickHoldErrLocked(agent, fixed.Add(10*time.Second))
	after := m.restartKickHoldErrLocked(agent, fixed.Add(restartInterruptedKickHold))
	m.mu.Unlock()
	if !errors.Is(inside, errKickHeldAfterRestart) || !strings.Contains(inside.Error(), "50s") {
		t.Errorf("hold error inside window = %v, want errKickHeldAfterRestart naming the remaining 50s", inside)
	}
	if after != nil {
		t.Errorf("hold must expire: got %v", after)
	}

	// Resetting the restart counter lifts the hold: that is the operator
	// saying "I have looked".
	if err := m.ResetRestartCount("worker"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	lifted := m.restartKickHoldErrLocked(agent, fixed.Add(time.Second))
	m.mu.Unlock()
	if lifted != nil {
		t.Errorf("ResetRestartCount must clear the hold, got %v", lifted)
	}
}

// TestSendKick_RefusedDuringRestartHold: both kick entry points refuse
// synchronously, with the hold reason, while the breaker is armed — a kick
// that lands one second after a turn-destroying restart is exactly the one
// the observed loop was made of.
func TestSendKick_RefusedDuringRestartHold(t *testing.T) {
	m, agent := newHoldTestAgent(t)
	m.mu.Lock()
	agent.kickHoldUntil = time.Now().Add(restartInterruptedKickHold)
	agent.kickHoldReason = "operator"
	m.mu.Unlock()

	err := m.SendKick("worker", "replay")
	if !errors.Is(err, errKickHeldAfterRestart) {
		t.Errorf("SendKick during hold = %v, want errKickHeldAfterRestart", err)
	}
	started, err := m.SendKickAsync("worker", "replay")
	if started || !errors.Is(err, errKickHeldAfterRestart) {
		t.Errorf("SendKickAsync during hold = (%v, %v), want (false, errKickHeldAfterRestart)", started, err)
	}
	if _, ok := m.KickDispatchState("worker"); ok {
		t.Error("a held kick must not register a dispatch")
	}
	m.mu.RLock()
	lastMsg := agent.LastKickMessage
	m.mu.RUnlock()
	if lastMsg != "" {
		t.Errorf("held kick was delivered: %q", lastMsg)
	}
}

// TestTearDownTurnLocked_ReportsProducing: the funnel reports whether it
// interrupted a turn and whether that turn was producing, which is what the
// restart path arms the breaker on.
func TestTearDownTurnLocked_ReportsProducing(t *testing.T) {
	m, agent := newHoldTestAgent(t)
	now := time.Now()
	origNow := turnLossNow
	turnLossNow = func() time.Time { return now }
	t.Cleanup(func() { turnLossNow = origNow })

	m.mu.Lock()
	defer m.mu.Unlock()
	if interrupted, producing := m.tearDownTurnLocked(agent, "restart"); interrupted || producing {
		t.Errorf("no pending kick: got (%v, %v), want (false, false)", interrupted, producing)
	}

	kickAt := now.Add(-10 * time.Second)
	agent.LastKick = &kickAt
	agent.kickLogPending = true
	if interrupted, producing := m.tearDownTurnLocked(agent, "restart"); !interrupted || producing {
		t.Errorf("idle turn: got (%v, %v), want (true, false)", interrupted, producing)
	}

	agent.kickLogPending = true
	agent.paneMu.Lock()
	agent.LastPaneChange = now.Add(-2 * time.Second) // pane moved after the kick
	agent.paneMu.Unlock()
	if interrupted, producing := m.tearDownTurnLocked(agent, "restart"); !interrupted || !producing {
		t.Errorf("producing turn: got (%v, %v), want (true, true)", interrupted, producing)
	}
	if agent.TurnLoss.Interruptions != 2 || agent.TurnLoss.Producing != 1 {
		t.Errorf("turn-loss aggregate = %+v, want 2 interruptions / 1 producing", agent.TurnLoss)
	}
}

// TestRestart_CancelsPendingAsyncKick is the end-to-end regression for the
// reported loop, against a real tmux pane: a kick is queued against a CLI that
// has not shown its prompt, the operator restarts, and the relaunched session
// must NOT receive the queued message when it finally shows a prompt. The
// dispatch must read as failed/cancelled the moment the restart runs, and the
// restart must have interrupted nothing (no kick had landed), so no hold is
// armed and a deliberate follow-up kick is still accepted.
func TestRestart_CancelsPendingAsyncKick(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	forceFastPaneShell(t)

	origPoll := inputPromptPollInterval
	inputPromptPollInterval = 200 * time.Millisecond
	t.Cleanup(func() { inputPromptPollInterval = origPoll })

	m := NewManager(map[string]config.AgentConfig{
		"worker": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["worker"]
	m.mu.RUnlock()

	// A live session whose pane shows a CLI marker but no input prompt, so
	// the async kick passes its preconditions and parks in the prompt wait.
	newRawTmuxSession(t, agent.tmuxSession)
	paneInject(t, agent.tmuxSession, "Claude Code")
	m.mu.Lock()
	agent.State = StateRunning
	m.mu.Unlock()

	wait := quiesceThenMarkReady(t, m, "worker")
	defer wait()
	defer cleanupAgent(t, m, "worker")

	started, err := m.SendKickAsync("worker", "the 71 KB prompt")
	if err != nil || !started {
		t.Fatalf("SendKickAsync = (%v, %v), want a queued delivery", started, err)
	}
	if d, ok := m.KickDispatchState("worker"); !ok || !d.Pending() {
		t.Fatalf("dispatch should be pending before the restart: %+v", d)
	}
	// Let the goroutine reach its prompt wait before the restart lands.
	time.Sleep(500 * time.Millisecond)

	if err := m.RestartWithReason(context.Background(), "worker", "operator"); err != nil {
		t.Fatalf("RestartWithReason: %v", err)
	}

	d, ok := m.KickDispatchState("worker")
	if !ok || d.Phase != KickPhaseFailed || !strings.Contains(d.Error, "cancelled: agent restarted") {
		t.Fatalf("dispatch after restart = %+v, want failed/cancelled by restart", d)
	}

	// quiesceThenMarkReady renders "goose is ready" into the relaunched pane
	// ~4s after the launch; give the (cancelled) goroutine ample time to see
	// it. The message must never land.
	time.Sleep(8 * time.Second)
	m.mu.RLock()
	lastMsg := agent.LastKickMessage
	hold := agent.kickHoldUntil
	epoch := agent.kickEpoch
	m.mu.RUnlock()
	if lastMsg != "" {
		t.Errorf("the cancelled kick was replayed into the relaunched session: %q", lastMsg)
	}
	if epoch != 1 {
		t.Errorf("kickEpoch = %d, want 1 after one restart", epoch)
	}
	if !hold.IsZero() {
		t.Error("no turn was interrupted (the kick never landed), so no hold should be armed")
	}

	// A deliberate follow-up kick is a fresh dispatch and still delivers.
	started, err = m.SendKickAsync("worker", "after the restart")
	if err != nil || !started {
		t.Fatalf("follow-up SendKickAsync = (%v, %v), want a queued delivery", started, err)
	}
	testutil.EventuallyEvery(t, 30*time.Second, 100*time.Millisecond, func() bool {
		d, ok := m.KickDispatchState("worker")
		return ok && !d.Pending()
	}, "follow-up kick never settled")
	if d, ok := m.KickDispatchState("worker"); !ok || d.Phase != KickPhaseDelivered {
		t.Fatalf("follow-up dispatch = %+v, want delivered", d)
	}
	m.mu.RLock()
	lastMsg = agent.LastKickMessage
	m.mu.RUnlock()
	if lastMsg != "after the restart" {
		t.Errorf("LastKickMessage = %q, want the follow-up kick", lastMsg)
	}
}

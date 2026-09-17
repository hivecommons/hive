package agent

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin #7421 on the manager side: how a kicked turn ENDED is
// classified from the pane the agent left at its prompt, and the verdict
// reaches the outcome observer exactly once per kick.

// The two no-op shapes observed live on the projectbluefin spoke.
const questionPane = `> Work the coverage queue described above.

⏺ I've reviewed the repository state.

  What should I focus on?
  Awaiting your kick or specific task assignment.

❯
  ? for shortcuts`

const standDownPane = `⏺ Preflight snapshot truncated — "1 additional open held PRs omitted"
  cannot verify the hold set is complete.

● STAND DOWN.
No issue opened, no PR opened, no bead created, no hold touched.

❯
  ? for shortcuts`

const productivePane = `⏺ Opened hold-gated PR #7412 adding tests for pkg/dashboard/status_builder.go.
  Filed bead b-4412 for the untested restart path.

❯
  ? for shortcuts`

func paneLines(pane string) []string {
	var out []string
	for _, l := range strings.Split(pane, "\n") {
		if t := strings.TrimRight(l, " \t"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func TestClassifyTurnTailRecognisesObservedNoOps(t *testing.T) {
	kind, reason := classifyTurnTail(paneLines(questionPane))
	if kind != KickOutcomeQuestion {
		t.Fatalf("question pane classified as %q (%q), want question", kind, reason)
	}
	if !strings.Contains(reason, "What should I focus on?") {
		t.Errorf("reason = %q, want the agent's question", reason)
	}

	kind, reason = classifyTurnTail(paneLines(standDownPane))
	if kind != KickOutcomeStandDown {
		t.Fatalf("stand-down pane classified as %q (%q), want stand-down", kind, reason)
	}
	if !strings.HasPrefix(reason, "● STAND DOWN") {
		t.Errorf("reason = %q, want the STAND DOWN line", reason)
	}

	// The explicit nothing-produced triple on its own is a no-op.
	kind, _ = classifyTurnTail([]string{"⏺ Nothing actionable in the queue.", "No issue opened, no PR opened, no bead created.", "❯"})
	if kind != KickOutcomeNoOp {
		t.Errorf("nothing-produced report classified as %q, want no-op", kind)
	}

	// A turn that produced work is merely "ended" — never a productivity claim,
	// but never a no-op either.
	kind, reason = classifyTurnTail(paneLines(productivePane))
	if kind != KickOutcomeEnded || reason != "" {
		t.Errorf("productive pane classified as %q (%q), want ended with no reason", kind, reason)
	}
}

// TestClassifyTurnTailIgnoresPolicyInstructions: the kick text a policy
// template carries is rendered into the pane too. "STAND DOWN" mid-sentence in
// an instruction, and the whole prompt echo ("> …"), must not decide the
// outcome — otherwise every kick whose template mentions standing down would
// read as a stand-down.
func TestClassifyTurnTailIgnoresPolicyInstructions(t *testing.T) {
	kind, reason := classifyTurnTail([]string{
		"> If the preflight snapshot is truncated, STAND DOWN and report it.",
		"> Never ask the operator what should I focus on; choose the next item.",
		"⏺ Preflight complete: 3 held PRs, snapshot intact.",
		"  Opened hold-gated PR #7413.",
		"❯",
	})
	if kind != KickOutcomeEnded {
		t.Errorf("echoed policy text decided the outcome: %q (%q)", kind, reason)
	}
	kind, _ = classifyTurnTail([]string{"⏺ Per policy, if the queue is empty I would stand down; it is not, so continuing.", "  Opened PR #1.", "❯"})
	if kind != KickOutcomeEnded {
		t.Errorf("a mid-sentence mention of standing down read as a stand-down: %q", kind)
	}
	// Precedence: a stated stand-down that also asks is still blocked.
	kind, _ = classifyTurnTail([]string{"● STAND DOWN — the hold set cannot be verified.", "  Let me know how you'd like to proceed.", "❯"})
	if kind != KickOutcomeStandDown {
		t.Errorf("stand-down + question classified as %q, want stand-down", kind)
	}
}

func TestPaneShowsTurnEnded(t *testing.T) {
	if !paneShowsTurnEnded(questionPane) {
		t.Error("a pane at the idle ❯ prompt must read as turn ended")
	}
	if paneShowsTurnEnded("⏺ Reading files…\n  ◉ Working (12s · esc to interrupt)\n❯") {
		t.Error("a pane with the working marker must not read as turn ended")
	}
	if paneShowsTurnEnded("") {
		t.Error("an empty capture is not a finished turn")
	}
}

func outcomeTestAgent(kickAt time.Time) *AgentProcess {
	k := kickAt
	return &AgentProcess{Name: "quality", LastKick: &k}
}

// TestKickOutcomeDueGates: the cheap poller gate refuses to classify while
// delivery is in progress, inside the post-delivery grace (the prompt is
// still visible before the model starts), before the pane has changed since
// the kick, and once this kick's turn is already settled.
func TestKickOutcomeDueGates(t *testing.T) {
	kickAt := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
	now := kickAt.Add(kickOutcomeGrace + time.Second)

	if kickOutcomeDue(&AgentProcess{Name: "quality"}, now) {
		t.Error("an agent that was never kicked has no turn to classify")
	}
	a := outcomeTestAgent(kickAt)
	if kickOutcomeDue(a, now) {
		t.Error("pane unchanged since the kick: not a finished turn (that is the stall watchdog's case)")
	}
	a.LastPaneChange = kickAt.Add(10 * time.Second)
	if kickOutcomeDue(a, kickAt.Add(kickOutcomeGrace-time.Second)) {
		t.Error("inside the post-delivery grace the prompt is the CLI accepting the kick, not a finished turn")
	}
	if !kickOutcomeDue(a, now) {
		t.Error("changed pane, grace elapsed, delivery finished: the gate must open")
	}
	a.kickDelivering.Store(true)
	if kickOutcomeDue(a, now) {
		t.Error("mid-delivery the pane has no idle chrome to judge; the gate must stay closed")
	}
	a.kickDelivering.Store(false)
	a.KickOutcome = KickOutcome{Kind: KickOutcomeEnded, KickAt: kickAt}
	if kickOutcomeDue(a, now) {
		t.Error("an already-settled kick must not be classified twice")
	}
	if !a.KickOutcome.Settled(a.LastKick) {
		t.Error("Settled must be true for the kick the outcome belongs to")
	}
	later := kickAt.Add(time.Hour)
	if a.KickOutcome.Settled(&later) {
		t.Error("an outcome for an older kick must not read as settled for a newer one")
	}
}

// TestSettleKickOutcomeNotifiesObserverOncePerKick: the verdict is stored on
// the agent, handed to the observer once, and not re-issued on later polls of
// the same idle pane; the next kick starts a fresh turn.
func TestSettleKickOutcomeNotifiesObserverOncePerKick(t *testing.T) {
	m := testManager(5)
	kickAt := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
	a := outcomeTestAgent(kickAt)
	m.agents[a.Name] = a

	var mu sync.Mutex
	var got []KickOutcome
	done := make(chan struct{}, 4)
	m.SetKickOutcomeObserver(func(name string, o KickOutcome) {
		mu.Lock()
		got = append(got, o)
		mu.Unlock()
		done <- struct{}{}
	})

	now := kickAt.Add(time.Minute)
	m.mu.Lock()
	o, settled := m.settleKickOutcomeLocked(a, "⏺ Still reading…\n  ◉ Working (4s · esc to interrupt)\n❯", now)
	m.mu.Unlock()
	if settled {
		t.Fatalf("a working pane settled the turn: %+v", o)
	}

	m.mu.Lock()
	o, settled = m.settleKickOutcomeLocked(a, questionPane, now)
	_, again := m.settleKickOutcomeLocked(a, questionPane, now.Add(time.Minute))
	m.mu.Unlock()
	if !settled || o.Kind != KickOutcomeQuestion || !o.KickAt.Equal(kickAt) || !o.At.Equal(now) {
		t.Fatalf("settle = %+v (%v), want a question outcome for this kick", o, settled)
	}
	if again {
		t.Fatal("the same idle pane settled the same kick twice")
	}
	if a.KickOutcome != o {
		t.Fatalf("outcome not stored on the agent: %+v", a.KickOutcome)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observer was not notified")
	}
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 1 || got[0].Kind != KickOutcomeQuestion {
		t.Fatalf("observer got %d verdicts %+v, want exactly one question", n, got)
	}

	// The next kick is a new turn: the old verdict no longer reads as settled
	// and the stand-down pane produces a second, different verdict.
	next := kickAt.Add(6 * time.Hour)
	a.LastKick = &next
	if a.KickOutcome.Settled(a.LastKick) {
		t.Fatal("a new kick must reopen classification")
	}
	m.mu.Lock()
	o, settled = m.settleKickOutcomeLocked(a, standDownPane, next.Add(time.Minute))
	m.mu.Unlock()
	if !settled || o.Kind != KickOutcomeStandDown {
		t.Fatalf("second turn = %+v (%v), want stand-down", o, settled)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("observer was not notified of the second verdict")
	}

	// Snapshot carries the verdict to the dashboard.
	if snap := a.snapshot(); snap.KickOutcome.Kind != KickOutcomeStandDown {
		t.Errorf("snapshot dropped the kick outcome: %+v", snap.KickOutcome)
	}

	// maybeSettleKickOutcome pays for the visible capture only once the gate
	// opens, and settles from it.
	// maybeSettleKickOutcome reads the real clock, so this kick is placed an
	// hour ago: the grace has elapsed and the gate opens.
	third := time.Now().Add(-time.Hour)
	a.LastKick = &third
	a.LastPaneChange = third.Add(time.Second)
	captures := 0
	m.maybeSettleKickOutcome(a, func() string { captures++; return productivePane })
	if captures != 1 {
		t.Fatalf("visible pane captured %d times, want 1", captures)
	}
	if a.KickOutcome.Kind != KickOutcomeEnded || !a.KickOutcome.KickAt.Equal(third) {
		t.Fatalf("maybeSettleKickOutcome outcome = %+v, want ended for the third kick", a.KickOutcome)
	}
	m.maybeSettleKickOutcome(a, func() string { captures++; return productivePane })
	if captures != 1 {
		t.Fatal("a settled kick paid for another capture")
	}
}

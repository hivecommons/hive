package hooks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietLogger keeps expected-failure tests from spamming the test output. The
// dispatcher logs failures at Error level by design, so tests that deliberately
// fail a hook would otherwise be unreadable.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type sentNotification struct {
	title, message, priority string
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []sentNotification
}

func (f *fakeNotifier) Send(title, message, priority string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentNotification{title, message, priority})
}

func (f *fakeNotifier) all() []sentNotification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentNotification(nil), f.sent...)
}

func (f *fakeNotifier) count() int { return len(f.all()) }

type pauseCall struct {
	agent, reason string
	cause         Causation
}

// fakePauser records pause calls and, crucially, re-emits the agent_paused
// transition the real audited pause API would emit — which is what lets the
// cascade test observe whether the depth-1 guard actually holds.
type fakePauser struct {
	mu    sync.Mutex
	calls []pauseCall
	// dispatcher, when set, receives the agent_paused transition that the
	// pause causes, exactly as the real pause path would.
	dispatcher *Dispatcher
	err        error
}

func (f *fakePauser) PauseAgent(ctx context.Context, agent, reason string, cause Causation) error {
	f.mu.Lock()
	f.calls = append(f.calls, pauseCall{agent, reason, cause})
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.dispatcher != nil {
		f.dispatcher.Fire(ctx, Payload{
			Transition: TransitionAgentPaused,
			Agent:      agent,
			Reason:     reason,
			Causation:  cause,
		})
	}
	return nil
}

func (f *fakePauser) all() []pauseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pauseCall(nil), f.calls...)
}

type annotation struct {
	agent, issueRef, note string
	attrs                 map[string]string
}

type fakeAnnotator struct {
	mu    sync.Mutex
	notes []annotation
}

func (f *fakeAnnotator) Annotate(ctx context.Context, agent, issueRef, note string, attrs map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes = append(f.notes, annotation{agent, issueRef, note, attrs})
	return nil
}

func (f *fakeAnnotator) all() []annotation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]annotation(nil), f.notes...)
}

type fakeApprovals struct {
	mu   sync.Mutex
	reqs []ApprovalRequest
}

func (f *fakeApprovals) EnqueueApproval(ctx context.Context, r ApprovalRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return nil
}

func (f *fakeApprovals) all() []ApprovalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ApprovalRequest(nil), f.reqs...)
}

type auditEntry struct {
	actor, action, agent string
	fields               map[string]any
}

type fakeAudit struct {
	mu      sync.Mutex
	entries []auditEntry
}

func (f *fakeAudit) Record(actor, action, agentName string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, auditEntry{actor, action, agentName, fields})
}

func (f *fakeAudit) all() []auditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auditEntry(nil), f.entries...)
}

func (f *fakeAudit) withAction(action string) []auditEntry {
	var out []auditEntry
	for _, e := range f.all() {
		if e.action == action {
			out = append(out, e)
		}
	}
	return out
}

// panickingNotifier models a sink that blows up, to prove failure isolation
// covers panics and not just returned errors.
type panickingNotifier struct{}

func (panickingNotifier) Send(title, message, priority string) {
	panic("sink exploded")
}

// mustRegistry compiles hooks or fails the test.
func mustRegistry(t *testing.T, hooks ...Hook) *Registry {
	t.Helper()
	reg, err := Compile(hooks)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return reg
}

// ---------------------------------------------------------------------------
// GUARANTEE 2 — depth-1 causation guard (loop prevention)
// ---------------------------------------------------------------------------

// TestDepth1GuardBlocksHookTriggeredCascade is the loop-prevention regression
// test. The setup is the exact cycle the RFC calls out: a hook on
// agent_paused whose action is pause. Without the guard this recurses forever.
//
// It asserts the INVARIANT (exactly one pause, from the world-caused
// transition) rather than merely "it terminated", and includes a positive
// control below proving the hook does fire when the transition is
// world-caused — so the test cannot pass just because nothing ever ran.
func TestDepth1GuardBlocksHookTriggeredCascade(t *testing.T) {
	audit := &fakeAudit{}
	pauser := &fakePauser{}

	// A self-feeding hook: pausing an agent fires the pause hook again.
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "pause-on-pause", On: TransitionAgentPaused, Action: ActionPause,
		}),
		quietLogger(),
		WithPauser(pauser),
		WithAuditSink(audit),
	)
	// Close the loop: the pause API re-emits agent_paused, as the real one does.
	pauser.dispatcher = d

	// A world-caused pause (depth 0) — this one MAY fire hooks.
	d.Fire(context.Background(), Payload{
		Transition: TransitionAgentPaused,
		Agent:      "reviewer",
		Trigger:    "operator",
	})
	d.Wait()

	calls := pauser.all()
	if len(calls) != 1 {
		t.Fatalf("depth-1 guard failed: expected exactly 1 pause, got %d — the cascade was not stopped", len(calls))
	}
	// The pause the hook performed must be marked hook-caused, which is what
	// stops the next round.
	if got := calls[0].cause; got.Depth != 1 || got.HookName != "pause-on-pause" {
		t.Errorf("hook-caused transition must carry depth-1 causation, got %+v", got)
	}
	if calls[0].cause.OriginTransition != TransitionAgentPaused {
		t.Errorf("causation should name the origin transition, got %q", calls[0].cause.OriginTransition)
	}

	// The suppressed second round must be visible in the audit log, not silent.
	if len(audit.withAction(AuditHookSuppressed)) != 1 {
		t.Errorf("expected exactly 1 depth-suppression audit entry, got %d",
			len(audit.withAction(AuditHookSuppressed)))
	}
}

// TestDepth1GuardPositiveControl is the companion to the cascade test: it
// proves the same hook DOES fire on a world-caused transition. Without this,
// the cascade test above would pass even if Fire were broken and never
// dispatched anything.
func TestDepth1GuardPositiveControl(t *testing.T) {
	pauser := &fakePauser{} // no dispatcher: does not re-emit
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "p", On: TransitionAgentPaused, Action: ActionPause}),
		quietLogger(),
		WithPauser(pauser),
	)

	d.Fire(context.Background(), Payload{
		Transition: TransitionAgentPaused, Agent: "reviewer",
	})
	d.Wait()

	if len(pauser.all()) != 1 {
		t.Fatalf("positive control: a world-caused transition MUST fire its hook, got %d calls",
			len(pauser.all()))
	}
}

// TestFireRefusesHookCausedTransitionDirectly checks the guard at its own
// level, independent of any action.
func TestFireRefusesHookCausedTransitionDirectly(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "n", On: TransitionAgentPaused, Action: ActionNotify}),
		quietLogger(),
		WithNotifier(notifier),
	)

	d.Fire(context.Background(), Payload{
		Transition: TransitionAgentPaused,
		Agent:      "reviewer",
		Causation:  Causation{Depth: 1, HookName: "upstream", OriginTransition: TransitionEscalationRed},
	})
	d.Wait()

	if notifier.count() != 0 {
		t.Errorf("a hook-caused transition must not fire hooks, got %d notifications", notifier.count())
	}
}

func TestCausationChildPreservesRootOrigin(t *testing.T) {
	root := Causation{}
	first := root.Child("hook-a", TransitionEscalationRed)
	if first.Depth != 1 || first.OriginTransition != TransitionEscalationRed {
		t.Fatalf("first child wrong: %+v", first)
	}
	// A hypothetical second level must keep the ROOT origin, not the
	// intermediate one, so the audit trail names the real cause.
	second := first.Child("hook-b", TransitionAgentPaused)
	if second.OriginTransition != TransitionEscalationRed {
		t.Errorf("root origin must be preserved, got %q", second.OriginTransition)
	}
	if second.MayFireHooks() {
		t.Error("depth-2 causation must not be allowed to fire hooks")
	}
}

func TestCausationPredicates(t *testing.T) {
	if (Causation{}).IsHookCaused() {
		t.Error("depth-0 is world-caused")
	}
	if (Causation{Depth: 1}).MayFireHooks() {
		t.Error("depth-1 must not fire hooks")
	}
	if !(Causation{}).MayFireHooks() {
		t.Error("depth-0 must fire hooks")
	}
}

// ---------------------------------------------------------------------------
// GUARANTEE 4 — failure isolation
// ---------------------------------------------------------------------------

// TestOneFailingHookDoesNotAffectOthers: a hook whose sink is unwired (and so
// errors) must not prevent the other hooks on the same transition from running.
func TestOneFailingHookDoesNotAffectOthers(t *testing.T) {
	notifier := &fakeNotifier{}
	annotator := &fakeAnnotator{}
	audit := &fakeAudit{}

	// The middle hook's sink (approvals) is deliberately NOT wired, so it
	// fails; the notify and annotate hooks must still run.
	d := NewDispatcher(
		mustRegistry(t,
			Hook{Name: "notify-hook", On: TransitionReviewRejected, Action: ActionNotify},
			Hook{Name: "broken-hook", On: TransitionReviewRejected, Action: ActionEnqueueApproval},
			Hook{Name: "annotate-hook", On: TransitionReviewRejected, Action: ActionAnnotate},
		),
		quietLogger(),
		WithNotifier(notifier),
		WithAnnotator(annotator),
		WithAuditSink(audit),
	)

	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "reviewer",
	})
	d.Wait()

	if notifier.count() != 1 {
		t.Errorf("notify hook should have run despite a sibling failing, got %d", notifier.count())
	}
	if len(annotator.all()) != 1 {
		t.Errorf("annotate hook should have run despite a sibling failing, got %d", len(annotator.all()))
	}

	failures := audit.withAction(AuditHookFailed)
	if len(failures) != 1 {
		t.Fatalf("expected exactly 1 failure audit entry, got %d", len(failures))
	}
	if failures[0].fields["hook"] != "broken-hook" {
		t.Errorf("wrong hook recorded as failed: %v", failures[0].fields["hook"])
	}
	if len(audit.withAction(AuditHookFired)) != 2 {
		t.Errorf("expected 2 successful firings audited, got %d", len(audit.withAction(AuditHookFired)))
	}
}

// TestPanickingHookIsIsolated proves the recovered boundary covers a sink that
// panics, not just one that returns an error.
func TestPanickingHookIsIsolated(t *testing.T) {
	annotator := &fakeAnnotator{}
	audit := &fakeAudit{}

	d := NewDispatcher(
		mustRegistry(t,
			Hook{Name: "panics", On: TransitionReviewRejected, Action: ActionNotify},
			Hook{Name: "fine", On: TransitionReviewRejected, Action: ActionAnnotate},
		),
		quietLogger(),
		WithNotifier(panickingNotifier{}),
		WithAnnotator(annotator),
		WithAuditSink(audit),
	)

	// Must not crash the test process.
	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()

	if len(annotator.all()) != 1 {
		t.Errorf("sibling hook should run despite a panicking hook, got %d", len(annotator.all()))
	}
	failures := audit.withAction(AuditHookFailed)
	if len(failures) != 1 || !strings.Contains(failures[0].fields["error"].(string), "panic") {
		t.Errorf("panic should be audited as a failure, got %+v", failures)
	}
}

// TestFailingHookDoesNotAffectTheTransition: Fire must return normally even
// when every hook fails, because the transition has already durably committed
// and a hook must never be able to undo or block it.
func TestFailingHookDoesNotAffectTheTransition(t *testing.T) {
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "broken", On: TransitionUpgradePause, Action: ActionNotify}),
		quietLogger(),
		// No notifier wired → the hook errors.
	)

	done := make(chan struct{})
	go func() {
		d.Fire(context.Background(), Payload{Transition: TransitionUpgradePause, To: "on"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Fire must return promptly regardless of hook outcomes")
	}
	d.Wait()
}

// ---------------------------------------------------------------------------
// GUARANTEE 1 — post-commit, off the critical path
// ---------------------------------------------------------------------------

// TestFireDoesNotBlockTheCallersCriticalPath: a slow hook must not extend the
// emitting site's latency. Fire hands off to a goroutine and returns.
func TestFireDoesNotBlockTheCallersCriticalPath(t *testing.T) {
	release := make(chan struct{})
	slow := &blockingAnnotator{release: release}

	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "slow", On: TransitionSweepCompleted, Action: ActionAnnotate}),
		quietLogger(),
		WithAnnotator(slow),
	)

	start := time.Now()
	d.Fire(context.Background(), Payload{Transition: TransitionSweepCompleted, Repo: "o/r"})
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("Fire blocked for %v; hooks must run off the critical path", elapsed)
	}

	close(release)
	d.Wait()
	if slow.count() != 1 {
		t.Errorf("the hook should still have run, got %d", slow.count())
	}
}

type blockingAnnotator struct {
	release chan struct{}
	mu      sync.Mutex
	n       int
}

func (b *blockingAnnotator) Annotate(ctx context.Context, agent, issueRef, note string, attrs map[string]string) error {
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	return nil
}

func (b *blockingAnnotator) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// TestNoHooksIsACheapNoOp: the overwhelmingly common case (no hooks
// configured) must cost nothing on the post-commit path.
func TestNoHooksIsACheapNoOp(t *testing.T) {
	audit := &fakeAudit{}
	d := NewDispatcher(mustRegistry(t), quietLogger(), WithAuditSink(audit))

	d.Fire(context.Background(), Payload{Transition: TransitionGovernorModeChange, From: "a", To: "b"})
	d.Wait()

	if len(audit.all()) != 0 {
		t.Errorf("an empty registry should produce no audit noise, got %d entries", len(audit.all()))
	}
}

func TestNilDispatcherAndNilRegistryAreSafe(t *testing.T) {
	var d *Dispatcher
	d.Fire(context.Background(), Payload{Transition: TransitionAgentPaused})
	d.Wait()
	if d.Len() != 0 {
		t.Error("nil dispatcher should report zero hooks")
	}

	d2 := NewDispatcher(nil, quietLogger())
	d2.Fire(context.Background(), Payload{Transition: TransitionAgentPaused})
	d2.Wait()
}

// TestFireOnlyDispatchesMatchingTransition guards the index.
func TestFireOnlyDispatchesMatchingTransition(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "n", On: TransitionReviewRejected, Action: ActionNotify}),
		quietLogger(),
		WithNotifier(notifier),
	)

	d.Fire(context.Background(), Payload{Transition: TransitionSweepCompleted, Repo: "o/r"})
	d.Wait()
	if notifier.count() != 0 {
		t.Errorf("a hook must not fire on a different transition, got %d", notifier.count())
	}

	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("the hook should fire on its own transition, got %d", notifier.count())
	}
}

// ---------------------------------------------------------------------------
// GUARANTEE 3 — rate limiting
// ---------------------------------------------------------------------------

// TestRateLimitEnforcedOnDispatch drives the limit through Fire, with a frozen
// clock so the test neither sleeps nor flakes.
func TestRateLimitEnforcedOnDispatch(t *testing.T) {
	notifier := &fakeNotifier{}
	audit := &fakeAudit{}

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "flappy", On: TransitionGovernorModeChange, Action: ActionNotify,
			RateLimitPerMinute: 3,
		}),
		quietLogger(),
		WithNotifier(notifier),
		WithAuditSink(audit),
		WithClock(clock),
	)

	// Ten flaps in the same instant; only 3 may get through.
	for i := 0; i < 10; i++ {
		d.Fire(context.Background(), Payload{
			Transition: TransitionGovernorModeChange, From: "normal", To: "conserve",
		})
	}
	d.Wait()

	if notifier.count() != 3 {
		t.Errorf("rate limit not enforced: expected 3 notifications, got %d", notifier.count())
	}
	if got := len(audit.withAction(AuditHookRateLimited)); got != 7 {
		t.Errorf("expected 7 rate-limit audit entries, got %d", got)
	}

	// After the window slides, capacity returns.
	now = now.Add(2 * time.Minute)
	d.Fire(context.Background(), Payload{Transition: TransitionGovernorModeChange, To: "normal"})
	d.Wait()
	if notifier.count() != 4 {
		t.Errorf("capacity should recover after the window, got %d total", notifier.count())
	}
}

// TestRateLimitSurvivesRegistryReload: reloading config must not reset the
// storm ceiling, or a reload loop becomes a way to bypass it.
func TestRateLimitSurvivesRegistryReload(t *testing.T) {
	notifier := &fakeNotifier{}
	now := time.Now()

	hook := Hook{
		Name: "h", On: TransitionGovernorModeChange, Action: ActionNotify,
		RateLimitPerMinute: 2,
	}
	d := NewDispatcher(mustRegistry(t, hook), quietLogger(),
		WithNotifier(notifier), WithClock(func() time.Time { return now }))

	for i := 0; i < 5; i++ {
		d.Fire(context.Background(), Payload{Transition: TransitionGovernorModeChange, To: "x"})
	}
	d.Wait()
	if notifier.count() != 2 {
		t.Fatalf("setup: expected 2, got %d", notifier.count())
	}

	// Hot reload with the same hook.
	d.SetRegistry(mustRegistry(t, hook))
	d.Fire(context.Background(), Payload{Transition: TransitionGovernorModeChange, To: "y"})
	d.Wait()

	if notifier.count() != 2 {
		t.Errorf("a config reload must not clear the rate-limit window, got %d", notifier.count())
	}
}

func TestSetRegistrySwapsHooks(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(mustRegistry(t), quietLogger(), WithNotifier(notifier))
	if d.Len() != 0 {
		t.Fatalf("expected empty, got %d", d.Len())
	}

	d.SetRegistry(mustRegistry(t, Hook{
		Name: "new", On: TransitionReviewRejected, Action: ActionNotify,
	}))
	if d.Len() != 1 {
		t.Errorf("expected 1 hook after reload, got %d", d.Len())
	}

	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("the newly loaded hook should fire, got %d", notifier.count())
	}
}

// ---------------------------------------------------------------------------
// GUARANTEE 5 — every firing is audited
// ---------------------------------------------------------------------------

func TestEveryFiringIsAudited(t *testing.T) {
	audit := &fakeAudit{}
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "audited", On: TransitionACMMLevelChange, Action: ActionNotify}),
		quietLogger(),
		WithNotifier(notifier),
		WithAuditSink(audit),
	)

	d.Fire(context.Background(), Payload{
		Transition: TransitionACMMLevelChange, From: "3", To: "4", Actor: "operator",
	})
	d.Wait()

	entries := audit.withAction(AuditHookFired)
	if len(entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(entries))
	}
	e := entries[0]
	if e.actor != auditActorSystem {
		t.Errorf("hook firings are system-attributed, got actor %q", e.actor)
	}
	if e.fields["hook"] != "audited" {
		t.Errorf("audit must name the responsible config rule, got %v", e.fields["hook"])
	}
	if e.fields["transition"] != string(TransitionACMMLevelChange) {
		t.Errorf("audit must name the transition, got %v", e.fields["transition"])
	}
	if e.fields["action"] != string(ActionNotify) {
		t.Errorf("audit must name the action, got %v", e.fields["action"])
	}
	if e.fields["outcome"] != "success" {
		t.Errorf("expected success outcome, got %v", e.fields["outcome"])
	}
}

func TestDispatcherWithoutAuditSinkDoesNotPanic(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "h", On: TransitionReviewRejected, Action: ActionNotify}),
		quietLogger(), WithNotifier(notifier))
	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()
	if notifier.count() != 1 {
		t.Error("an unwired audit sink must not prevent the action")
	}
}

// ---------------------------------------------------------------------------
// Predicates
// ---------------------------------------------------------------------------

func TestWhenPredicateGatesFiring(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "only-reviewer", On: TransitionAgentPaused, Action: ActionNotify,
			When: `t.agent == "reviewer"`,
		}),
		quietLogger(), WithNotifier(notifier))

	d.Fire(context.Background(), Payload{Transition: TransitionAgentPaused, Agent: "scanner"})
	d.Wait()
	if notifier.count() != 0 {
		t.Errorf("non-matching predicate should not fire, got %d", notifier.count())
	}

	d.Fire(context.Background(), Payload{Transition: TransitionAgentPaused, Agent: "reviewer"})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("matching predicate should fire, got %d", notifier.count())
	}
}

func TestWhenPredicateReadsAttrsAndModelFields(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "stale-pin", On: TransitionReviewRejected, Action: ActionNotify,
			When: `t.pin != "" && attr(t.attrs, "pr") != ""`,
		}),
		quietLogger(), WithNotifier(notifier))

	// Missing pin → no fire.
	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "r",
		Attrs: map[string]string{AttrPR: "42"},
	})
	d.Wait()
	if notifier.count() != 0 {
		t.Errorf("expected no fire without a pin, got %d", notifier.count())
	}

	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "r", Pin: "20240229",
		Attrs: map[string]string{AttrPR: "42"},
	})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("expected a fire with pin and pr, got %d", notifier.count())
	}
}

// TestAttrHelperIsTotalOverMissingKeys guards the operator-usability trap that
// motivated attr(): CEL's native map index RAISES on an absent key, and under
// the fail-closed rule a raised error is a no-match — so a predicate that looks
// obviously correct would silently never fire. attr() must be total.
func TestAttrHelperIsTotalOverMissingKeys(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "h", On: TransitionUpgradePause, Action: ActionNotify,
			When: `attr(t.attrs, "missing") == ""`,
		}),
		quietLogger(), WithNotifier(notifier))

	// A payload with a nil Attrs map entirely.
	d.Fire(context.Background(), Payload{Transition: TransitionUpgradePause, To: "on"})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("attr() on a nil Attrs map should yield \"\", got %d fires", notifier.count())
	}

	// A populated map that simply lacks the key.
	d.Fire(context.Background(), Payload{
		Transition: TransitionUpgradePause, To: "off",
		Attrs: map[string]string{"other": "v"},
	})
	d.Wait()
	if notifier.count() != 2 {
		t.Errorf("attr() on an absent key should yield \"\", got %d fires", notifier.count())
	}
}

// TestAttrHelperReadsPresentKeys is the positive control for attr(): it must
// actually return values, not just always "".
func TestAttrHelperReadsPresentKeys(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "h", On: TransitionReviewRejected, Action: ActionNotify,
			When: `attr(t.attrs, "pr") == "4001"`,
		}),
		quietLogger(), WithNotifier(notifier))

	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "a",
		Attrs: map[string]string{AttrPR: "999"},
	})
	d.Wait()
	if notifier.count() != 0 {
		t.Errorf("non-matching attr value should not fire, got %d", notifier.count())
	}

	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "a",
		Attrs: map[string]string{AttrPR: "4001"},
	})
	d.Wait()
	if notifier.count() != 1 {
		t.Errorf("matching attr value should fire, got %d", notifier.count())
	}
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

func TestPauseActionGoesThroughAuditedAPIWithCausation(t *testing.T) {
	pauser := &fakePauser{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "pause-on-red", On: TransitionEscalationRed, Action: ActionPause,
			Params: map[string]string{"agent": "fixer", "reason": "red CI"},
		}),
		quietLogger(), WithPauser(pauser))

	d.Fire(context.Background(), Payload{
		Transition: TransitionEscalationRed, Repo: "o/r",
	})
	d.Wait()

	calls := pauser.all()
	if len(calls) != 1 {
		t.Fatalf("expected 1 pause, got %d", len(calls))
	}
	if calls[0].agent != "fixer" || calls[0].reason != "red CI" {
		t.Errorf("params not honored: %+v", calls[0])
	}
	if calls[0].cause.Depth != 1 {
		t.Errorf("pause must carry depth-1 causation, got %+v", calls[0].cause)
	}
}

func TestPauseActionFallsBackToPayloadAgent(t *testing.T) {
	pauser := &fakePauser{}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "p", On: TransitionAgentResumed, Action: ActionPause}),
		quietLogger(), WithPauser(pauser))

	d.Fire(context.Background(), Payload{Transition: TransitionAgentResumed, Agent: "from-payload"})
	d.Wait()

	if calls := pauser.all(); len(calls) != 1 || calls[0].agent != "from-payload" {
		t.Errorf("expected fallback to the payload agent, got %+v", calls)
	}
}

func TestAnnotateActionRecordsTimelineEntryWithModelContext(t *testing.T) {
	annotator := &fakeAnnotator{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "note-it", On: TransitionReviewRejected, Action: ActionAnnotate,
			Params: map[string]string{"note": "quality flag"},
		}),
		quietLogger(), WithAnnotator(annotator))

	d.Fire(context.Background(), Payload{
		Transition: TransitionReviewRejected, Agent: "reviewer",
		Model: "claude-opus-4", Backend: "anthropic", Pin: "20240229",
	})
	d.Wait()

	notes := annotator.all()
	if len(notes) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(notes))
	}
	if notes[0].note != "quality flag" {
		t.Errorf("note param not honored: %q", notes[0].note)
	}
	// The timeline reader should see the same model context a notification
	// would have shown.
	for k, want := range map[string]string{
		"hook": "note-it", "transition": string(TransitionReviewRejected),
		"model": "claude-opus-4", "backend": "anthropic", "pin": "20240229",
	} {
		if notes[0].attrs[k] != want {
			t.Errorf("attr %q: got %q, want %q", k, notes[0].attrs[k], want)
		}
	}
}

// TestEnqueueApprovalConsumesTheQueueAPI proves the action delegates to the
// #4000 queue interface rather than implementing storage of its own.
func TestEnqueueApprovalConsumesTheQueueAPI(t *testing.T) {
	queue := &fakeApprovals{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "needs-approval", On: TransitionACMMLevelChange, Action: ActionEnqueueApproval,
			Params: map[string]string{"kind": "acmm-raise", "summary": "Confirm level raise"},
		}),
		quietLogger(), WithApprovalQueue(queue))

	d.Fire(context.Background(), Payload{
		Transition: TransitionACMMLevelChange, From: "3", To: "5", Actor: "operator",
	})
	d.Wait()

	reqs := queue.all()
	if len(reqs) != 1 {
		t.Fatalf("expected 1 approval enqueued, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Kind != "acmm-raise" || r.Summary != "Confirm level raise" {
		t.Errorf("params not honored: %+v", r)
	}
	if r.Transition != TransitionACMMLevelChange || r.HookName != "needs-approval" {
		t.Errorf("provenance missing: %+v", r)
	}
	if r.Cause.Depth != 1 {
		t.Errorf("approval must carry depth-1 causation, got %+v", r.Cause)
	}
}

// TestUnwiredApprovalQueueFailsLoudly: until #4000 lands, an enqueue-approval
// hook must report an error per firing rather than silently doing nothing —
// an approval that never queues is worse than a loud failure.
func TestUnwiredApprovalQueueFailsLoudly(t *testing.T) {
	audit := &fakeAudit{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "unwired", On: TransitionReviewRejected, Action: ActionEnqueueApproval,
		}),
		quietLogger(), WithAuditSink(audit))

	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()

	failures := audit.withAction(AuditHookFailed)
	if len(failures) != 1 {
		t.Fatalf("expected a loud failure, got %d entries", len(failures))
	}
	if msg, _ := failures[0].fields["error"].(string); !strings.Contains(msg, "#4000") {
		t.Errorf("the error should point at the unlanded queue, got %q", msg)
	}
}

func TestUnwiredSinksReportErrorsNotPanics(t *testing.T) {
	for _, tc := range []struct {
		action Action
		on     Transition
		params map[string]string
	}{
		{ActionNotify, TransitionReviewRejected, nil},
		{ActionPause, TransitionAgentPaused, nil},
		{ActionAnnotate, TransitionReviewRejected, nil},
		{ActionEnqueueApproval, TransitionReviewRejected, nil},
	} {
		audit := &fakeAudit{}
		d := NewDispatcher(
			mustRegistry(t, Hook{
				Name: "h", On: tc.on, Action: tc.action, Params: tc.params,
			}),
			quietLogger(), WithAuditSink(audit))

		d.Fire(context.Background(), Payload{Transition: tc.on, Agent: "a"})
		d.Wait()

		if got := len(audit.withAction(AuditHookFailed)); got != 1 {
			t.Errorf("action %q: expected 1 audited failure, got %d", tc.action, got)
		}
	}
}

func TestPauseActionPropagatesSinkError(t *testing.T) {
	audit := &fakeAudit{}
	pauser := &fakePauser{err: errors.New("pause API rejected")}
	d := NewDispatcher(
		mustRegistry(t, Hook{Name: "p", On: TransitionAgentPaused, Action: ActionPause}),
		quietLogger(), WithPauser(pauser), WithAuditSink(audit))

	d.Fire(context.Background(), Payload{Transition: TransitionAgentPaused, Agent: "a"})
	d.Wait()

	failures := audit.withAction(AuditHookFailed)
	if len(failures) != 1 {
		t.Fatalf("expected the sink error to be audited, got %d", len(failures))
	}
	if msg, _ := failures[0].fields["error"].(string); !strings.Contains(msg, "pause API rejected") {
		t.Errorf("error text lost: %q", msg)
	}
}

func TestNotifyPriorityMapping(t *testing.T) {
	for in, want := range map[string]string{
		"high": "high", "HIGH": "high", " low ": "low",
		"default": "default", "": "default", "bogus": "default",
	} {
		if got := notifyPriority(in); got != want {
			t.Errorf("notifyPriority(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotifyHonorsCustomTitleAndMessage(t *testing.T) {
	notifier := &fakeNotifier{}
	d := NewDispatcher(
		mustRegistry(t, Hook{
			Name: "custom", On: TransitionSweepCompleted, Action: ActionNotify,
			Params: map[string]string{
				"title": "Sweep done", "message": "custom body", "priority": "low",
			},
		}),
		quietLogger(), WithNotifier(notifier))

	d.Fire(context.Background(), Payload{Transition: TransitionSweepCompleted, Repo: "o/r"})
	d.Wait()

	sent := notifier.all()
	if len(sent) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(sent))
	}
	if sent[0].title != "Sweep done" || sent[0].message != "custom body" || sent[0].priority != "low" {
		t.Errorf("custom params not honored: %+v", sent[0])
	}
}

func TestExecuteRejectsUnvettedActionAsBackstop(t *testing.T) {
	// A Hook constructed in code, bypassing registry validation, must still be
	// refused at execution — fail-closed at both layers.
	d := NewDispatcher(nil, quietLogger(), WithNotifier(&fakeNotifier{}))
	err := d.execute(context.Background(), Hook{Name: "x", Action: Action("exec")}, Payload{})
	if err == nil || !strings.Contains(err.Error(), "unknown action") {
		t.Errorf("expected an unvetted action to be refused at execute, got %v", err)
	}
}

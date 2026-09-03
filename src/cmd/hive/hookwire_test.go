package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/timeline"
)

func hookTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// resetHookDispatcher clears the process-wide cache so tests do not leak state
// into each other.
func resetHookDispatcher(t *testing.T) {
	t.Helper()
	globalHookDispatcher.mu.Lock()
	defer globalHookDispatcher.mu.Unlock()
	globalHookDispatcher.sig = ""
	globalHookDispatcher.dispatcher = nil
	globalHookDispatcher.built = false
}

func TestBuildHookDispatcherArmsConfiguredHooks(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	cfg := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify",
	}}}
	buildHookDispatcher(cfg, hookSinks{}, hookTestLogger())

	d := hookDispatcher()
	if d == nil {
		t.Fatal("a configured hook should produce a dispatcher")
	}
	if d.Len() != 1 {
		t.Errorf("expected 1 hook armed, got %d", d.Len())
	}
}

func TestBuildHookDispatcherNoHooksIsNil(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	buildHookDispatcher(&config.Config{}, hookSinks{}, hookTestLogger())
	if hookDispatcher() != nil {
		t.Error("no configured hooks should leave the dispatcher nil (total no-op)")
	}
	// A nil dispatcher must still be safe to fire through.
	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionGovernorModeChange,
	})
}

// TestBuildHookDispatcherKeepsPreviousSetOnInvalidConfig is the fail-closed
// reload guarantee: a bad edit must not disarm hooks that were working.
func TestBuildHookDispatcherKeepsPreviousSetOnInvalidConfig(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	good := &config.Config{Hooks: []config.HookRule{{
		Name: "good", On: "review_rejected", Action: "notify",
	}}}
	buildHookDispatcher(good, hookSinks{}, hookTestLogger())
	if hookDispatcher().Len() != 1 {
		t.Fatal("setup: expected the good hook armed")
	}

	// Operator introduces a typo.
	bad := &config.Config{Hooks: []config.HookRule{{
		Name: "bad", On: "review_rejcted", Action: "notify",
	}}}
	buildHookDispatcher(bad, hookSinks{}, hookTestLogger())

	d := hookDispatcher()
	if d == nil || d.Len() != 1 {
		t.Error("an invalid reload must keep the previous hook set armed, not disarm it")
	}
}

// TestBuildHookDispatcherReloadPreservesRateLimitWindow: swapping the registry
// in place (rather than rebuilding) is what stops a reload loop from clearing
// the anti-storm ceiling.
func TestBuildHookDispatcherReloadPreservesRateLimitWindow(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	cfg := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify", RateLimitPerMinute: 1,
	}}}
	buildHookDispatcher(cfg, hookSinks{}, hookTestLogger())
	first := hookDispatcher()

	// A changed hook list forces a rebuild path.
	cfg2 := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify", RateLimitPerMinute: 2,
	}}}
	buildHookDispatcher(cfg2, hookSinks{}, hookTestLogger())
	second := hookDispatcher()

	if first != second {
		t.Error("reload must swap the registry inside the SAME dispatcher, " +
			"or rate-limit windows reset and the storm ceiling can be bypassed")
	}
}

// TestDisarmThenRearmPreservesRateLimitWindow is the bypass regression.
//
// Reload preserves the limiter by swapping the registry in place — but if
// removing every hook DROPPED the dispatcher, re-adding them would build a
// fresh one with a fresh limiter. Two ordinary config edits (remove all hooks,
// re-add them) would then clear the storm ceiling, defeating the exact
// protection the in-place swap exists to provide.
func TestDisarmThenRearmPreservesRateLimitWindow(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	armed := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify",
	}}}
	buildHookDispatcher(armed, hookSinks{}, hookTestLogger())
	first := hookDispatcher()
	if first == nil {
		t.Fatal("setup: expected an armed dispatcher")
	}

	// Operator removes every hook…
	buildHookDispatcher(&config.Config{}, hookSinks{}, hookTestLogger())
	disarmed := hookDispatcher()
	if disarmed == nil {
		t.Fatal("disarming must keep the dispatcher (with an empty registry), " +
			"or the rate-limit windows are discarded with it")
	}
	if disarmed.Len() != 0 {
		t.Errorf("a disarmed dispatcher should hold no hooks, got %d", disarmed.Len())
	}

	// …then puts them back.
	buildHookDispatcher(armed, hookSinks{}, hookTestLogger())
	rearmed := hookDispatcher()

	if rearmed != first {
		t.Error("disarm→re-arm must reuse the SAME dispatcher; rebuilding it resets " +
			"the rate-limit windows and gives operators a two-edit ceiling bypass")
	}
	if rearmed.Len() != 1 {
		t.Errorf("expected the hook re-armed, got %d", rearmed.Len())
	}
}

func TestBuildHookDispatcherSkipsRecompileWhenUnchanged(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	cfg := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify",
	}}}
	buildHookDispatcher(cfg, hookSinks{}, hookTestLogger())
	before := hookDispatcher()

	// Same list again: must be a signature hit, not a rebuild.
	buildHookDispatcher(cfg, hookSinks{}, hookTestLogger())
	if hookDispatcher() != before {
		t.Error("an unchanged hook list must not rebuild the dispatcher")
	}
}

// ---------------------------------------------------------------------------
// Adapters
// ---------------------------------------------------------------------------

func TestAnnotatorAdapterRecordsOnTheExistingTimeline(t *testing.T) {
	store := timeline.NewStore()
	a := &annotatorAdapter{store: store}

	err := a.Annotate(context.Background(), "reviewer", "o/r#1", "quality flag",
		map[string]string{"hook": "h", "model": "claude-opus-4"})
	if err != nil {
		t.Fatalf("annotate: %v", err)
	}

	events := store.Recent(10)
	if len(events) != 1 {
		t.Fatalf("expected 1 timeline event, got %d", len(events))
	}
	e := events[0]
	if e.Agent != "reviewer" || e.IssueRef != "o/r#1" {
		t.Errorf("event fields lost: %+v", e)
	}
	if e.Attrs["note"] != "quality flag" {
		t.Errorf("note should be carried in attrs, got %q", e.Attrs["note"])
	}
	if e.Attrs["model"] != "claude-opus-4" {
		t.Errorf("model context lost: %+v", e.Attrs)
	}
}

func TestAdaptersReportErrorsWhenBackingObjectMissing(t *testing.T) {
	if err := (&annotatorAdapter{}).Annotate(context.Background(), "a", "", "n", nil); err == nil {
		t.Error("an unwired timeline must report an error, not silently succeed")
	}
	if err := (&pauserAdapter{}).PauseAgent(context.Background(), "a", "r", hooks.Causation{}); err == nil {
		t.Error("an unwired agent manager must report an error, not silently succeed")
	}
	// The notifier adapter is fire-and-forget; it must simply not panic.
	(&notifierAdapter{}).Send("t", "m", "high")
}

// TestHookPauseTriggerNamesTheResponsibleHook: a hook-driven pause must be
// distinguishable from an operator's or the governor's in the durable
// paused_trigger record, and must name WHICH hook did it.
func TestHookPauseTriggerNamesTheResponsibleHook(t *testing.T) {
	got := hookPauseTriggerFor(hooks.Causation{Depth: 1, HookName: "pause-on-red"})
	if !strings.HasPrefix(got, hookPauseTrigger) {
		t.Errorf("trigger should be identifiable as hook-driven, got %q", got)
	}
	if !strings.Contains(got, "pause-on-red") {
		t.Errorf("trigger should name the responsible hook, got %q", got)
	}

	// With no hook name it still identifies as hook-driven rather than
	// masquerading as an operator action.
	if got := hookPauseTriggerFor(hooks.Causation{}); got != hookPauseTrigger {
		t.Errorf("expected %q, got %q", hookPauseTrigger, got)
	}
}

// TestHookPauseActorIsNonHumanAndAttributed adopts #4055's PausedBy: a
// hook-driven pause must not land anonymous (the "paused, actor unknown" state
// #4041 found indistinguishable from a malfunction), but it also must not
// fabricate a human actor, which is what PauseBy's contract forbids.
func TestHookPauseActorIsNonHumanAndAttributed(t *testing.T) {
	got := hookPauseActor(hooks.Causation{Depth: 1, HookName: "pause-on-red"})

	if got == "" {
		t.Error("a hook-driven pause must record an actor, not land anonymous")
	}
	if !strings.HasPrefix(got, hookPauseTrigger+":") {
		t.Errorf("the actor must be a machine identity a reader cannot mistake "+
			"for a dashboard user, got %q", got)
	}
	if !strings.Contains(got, "pause-on-red") {
		t.Errorf("the actor should name the responsible hook, got %q", got)
	}

	// Unnamed hook: still non-anonymous, still clearly not a person.
	if got := hookPauseActor(hooks.Causation{}); got != hookPauseTrigger {
		t.Errorf("expected %q, got %q", hookPauseTrigger, got)
	}
}

type hookWireAudit struct {
	mu      sync.Mutex
	actions []string
	ch      chan string
}

func (a *hookWireAudit) Record(actor, action, agentName string, fields map[string]any) {
	a.mu.Lock()
	a.actions = append(a.actions, action)
	a.mu.Unlock()
	select {
	case a.ch <- action:
	default:
	}
}

func (a *hookWireAudit) count(action string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int
	for _, got := range a.actions {
		if got == action {
			n++
		}
	}
	return n
}

func TestAgentPausedEmitterCarriesHookCausationAndStopsPauseLoop(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	mgr := agent.NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, hookTestLogger(), agent.ProjectContext{})
	audit := &hookWireAudit{ch: make(chan string, 8)}
	cfg := &config.Config{Hooks: []config.HookRule{{
		Name: "pause-again", On: "agent_paused", Action: "pause",
		Params: map[string]string{"agent": "scanner"},
	}}}
	buildHookDispatcher(cfg, hookSinks{AgentMgr: mgr, Audit: audit}, hookTestLogger())
	installAgentPauseEmitter(mgr)

	hookDispatcher().Fire(context.Background(), hooks.Payload{
		Transition: hooks.TransitionAgentPaused,
		Agent:      "scanner",
		Trigger:    "world",
		Reason:     "positive control",
	})
	deadline := time.After(time.Second)
	for audit.count(hooks.AuditHookSuppressed) == 0 {
		select {
		case <-audit.ch:
		case <-deadline:
			t.Fatalf("expected hook-caused pause transition to be depth-suppressed; audit=%v", audit.actions)
		}
	}
	hookDispatcher().Wait()

	if got := audit.count(hooks.AuditHookFired); got != 1 {
		t.Fatalf("pause hook should fire exactly once before depth suppression, got %d actions=%v", got, audit.actions)
	}
	status, err := mgr.GetStatus("scanner")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.PausedBy != "hook:pause-again" || status.PausedTrigger != "hook:pause-again" {
		t.Fatalf("hook pause provenance lost: by=%q trigger=%q", status.PausedBy, status.PausedTrigger)
	}
}

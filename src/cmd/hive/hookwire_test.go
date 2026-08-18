package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/hooks"
	"github.com/kubestellar/hive/pkg/timeline"
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

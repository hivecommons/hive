package main

// Tests for the hookwire.go emitters that had no coverage:
// installGovernorModeChangeEmitter (the post-commit governor_mode_change
// emission) and the nil guards of installUpgradePauseEmitter. The agent-pause
// emitter is exercised by hookwire_test.go's causation/loop test.
//
// installUpgradePauseEmitter's firing path is NOT covered here: the only way
// to reach hub.emitUpgradePause from outside pkg/hub is the admin-gated
// POST /api/saas/upgrade-pause handler, which needs a running HubServer. Its
// closure mirrors the governor emitter tested below; only the nil guard is
// reachable from this package.

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/timeline"
)

// snapshot returns a copy of the recorded audit actions, safe under -race.
func (a *hookWireAudit) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.actions...)
}

// governorForHookTests builds a real governor with the standard four-mode
// ladder, matching pkg/governor's own test fixture: pressure 25 crosses the
// surge threshold from the idle boot mode.
func governorForHookTests(t *testing.T) *governor.Governor {
	t.Helper()
	cadences := map[string]config.Cadence{"scanner": "15m"}
	cfg := config.GovernorConfig{
		Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 20, Cadences: cadences},
			"busy":  {Threshold: 10, Cadences: cadences},
			"quiet": {Threshold: 2, Cadences: cadences},
			"idle":  {Threshold: 0, Cadences: cadences},
		},
	}
	agents := map[string]config.AgentConfig{"scanner": {Enabled: true}}
	return governor.New(cfg, agents, hookTestLogger())
}

// TestInstallEmittersNilBackingObjectsAreSafe: wiring runs at startup before
// every subsystem necessarily exists; a nil governor or hub must be a no-op,
// not a crash.
func TestInstallEmittersNilBackingObjectsAreSafe(t *testing.T) {
	installGovernorModeChangeEmitter(nil)
	installUpgradePauseEmitter(nil)
}

// TestGovernorModeChangeEmitterIsNoOpWithoutHooks: the emitter is installed
// unconditionally, but with no hooks configured the dispatcher is nil and
// Fire must tolerate that — a mode change on a hookless hive cannot panic.
func TestGovernorModeChangeEmitterIsNoOpWithoutHooks(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	gov := governorForHookTests(t)
	installGovernorModeChangeEmitter(gov)

	gov.Evaluate(25, 0, 0, 0) // idle → surge with a nil dispatcher

	if s := gov.GetState(); s.Mode != governor.ModeSurge {
		t.Fatalf("mode change itself must still commit, got %s", s.Mode)
	}
}

// TestGovernorModeChangeEmitterFiresHookOnCommittedModeChange wires a real
// governor to a real dispatcher and drives a mode change through Evaluate —
// the same post-commit path production uses.
//
// The `when:` predicate is the payload assertion: it only matches when the
// emitter carried the committed From/To, the "system" actor, and a non-empty
// reason. If the emitter dropped or misfiled any of them, the hook would not
// fire at all and the test fails on the missing audit entry.
func TestGovernorModeChangeEmitterFiresHookOnCommittedModeChange(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	store := timeline.NewStore()
	audit := &hookWireAudit{ch: make(chan string, 8)}
	cfg := &config.Config{Hooks: []config.HookRule{{
		Name:   "on-surge",
		On:     "governor_mode_change",
		Action: "annotate",
		Params: map[string]string{"note": "governor went surge", "issue_ref": "governor"},
		When:   `t.from == "IDLE" && t.to == "SURGE" && t.actor == "system" && t.reason != ""`,
	}}}
	buildHookDispatcher(cfg, hookSinks{Timeline: store, Audit: audit}, hookTestLogger())

	gov := governorForHookTests(t)
	installGovernorModeChangeEmitter(gov)

	gov.Evaluate(25, 0, 0, 0) // pressure 25 crosses the surge threshold

	deadline := time.After(2 * time.Second)
	for audit.count(hooks.AuditHookFired) == 0 {
		select {
		case <-audit.ch:
		case <-deadline:
			t.Fatalf("expected the governor_mode_change hook to fire; audit=%v", audit.snapshot())
		}
	}
	hookDispatcher().Wait()

	if got := audit.count(hooks.AuditHookFired); got != 1 {
		t.Fatalf("hook should fire exactly once, got %d (%v)", got, audit.snapshot())
	}

	events := store.Recent(10)
	if len(events) != 1 {
		t.Fatalf("expected 1 timeline annotation, got %d", len(events))
	}
	e := events[0]
	if e.Attrs["note"] != "governor went surge" {
		t.Errorf("annotation note lost: %+v", e.Attrs)
	}
	if e.Attrs["hook"] != "on-surge" || e.Attrs["transition"] != "governor_mode_change" {
		t.Errorf("annotation must name the hook and transition: %+v", e.Attrs)
	}
}

// TestGovernorModeChangeEmitterSkipsWhenPredicateExcludesTransition is the
// negative control for the test above: the same wiring, a mode change the
// predicate does NOT match (idle → busy), and the hook must stay silent —
// proving the fired case fired on the payload, not on any mode change.
func TestGovernorModeChangeEmitterSkipsWhenPredicateExcludesTransition(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	audit := &hookWireAudit{ch: make(chan string, 8)}
	cfg := &config.Config{Hooks: []config.HookRule{{
		Name:   "on-surge",
		On:     "governor_mode_change",
		Action: "annotate",
		When:   `t.to == "SURGE"`,
	}}}
	buildHookDispatcher(cfg, hookSinks{Timeline: timeline.NewStore(), Audit: audit}, hookTestLogger())

	gov := governorForHookTests(t)
	installGovernorModeChangeEmitter(gov)

	gov.Evaluate(15, 0, 0, 0) // idle → busy: a real change the predicate excludes
	hookDispatcher().Wait()

	if got := audit.count(hooks.AuditHookFired); got != 0 {
		t.Fatalf("hook fired on a transition its predicate excludes: %v", audit.snapshot())
	}
}

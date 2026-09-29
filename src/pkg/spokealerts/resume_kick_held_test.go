package spokealerts

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
)

type heldSink struct {
	added   map[string]string
	cleared []string
}

func (s *heldSink) AddSystemAlert(id, severity, message string) {
	if s.added == nil {
		s.added = map[string]string{}
	}
	s.added[id] = severity + "|" + message
}

func (s *heldSink) ClearSystemAlert(id string) { s.cleared = append(s.cleared, id) }

func throttled(agents ...string) []ResumeKickHeldAgent {
	out := make([]ResumeKickHeldAgent, 0, len(agents))
	for _, a := range agents {
		out = append(out, ResumeKickHeldAgent{Agent: a, Reason: governor.ResumeRefusalIntervalThrottle})
	}
	return out
}

// TestResumeKickHeld_RaisesPerAgentAndClearsOnLaterKick: an interval-throttled
// agent gets a warning naming it; a kick recorded AFTER the hold clears it, an
// older kick does not, and an agent that was never held is untouched. This is
// also the interval_throttle regression for #9612: the banner still shows and
// still clears when its scheduled kick lands.
func TestResumeKickHeld_RaisesPerAgentAndClearsOnLaterKick(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 9, 50, 0, time.UTC)
	r := NewResumeKickHeld()
	r.now = func() time.Time { return now }
	sink := &heldSink{}

	r.Apply(sink, throttled("scanner", "reviewer"), func(a string) bool { return a == "scanner" })
	if got := sink.added[ResumeKickHeldAlertID("scanner")]; !strings.HasPrefix(got, "warning|") || !strings.Contains(got, "OOM-killed") || !strings.Contains(got, "memory limit") {
		t.Fatalf("scanner alert = %q", got)
	}
	if got := sink.added[ResumeKickHeldAlertID("reviewer")]; strings.Contains(got, "OOM") || !strings.Contains(got, "agent reviewer is idle") || !strings.Contains(got, "next scheduled slot") {
		t.Fatalf("reviewer alert = %q", got)
	}

	kicks := map[string]time.Time{"scanner": now.Add(-time.Minute), "reviewer": now.Add(2 * time.Minute)}
	r.ClearKicked(sink, func(a string) (time.Time, bool) { t, ok := kicks[a]; return t, ok })
	if len(sink.cleared) != 1 || sink.cleared[0] != ResumeKickHeldAlertID("reviewer") {
		t.Fatalf("cleared = %v, want only reviewer", sink.cleared)
	}
	if _, still := r.heldAt["scanner"]; !still {
		t.Fatal("scanner should remain held: its last kick predates the hold")
	}

	kicks["scanner"] = now.Add(time.Second)
	r.ClearKicked(sink, func(a string) (time.Time, bool) { t, ok := kicks[a]; return t, ok })
	if len(sink.cleared) != 2 || len(r.heldAt) != 0 {
		t.Fatalf("after later kick cleared=%v heldAt=%v", sink.cleared, r.heldAt)
	}
}

// TestResumeKickHeld_ReapplyKeepsOriginalHoldTime: a second refusal on the
// next eval cycle must not move the hold forward, or a kick that landed in
// between would never clear the alert.
func TestResumeKickHeld_ReapplyKeepsOriginalHoldTime(t *testing.T) {
	now := time.Date(2026, 9, 28, 21, 0, 0, 0, time.UTC)
	r := NewResumeKickHeld()
	r.now = func() time.Time { return now }
	sink := &heldSink{}
	r.Apply(sink, throttled("scanner"), nil)
	r.now = func() time.Time { return now.Add(5 * time.Minute) }
	r.Apply(sink, throttled("scanner"), nil)
	if got := r.heldAt["scanner"]; !got.Equal(now) {
		t.Fatalf("heldAt = %v, want original %v", got, now)
	}
	r.ClearKicked(sink, func(string) (time.Time, bool) { return now.Add(time.Minute), true })
	if len(sink.cleared) != 1 {
		t.Fatalf("kick between refusals should clear; cleared=%v", sink.cleared)
	}
}

// TestResumeKickHeld_NotRaisedForConfiguredIdle (#9612): a refusal that means
// "idle by configuration" (paused in mode, unscheduled, on-demand, not
// interval) or "budget exhausted" (covered by the fleet budget banner) never
// raises a per-agent banner, while an interval throttle in the same batch does.
func TestResumeKickHeld_NotRaisedForConfiguredIdle(t *testing.T) {
	r := NewResumeKickHeld()
	sink := &heldSink{}
	held := []ResumeKickHeldAgent{
		{Agent: "telemetry", Reason: governor.ResumeRefusalPausedInMode},
		{Agent: "guide", Reason: governor.ResumeRefusalOnDemand},
		{Agent: "architect", Reason: governor.ResumeRefusalUnscheduled},
		{Agent: "operations", Reason: governor.ResumeRefusalNotInterval},
		{Agent: "sec-check", Reason: governor.ResumeRefusalBudgetExhausted},
		{Agent: "scanner", Reason: governor.ResumeRefusalIntervalThrottle},
	}
	r.Apply(sink, held, nil)
	if len(sink.added) != 1 {
		t.Fatalf("alerts raised = %v, want only scanner", sink.added)
	}
	if _, ok := sink.added[ResumeKickHeldAlertID("scanner")]; !ok {
		t.Fatalf("interval-throttled scanner must still get its banner; added=%v", sink.added)
	}
	if len(r.heldAt) != 1 {
		t.Fatalf("tracked = %v, want only scanner", r.heldAt)
	}
}

// TestResumeKickHeld_StuckPausedCaseClearsNextCycle is the #9612 stuck case:
// an agent that carried a throttle banner is now refused because its cadence
// is paused in the current mode. The refusal raises nothing, and the existing
// banner clears on the next eval cycle through both paths: Apply with the new
// reason, and Reconcile reading the gate.
func TestResumeKickHeld_StuckPausedCaseClearsNextCycle(t *testing.T) {
	now := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)

	t.Run("via Apply", func(t *testing.T) {
		r := NewResumeKickHeld()
		r.now = func() time.Time { return now }
		sink := &heldSink{}
		r.Apply(sink, throttled("telemetry"), nil)
		r.Apply(sink, []ResumeKickHeldAgent{{Agent: "telemetry", Reason: governor.ResumeRefusalPausedInMode}}, nil)
		if len(sink.cleared) != 1 || sink.cleared[0] != ResumeKickHeldAlertID("telemetry") || len(r.heldAt) != 0 {
			t.Fatalf("cleared=%v heldAt=%v, want telemetry cleared", sink.cleared, r.heldAt)
		}
	})

	t.Run("via Reconcile", func(t *testing.T) {
		r := NewResumeKickHeld()
		r.now = func() time.Time { return now }
		sink := &heldSink{}
		r.Apply(sink, throttled("telemetry"), nil)
		r.now = func() time.Time { return now.Add(5 * time.Minute) }
		r.Reconcile(sink, ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) {
				return governor.ResumeRefusalPausedInMode, 0
			},
		})
		if len(sink.cleared) != 1 || len(r.heldAt) != 0 {
			t.Fatalf("cleared=%v heldAt=%v, want telemetry cleared", sink.cleared, r.heldAt)
		}
	})
}

// TestResumeKickHeld_ReconcileClearRules: each #9612 clear condition clears
// on its own, and with none of them true (gate still throttling, inside max
// age) the banner stays. The "stays" row is the negative control that proves
// each clearing row is clearing for its own reason.
func TestResumeKickHeld_ReconcileClearRules(t *testing.T) {
	raised := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	const interval = time.Hour
	throttleGate := func(string) (governor.ResumeRefusal, time.Duration) {
		return governor.ResumeRefusalIntervalThrottle, interval
	}
	cases := []struct {
		name      string
		elapsed   time.Duration
		facts     ResumeKickHeldFacts
		wantClear bool
	}{
		{name: "nothing changed stays", elapsed: 10 * time.Minute, facts: ResumeKickHeldFacts{
			LastKick:    func(string) (time.Time, bool) { return raised.Add(-time.Minute), true },
			Inactive:    func(string) bool { return false },
			WorkedSince: func(string, time.Time) bool { return false },
			Gate:        throttleGate,
		}},
		{name: "gate would now allow stays until kicked", elapsed: 10 * time.Minute, facts: ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) { return governor.ResumeAllowed, interval },
		}},
		{name: "kicked after raise", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			LastKick: func(string) (time.Time, bool) { return raised.Add(time.Minute), true },
			Gate:     throttleGate,
		}},
		{name: "operator-paused, disabled or removed", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			Inactive: func(string) bool { return true },
			Gate:     throttleGate,
		}},
		{name: "working again", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			WorkedSince: func(_ string, since time.Time) bool { return since.Equal(raised) },
			Gate:        throttleGate,
		}},
		{name: "mode change to paused", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) { return governor.ResumeRefusalPausedInMode, 0 },
		}},
		{name: "now unscheduled", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) { return governor.ResumeRefusalUnscheduled, 0 },
		}},
		{name: "now on-demand", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) { return governor.ResumeRefusalOnDemand, interval },
		}},
		{name: "budget ran out", elapsed: 10 * time.Minute, wantClear: true, facts: ResumeKickHeldFacts{
			Gate: func(string) (governor.ResumeRefusal, time.Duration) {
				return governor.ResumeRefusalBudgetExhausted, interval
			},
		}},
		{name: "just under max age stays", elapsed: ResumeKickHeldMaxAgeIntervals*interval - time.Second, facts: ResumeKickHeldFacts{
			Gate: throttleGate,
		}},
		{name: "max age of two intervals", elapsed: ResumeKickHeldMaxAgeIntervals * interval, wantClear: true, facts: ResumeKickHeldFacts{
			Gate: throttleGate,
		}},
		{name: "fallback max age when interval unknown", elapsed: ResumeKickHeldFallbackMaxAge, wantClear: true, facts: ResumeKickHeldFacts{}},
		{name: "under fallback max age stays", elapsed: ResumeKickHeldFallbackMaxAge - time.Second, facts: ResumeKickHeldFacts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewResumeKickHeld()
			r.now = func() time.Time { return raised }
			sink := &heldSink{}
			r.Apply(sink, throttled("scanner"), nil)
			r.now = func() time.Time { return raised.Add(tc.elapsed) }
			r.Reconcile(sink, tc.facts)
			_, stillHeld := r.heldAt["scanner"]
			if cleared := len(sink.cleared) == 1 && sink.cleared[0] == ResumeKickHeldAlertID("scanner"); cleared != tc.wantClear || stillHeld == tc.wantClear {
				t.Fatalf("cleared=%v heldAt=%v, wantClear=%v", sink.cleared, r.heldAt, tc.wantClear)
			}
		})
	}
}

// TestResumeKickHeldMaxAge: two cadence intervals, or the fallback when the
// interval is unknown.
func TestResumeKickHeldMaxAge(t *testing.T) {
	if got := ResumeKickHeldMaxAge(3 * time.Hour); got != 6*time.Hour {
		t.Fatalf("max age for 3h = %v, want 6h", got)
	}
	for _, unknown := range []time.Duration{0, -time.Second} {
		if got := ResumeKickHeldMaxAge(unknown); got != ResumeKickHeldFallbackMaxAge {
			t.Fatalf("max age for %v = %v, want fallback %v", unknown, got, ResumeKickHeldFallbackMaxAge)
		}
	}
}

// TestResumeKickHeldMessage_NamesReason: the banner names the gate's actual
// reason and remedy, and only an interval throttle offers "wait for its next
// scheduled slot" (#9612). The OOM variant keeps its cause and remedy.
func TestResumeKickHeldMessage_NamesReason(t *testing.T) {
	const slot = "wait for its next scheduled slot"
	cases := []struct {
		reason   governor.ResumeRefusal
		wantPart string
		wantSlot bool
	}{
		{governor.ResumeRefusalIntervalThrottle, "one resume kick per cadence interval", true},
		{governor.ResumeRefusalPausedInMode, "paused in the current mode", false},
		{governor.ResumeRefusalUnscheduled, "no cadence in the current mode", false},
		{governor.ResumeRefusalNotInterval, "not interval-based", false},
		{governor.ResumeRefusalOnDemand, "on-demand", false},
		{governor.ResumeRefusalBudgetExhausted, "budget is exhausted", false},
		{governor.ResumeRefusal("mystery"), "(mystery)", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			for _, oom := range []bool{false, true} {
				msg := ResumeKickHeldMessage("telemetry", tc.reason, oom)
				if !strings.HasPrefix(msg, "agent telemetry is idle at a fresh prompt: ") {
					t.Fatalf("msg = %q", msg)
				}
				if !strings.Contains(msg, tc.wantPart) {
					t.Fatalf("msg = %q, want it to name %q", msg, tc.wantPart)
				}
				if got := strings.Contains(msg, slot); got != tc.wantSlot {
					t.Fatalf("msg = %q, contains %q = %v, want %v", msg, slot, got, tc.wantSlot)
				}
				if got := strings.Contains(msg, "OOM-killed") && strings.Contains(msg, "raise the pod memory limit"); got != oom {
					t.Fatalf("oom=%v msg = %q", oom, msg)
				}
				if strings.Contains(msg, "—") {
					t.Fatalf("msg contains an em-dash: %q", msg)
				}
			}
		})
	}
}

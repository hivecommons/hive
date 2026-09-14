package contributorquota

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/rotation"
)

func pct(v int) *int { return &v }

func env(vals map[string]string) EnvSource {
	return func(k string) (string, bool) { v, ok := vals[k]; return v, ok }
}

func reading(remaining int) Reading {
	return Reading{State: StateAvailable, Limits: []rotation.LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: remaining}}}
}

func TestEvaluateBoundaryRefusesAtEquality(t *testing.T) {
	cfg := Config{Mode: ModeAsk, BaseReservePct: 20, TierReservePct: map[string]*int{}}
	for _, tc := range []struct {
		name      string
		remaining int
		wantAdmit bool
	}{
		{name: "equality refuses", remaining: 20, wantAdmit: false},
		{name: "one unit above admits", remaining: 21, wantAdmit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.Evaluate(reading(tc.remaining), TierMedium)
			if got.Admit != tc.wantAdmit {
				t.Fatalf("Admit=%v, want %v (decision=%+v)", got.Admit, tc.wantAdmit, got)
			}
		})
	}
}

func TestRequiredReserveMaxAndTierInheritance(t *testing.T) {
	cfg := Config{
		Mode:             ModeAsk,
		BaseReservePct:   20,
		ShortReservePct:  pct(25),
		WeeklyReservePct: pct(30),
		TierReservePct: map[string]*int{
			TierSimple:  nil,
			TierMedium:  nil,
			TierComplex: pct(35),
			TierUnknown: pct(40),
		},
	}
	for _, tc := range []struct {
		name string
		kind string
		tier string
		want int
	}{
		{name: "window reserve wins", kind: "weekly", tier: TierMedium, want: 30},
		{name: "tier reserve wins", kind: "weekly", tier: TierComplex, want: 35},
		{name: "unknown tier reserve wins", kind: "session", tier: TierUnknown, want: 40},
		{name: "unset tier inherits effective window reserve", kind: "session", tier: TierSimple, want: 25},
		{name: "unconfigured kind inherits base", kind: "weekly_scoped", tier: TierMedium, want: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.RequiredReserve(rotation.LimitWindow{Kind: tc.kind}, tc.tier)
			if got != tc.want {
				t.Fatalf("RequiredReserve=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestInFlightTaskNotInterruptedButNextAdmissionRefused(t *testing.T) {
	g := NewGuard(Config{Mode: ModeAsk, BaseReservePct: 20, TierReservePct: map[string]*int{}})
	g.StartTask()
	if got := g.Update(reading(20), TierMedium); !got.Admit || got.Reason != "active_task_not_interrupted" {
		t.Fatalf("active task was interrupted: %+v", got)
	}
	g.FinishTask()
	if got := g.Admit(TierMedium); got.Admit || !got.Wait || got.Reason != StateGuarded {
		t.Fatalf("next admission should be refused: %+v", got)
	}
}

func TestAutomaticResumeAndExplicitStayPaused(t *testing.T) {
	cfg := Config{Mode: ModeAsk, BaseReservePct: 20, TierReservePct: map[string]*int{}}
	g := NewGuard(cfg)
	if got := g.Update(reading(20), TierMedium); got.Admit || !g.Paused() {
		t.Fatalf("expected guarded pause: decision=%+v paused=%v", got, g.Paused())
	}
	if got := g.Update(reading(21), TierMedium); !got.Admit || g.Paused() {
		t.Fatalf("expected automatic resume: decision=%+v paused=%v", got, g.Paused())
	}

	g = NewGuard(cfg)
	g.Update(reading(20), TierMedium)
	g.StayPaused()
	if got := g.Update(reading(99), TierMedium); got.Admit || got.Reason != "explicit_pause" || !g.Paused() {
		t.Fatalf("explicit pause should suppress auto resume: decision=%+v paused=%v", got, g.Paused())
	}
}

func TestParseConfigInvalidPercentages(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{
		{name: "negative", key: "HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT", value: "-1"},
		{name: "too high", key: "HIVE_CONTRIBUTOR_QUOTA_WEEKLY_MIN_REMAINING_PCT", value: "101"},
		{name: "non numeric", key: "HIVE_CONTRIBUTOR_QUOTA_COMPLEX_MIN_REMAINING_PCT", value: "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig(env(map[string]string{tc.key: tc.value}))
			if err == nil {
				t.Fatal("ParseConfig error=nil, want invalid percentage error")
			}
			if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "0 to 100") {
				t.Fatalf("error %q is not actionable", err)
			}
		})
	}
}

func TestOffFullyDisablesGuard(t *testing.T) {
	cfg, err := ParseConfig(env(map[string]string{"HIVE_CONTRIBUTOR_QUOTA_GUARD": "off"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Reading{
		reading(0),
		{State: StateUnknown},
		{State: StateStale},
	} {
		if got := cfg.Evaluate(r, TierComplex); !got.Admit || got.Wait {
			t.Fatalf("off should admit despite %+v; got %+v", r, got)
		}
	}
}

func TestUnknownAndStaleWaitSafely(t *testing.T) {
	cfg := Config{Mode: ModeAsk, BaseReservePct: 20, TierReservePct: map[string]*int{}}
	for _, state := range []string{StateUnknown, StateStale} {
		t.Run(state, func(t *testing.T) {
			got := cfg.Evaluate(Reading{State: state}, TierSimple)
			if got.Admit || !got.Wait || got.Reason != state {
				t.Fatalf("%s should wait safely, got %+v", state, got)
			}
		})
	}
}

// An unrecognized window kind must not fail open. "weekly_scoped" was itself a
// late addition to Claude's reporting, so new kinds demonstrably appear, and a
// guard that silently skips them admits work precisely when a real limit is
// exhausted.
func TestUnrecognizedWindowKindStillGuards(t *testing.T) {
	cfg := Config{Mode: ModePause}

	exhausted := []rotation.LimitWindow{{ID: "daily", Kind: "daily", PctRemaining: 0}}
	got := cfg.Evaluate(Reading{State: StateAvailable, Limits: exhausted}, TierComplex)
	if got.Admit {
		t.Fatalf("exhausted unrecognized window admitted work: %+v", got)
	}
	if got.Reason != StateGuardedUnknownWindow {
		t.Errorf("Reason = %q, want %q so operators can tell the cases apart", got.Reason, StateGuardedUnknownWindow)
	}
	if got.WindowID != "daily" {
		t.Errorf("WindowID = %q, want the offending window to be named", got.WindowID)
	}

	// A healthy unrecognized window must not cause a spurious pause.
	healthy := []rotation.LimitWindow{{ID: "daily", Kind: "daily", PctRemaining: 90}}
	if got := cfg.Evaluate(Reading{State: StateAvailable, Limits: healthy}, TierComplex); !got.Admit {
		t.Errorf("healthy unrecognized window should admit, got %+v", got)
	}
}

// ContinueOnce is the operator escape from a guarded pause. It had no test at
// all, and "once" is the whole contract: it must admit exactly one task and
// then rearm, otherwise an operator who overrides a single pause silently
// disables the guard for the rest of the session.
func TestContinueOnceAdmitsExactlyOneTaskThenRearms(t *testing.T) {
	cfg := Config{Mode: ModePause}
	exhausted := Reading{State: StateAvailable, Limits: []rotation.LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 0}}}

	g := NewGuard(cfg)
	if d := g.Update(exhausted, TierComplex); d.Admit {
		t.Fatalf("exhausted quota should pause, got %+v", d)
	}
	if !g.Paused() {
		t.Fatal("guard should report paused")
	}

	g.ContinueOnce()
	if d := g.Admit(TierComplex); !d.Admit {
		t.Fatalf("ContinueOnce should admit the next task, got %+v", d)
	}

	// Finishing that task must rearm the guard while the quota is still spent.
	g.StartTask()
	g.FinishTask()
	if d := g.Admit(TierComplex); d.Admit {
		t.Errorf("guard must rearm after the single permitted task, got %+v", d)
	}
}

func TestAdmitRespectsModeOffAndExplicitPause(t *testing.T) {
	exhausted := Reading{State: StateAvailable, Limits: []rotation.LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 0}}}

	off := NewGuard(Config{Mode: ModeOff})
	off.Update(exhausted, TierComplex)
	if d := off.Admit(TierComplex); !d.Admit {
		t.Errorf("ModeOff must never block, got %+v", d)
	}

	healthy := Reading{State: StateAvailable, Limits: []rotation.LimitWindow{{ID: "weekly", Kind: "weekly", PctRemaining: 95}}}
	stay := NewGuard(Config{Mode: ModePause})
	stay.Update(healthy, TierComplex)
	stay.StayPaused()
	d := stay.Admit(TierComplex)
	if d.Admit {
		t.Errorf("an explicit pause must outrank a healthy reading, got %+v", d)
	}
	if d.Reason != "explicit_pause" {
		t.Errorf("Reason = %q, want explicit_pause", d.Reason)
	}
}

func TestParseConfigRejectsOutOfRangeAndUnparseablePercentages(t *testing.T) {
	for _, tc := range []struct{ name, key, val string }{
		{"base above 100", "HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT", "101"},
		{"base negative", "HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT", "-1"},
		{"base not a number", "HIVE_CONTRIBUTOR_QUOTA_MIN_REMAINING_PCT", "high"},
		{"optional short out of range", "HIVE_CONTRIBUTOR_QUOTA_SHORT_MIN_REMAINING_PCT", "250"},
		{"optional weekly not a number", "HIVE_CONTRIBUTOR_QUOTA_WEEKLY_MIN_REMAINING_PCT", "soon"},
		{"optional tier out of range", "HIVE_CONTRIBUTOR_QUOTA_COMPLEX_MIN_REMAINING_PCT", "-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := func(k string) (string, bool) {
				if k == tc.key {
					return tc.val, true
				}
				return "", false
			}
			if _, err := ParseConfig(env); err == nil {
				t.Errorf("%s=%q should be rejected, not silently defaulted", tc.key, tc.val)
			}
		})
	}
}

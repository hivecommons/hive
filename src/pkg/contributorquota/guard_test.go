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

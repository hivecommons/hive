package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin the continuous-mode liveness hole observed on hive's own
// reviewer: a kick at 19:14Z whose turn-end pane signal was missed left the
// agent idle at its prompt for 21h with zero further kicks, because the only
// re-arm path was RecordKickOutcome and every other recovery was keyed off
// kick attempts that never happened.

// ceilingTestGovernor builds a continuous "scanner" whose boot kick is due.
func ceilingTestGovernor(now *time.Time, agent config.AgentConfig) *Governor {
	agent.ContinuousCooldown = time.Minute
	return continuousTestGovernor(now, agent, "6h")
}

// kickWithoutOutcome delivers the boot kick and never records an outcome,
// simulating a missed turn-end signal.
func kickWithoutOutcome(g *Governor, now *time.Time) time.Time {
	g.RecordKick("scanner")
	return *now
}

func TestContinuousStaleTurnPresumedEndedAfterCeiling(t *testing.T) {
	now := time.Date(2026, 10, 18, 19, 14, 0, 0, time.UTC)
	g := ceilingTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousTurnCeiling: 30 * time.Minute})
	kickAt := kickWithoutOutcome(g, &now)

	// Inside the ceiling: the governor must keep believing the turn runs.
	now = kickAt.Add(29 * time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("re-kick fired before continuous_turn_ceiling elapsed")
	}
	if st := g.GetState().Continuous["scanner"]; !st.NextKick.IsZero() || st.PresumedTurnEnds != 0 {
		t.Fatalf("continuous state armed inside the ceiling: %+v", st)
	}

	// At the ceiling: presume the turn over and arm the cooldown.
	now = kickAt.Add(30 * time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("presumed end must arm the cooldown, not kick in the same tick")
	}
	st := g.GetState().Continuous["scanner"]
	if want := now.Add(time.Minute); !st.NextKick.Equal(want) {
		t.Fatalf("next kick after presumed end = %v, want %v", st.NextKick, want)
	}
	if st.PresumedTurnEnds != 1 {
		t.Fatalf("presumed turn ends = %d, want 1", st.PresumedTurnEnds)
	}
	rec, ok := g.kickOutcomes["scanner"]
	if !ok || rec.Kind != KickOutcomePresumedEnded || !rec.KickAt.Equal(kickAt) {
		t.Fatalf("synthetic outcome = %+v (ok=%v), want presumed-ended for kick %v", rec, ok, kickAt)
	}

	// After the cooldown the agent is kicked again instead of idling forever.
	now = st.NextKick
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("agent was not re-kicked after presumed turn end")
	}
	g.RecordKick("scanner")
	secondKick := now

	// The presumption is per kick: the new turn gets its own full ceiling.
	now = secondKick.Add(29 * time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("second turn presumed ended before its own ceiling")
	}
	now = secondKick.Add(30 * time.Minute)
	g.Evaluate(0, 0, 0, 0)
	if st := g.GetState().Continuous["scanner"]; st.PresumedTurnEnds != 2 {
		t.Fatalf("presumed turn ends after second stale turn = %d, want 2", st.PresumedTurnEnds)
	}
}

func TestContinuousStaleTurnDefaultCeiling(t *testing.T) {
	now := time.Date(2026, 10, 18, 19, 14, 0, 0, time.UTC)
	g := ceilingTestGovernor(&now, config.AgentConfig{Enabled: true})
	kickAt := kickWithoutOutcome(g, &now)

	now = kickAt.Add(config.DefaultContinuousTurnCeiling - time.Second)
	g.Evaluate(0, 0, 0, 0)
	if st := g.GetState().Continuous["scanner"]; !st.NextKick.IsZero() {
		t.Fatalf("default ceiling armed early: %+v", st)
	}
	now = kickAt.Add(config.DefaultContinuousTurnCeiling)
	g.Evaluate(0, 0, 0, 0)
	if st := g.GetState().Continuous["scanner"]; st.NextKick.IsZero() {
		t.Fatalf("default ceiling did not re-arm the stale turn: %+v", st)
	}
}

func TestContinuousStaleTurnCeilingDisabledByNegative(t *testing.T) {
	now := time.Date(2026, 10, 18, 19, 14, 0, 0, time.UTC)
	g := ceilingTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousTurnCeiling: -1})
	kickAt := kickWithoutOutcome(g, &now)

	now = kickAt.Add(21 * time.Hour)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("disabled ceiling still re-kicked")
	}
	if st := g.GetState().Continuous["scanner"]; !st.NextKick.IsZero() || st.PresumedTurnEnds != 0 {
		t.Fatalf("disabled ceiling armed the agent: %+v", st)
	}
}

func TestContinuousRealOutcomeInsideCeilingWinsOverPresumption(t *testing.T) {
	now := time.Date(2026, 10, 18, 19, 14, 0, 0, time.UTC)
	g := ceilingTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousTurnCeiling: 30 * time.Minute})
	kickAt := kickWithoutOutcome(g, &now)

	now = kickAt.Add(10 * time.Minute)
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now)
	endedAt := now

	now = kickAt.Add(31 * time.Minute)
	g.Evaluate(0, 0, 0, 0)
	st := g.GetState().Continuous["scanner"]
	if st.PresumedTurnEnds != 0 {
		t.Fatalf("real outcome was overridden by a presumption: %+v", st)
	}
	if rec := g.kickOutcomes["scanner"]; rec.Kind != KickOutcomeEnded || !rec.At.Equal(endedAt) {
		t.Fatalf("outcome record replaced: %+v", rec)
	}
}

func TestContinuousCeilingIgnoresCadenceModeAgents(t *testing.T) {
	now := time.Date(2026, 10, 18, 19, 14, 0, 0, time.UTC)
	g := outcomeTestGovernor(&now)
	g.agents["quality"] = config.AgentConfig{Backend: "claude", Enabled: true, ContinuousTurnCeiling: time.Minute}
	if !contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("never-kicked agent must be due")
	}
	g.RecordKick("quality")
	kickAt := now

	now = kickAt.Add(time.Hour)
	if contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("cadence-mode agent kicked inside its 6h cadence by the continuous ceiling")
	}
	if _, ok := g.kickOutcomes["quality"]; ok {
		t.Fatal("cadence-mode agent got a synthetic outcome")
	}
}

func TestEffectiveContinuousTurnCeiling(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, config.DefaultContinuousTurnCeiling},
		{20 * time.Minute, 20 * time.Minute},
		{-1, 0},
	}
	for _, tc := range cases {
		if got := (config.AgentConfig{ContinuousTurnCeiling: tc.in}).EffectiveContinuousTurnCeiling(); got != tc.want {
			t.Errorf("ceiling(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

package governor

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func continuousWakeGovernor(now *time.Time) *Governor {
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"reviewer": "continuous", "quality": "6h"}},
	}}, map[string]config.AgentConfig{
		"reviewer": {Backend: "copilot", Enabled: true},
		"quality":  {Backend: "copilot", Enabled: true},
	}, slog.Default())
	g.now = func() time.Time { return *now }
	return g
}

// TestNextContinuousKickAtTracksKickEnd pins the wake contract: once a
// continuous agent's kick turn ends, the governor exposes the re-kick time
// (end + cooldown) and the end itself, so the run loop can wake on it rather
// than waiting for the next cadence eval tick (the observed 7–15 min idle).
func TestNextContinuousKickAtTracksKickEnd(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 30, 0, 0, time.UTC)
	g := continuousWakeGovernor(&now)
	g.Evaluate(0, 0, 0, 0)

	// Enabling continuous arms an initial kick; the wake honors that too.
	if at, who := g.NextContinuousKickAt(); at.IsZero() || who != "reviewer" {
		t.Fatalf("initial continuous arm must be visible to the wake, got %v %q", at, who)
	}

	g.RecordKick("reviewer")
	kickAt := now
	now = now.Add(6 * time.Minute)
	g.RecordKickOutcome("reviewer", KickOutcomeEnded, "Kick complete — 3 reviews posted", kickAt, now)
	endedAt := now

	st := g.GetState().Continuous["reviewer"]
	if !st.LastKickEnd.Equal(endedAt) {
		t.Fatalf("LastKickEnd = %v, want %v", st.LastKickEnd, endedAt)
	}
	cooldown := g.agents["reviewer"].EffectiveContinuousCooldown()
	at, who := g.NextContinuousKickAt()
	if who != "reviewer" || !at.Equal(endedAt.Add(cooldown)) {
		t.Fatalf("NextContinuousKickAt = %v %q, want %v reviewer", at, who, endedAt.Add(cooldown))
	}

	// A failed delivery moves the wake to the backoff deadline, never earlier.
	now = at
	g.RecordKickFailure("reviewer", errors.New("CLI did not reach input prompt"), now)
	at2, _ := g.NextContinuousKickAt()
	if !at2.After(at) {
		t.Fatalf("backoff must push the wake later: %v -> %v", at, at2)
	}
	if st := g.GetState().Continuous["reviewer"]; !at2.Equal(st.BackoffUntil) {
		t.Fatalf("wake %v != BackoffUntil %v", at2, st.BackoffUntil)
	}
}

// TestNextContinuousKickAtIgnoresCadenceAndBlocked: a cadence agent never
// contributes, and a budget-blocked continuous agent (NextKick cleared) does
// not either, so the wake never spins on an agent the eval would skip.
func TestNextContinuousKickAtIgnoresCadenceAndBlocked(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 30, 0, 0, time.UTC)
	g := continuousWakeGovernor(&now)
	g.Evaluate(0, 0, 0, 0)
	g.mu.Lock()
	delete(g.state.Continuous, "reviewer")
	g.mu.Unlock()
	g.RecordKick("quality")
	g.RecordKickOutcome("quality", KickOutcomeEnded, "done", now, now.Add(time.Minute))
	if at, who := g.NextContinuousKickAt(); !at.IsZero() {
		t.Fatalf("cadence agent must not schedule a wake, got %v %q", at, who)
	}

	g.mu.Lock()
	g.state.Continuous["reviewer"] = ContinuousState{NextKick: now.Add(time.Minute), Blocked: "budget"}
	g.mu.Unlock()
	if at, who := g.NextContinuousKickAt(); !at.IsZero() {
		t.Fatalf("blocked agent must not schedule a wake, got %v %q", at, who)
	}
}

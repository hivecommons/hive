package governor

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests pin #7421 on the governor side: a kick that ended on a
// clarifying question or a policy stand-down must not be accounted like one
// that produced work.

func outcomeTestGovernor(now *time.Time) *Governor {
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"quality": "6h"}},
	}}, map[string]config.AgentConfig{
		"quality": {Backend: "claude", Enabled: true},
	}, slog.Default())
	g.now = func() time.Time { return *now }
	return g
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestQuestionOutcomeEarnsOneEarlyRekick: the observed shape — the agent
// answers a 6h-cadence kick with "What should I focus on?". Before, the
// governor waited the full 6h. Now it re-kicks after questionRekickDelay,
// exactly once: a second question inside the cooldown goes back to the
// cadence, so a model that answers every kick with a question cannot turn the
// cadence into a 5-minute token-burning loop.
func TestQuestionOutcomeEarnsOneEarlyRekick(t *testing.T) {
	now := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
	g := outcomeTestGovernor(&now)

	if !contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("never-kicked agent must be due")
	}
	g.RecordKick("quality")
	kickAt := now

	// Turn ends 4 minutes later on a question.
	now = now.Add(4 * time.Minute)
	g.RecordKickOutcome("quality", KickOutcomeQuestion, "What should I focus on?", kickAt, now)
	questionAt := now

	// Inside the re-kick delay: not yet.
	now = questionAt.Add(questionRekickDelay - time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("re-kick fired before questionRekickDelay elapsed")
	}
	// Past the delay, far inside the 6h cadence: due.
	now = questionAt.Add(questionRekickDelay + time.Second)
	if !contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("a question-ended kick must be re-kicked after the delay instead of waiting out the 6h cadence (#7421)")
	}
	if !g.KickOutcomes()["quality"].Rekicked {
		t.Fatal("the outcome was not marked re-kicked")
	}
	// The re-kick itself must not fire twice.
	g.RecordKick("quality")
	rekickAt := now
	now = now.Add(questionRekickDelay + time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("the same question outcome earned a second early re-kick")
	}

	// The agent asks AGAIN inside the cooldown: recorded, but no third try.
	now = rekickAt.Add(3 * time.Minute)
	g.RecordKickOutcome("quality", KickOutcomeQuestion, "Awaiting your kick or specific task assignment.", rekickAt, now)
	rec := g.KickOutcomes()["quality"]
	if !rec.Rekicked {
		t.Fatal("a repeat question inside the cooldown must inherit the used re-kick")
	}
	now = now.Add(questionRekickDelay + time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("a repeat question inside the cooldown re-kicked again — that is the token-burning loop")
	}
	// The cadence itself still works.
	now = rekickAt.Add(6*time.Hour + time.Minute)
	if !contains(g.Evaluate(0, 0, 0, 0), "quality") {
		t.Fatal("the normal cadence must still fire")
	}
}

// TestStandDownAndEndedOutcomesDoNotRekick: a stand-down is a legitimate
// refusal (re-kicking would just hit the same policy wall) and "ended" is not
// a no-op; neither may shorten the cadence.
func TestStandDownAndEndedOutcomesDoNotRekick(t *testing.T) {
	for _, kind := range []string{KickOutcomeStandDown, KickOutcomeNoOp, KickOutcomeEnded} {
		now := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
		g := outcomeTestGovernor(&now)
		g.Evaluate(0, 0, 0, 0)
		g.RecordKick("quality")
		kickAt := now
		now = now.Add(2 * time.Minute)
		g.RecordKickOutcome("quality", kind, "STAND DOWN.", kickAt, now)
		now = now.Add(questionRekickDelay + time.Hour)
		if contains(g.Evaluate(0, 0, 0, 0), "quality") {
			t.Errorf("outcome %q re-kicked early; only a clarifying question is a defect worth retrying", kind)
		}
	}
	now := time.Now()
	g := outcomeTestGovernor(&now)
	g.RecordKickOutcome("quality", KickOutcomeStandDown, "STAND DOWN.", now, now)
	if !g.KickOutcomes()["quality"].Blocked() {
		t.Error("a stand-down outcome must read as blocked for the dashboard")
	}
	g.RecordKickOutcome("quality", KickOutcomeQuestion, "?", now, now)
	if g.KickOutcomes()["quality"].Blocked() {
		t.Error("a question is a defect, not a legitimate block")
	}
}

// TestOutcomeStampsKickHistory: the persisted kick history must distinguish
// a fruitless kick from one that produced work — the record for the kick the
// outcome belongs to carries the verdict, older ones are untouched, and the
// verdict survives a state-file round trip via SeedKickHistory (which also
// rebuilds the per-agent outcome for the dashboard).
func TestOutcomeStampsKickHistory(t *testing.T) {
	now := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
	g := outcomeTestGovernor(&now)
	g.RecordKick("quality")
	first := now
	now = now.Add(6 * time.Hour)
	g.RecordKick("quality")
	second := now
	now = now.Add(3 * time.Minute)
	g.RecordKickOutcome("quality", KickOutcomeStandDown, "● STAND DOWN.", second, now)

	hist := g.KickHistory()
	if len(hist) != 2 {
		t.Fatalf("history = %d records, want 2", len(hist))
	}
	if hist[0].Outcome != "" || !hist[0].Timestamp.Equal(first) {
		t.Errorf("older kick was stamped: %+v", hist[0])
	}
	if hist[1].Outcome != KickOutcomeStandDown || hist[1].OutcomeReason != "● STAND DOWN." {
		t.Errorf("latest kick not stamped with the outcome: %+v", hist[1])
	}

	// A kick with no verdict (the turn is still running, or the manager never
	// classified it) stays unstamped — the history must never claim more than
	// it knows.
	now = now.Add(6 * time.Hour)
	g.RecordKick("quality")
	if h := g.KickHistory(); h[2].Outcome != "" {
		t.Errorf("a fresh kick carries an outcome it never had: %+v", h[2])
	}

	restored := New(config.GovernorConfig{}, nil, slog.Default())
	restored.SeedKickHistory(hist)
	rec, ok := restored.KickOutcomes()["quality"]
	if !ok || rec.Kind != KickOutcomeStandDown || rec.Reason != "● STAND DOWN." {
		t.Fatalf("restored outcome = %+v, want the stand-down rebuilt from history", rec)
	}
	if !rec.Rekicked {
		t.Error("a restored outcome must never earn an early re-kick (its observation time is not persisted)")
	}
}

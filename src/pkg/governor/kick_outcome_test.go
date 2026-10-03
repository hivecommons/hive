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

func continuousTestGovernor(now *time.Time, agent config.AgentConfig, cadence config.Cadence) *Governor {
	if agent.Backend == "" {
		agent.Backend = "claude"
	}
	agent.Continuous = true
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": cadence}},
	}}, map[string]config.AgentConfig{"scanner": agent}, slog.Default())
	g.now = func() time.Time { return *now }
	g.Evaluate(0, 0, 0, 0)
	return g
}

func TestContinuousOutcomeSchedulesAfterCooldown(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: 90 * time.Second}, "6h")
	g.RecordKick("scanner")
	kickAt := now
	now = now.Add(10 * time.Minute)
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now)

	st := g.GetState().Continuous["scanner"]
	if want := now.Add(90 * time.Second); !st.NextKick.Equal(want) {
		t.Fatalf("continuous next kick = %v, want %v", st.NextKick, want)
	}
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous kick fired before cooldown")
	}
	now = st.NextKick
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous kick did not fire after cooldown")
	}
}

func TestContinuousUpdateAgentsEnableArmsIdleAgent(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": "6h"}},
	}}, map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	}, slog.Default())
	g.now = func() time.Time { return now }
	g.Evaluate(0, 0, 0, 0)
	g.RecordKick("scanner")
	kickAt := now
	now = now.Add(10 * time.Minute)
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now)

	now = now.Add(5 * time.Minute)
	g.UpdateAgents(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, Continuous: true, ContinuousCooldown: 2 * time.Minute},
	})
	st := g.GetState().Continuous["scanner"]
	if want := now.Add(2 * time.Minute); !st.NextKick.Equal(want) {
		t.Fatalf("continuous next kick after enable = %v, want %v", st.NextKick, want)
	}
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous kick fired before enable cooldown")
	}
	now = st.NextKick
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous kick did not fire after enable cooldown")
	}
}

func TestContinuousUpdateAgentsDisableClearsState(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: time.Minute}, "6h")
	g.RecordKick("scanner")
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", now, now)
	if _, ok := g.GetState().Continuous["scanner"]; !ok {
		t.Fatal("continuous state was not created")
	}
	g.UpdateAgents(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	})
	if _, ok := g.GetState().Continuous["scanner"]; ok {
		t.Fatal("continuous state survived disabling continuous mode")
	}
}

func TestContinuousUpdateAgentsEnableBlockedSetsBlocked(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": "pause"}},
	}}, map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true},
	}, slog.Default())
	g.now = func() time.Time { return now }
	g.Evaluate(0, 0, 0, 0)

	g.UpdateAgents(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, Continuous: true, ContinuousCooldown: time.Minute},
	})
	st := g.GetState().Continuous["scanner"]
	if st.Blocked != "paused_in_mode" {
		t.Fatalf("continuous blocked = %q, want paused_in_mode", st.Blocked)
	}
	if !st.NextKick.IsZero() {
		t.Fatalf("continuous next kick scheduled despite paused cadence: %+v", st)
	}
}

func TestContinuousBootArmsWithoutDoubleKick(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle": {Cadences: map[string]config.Cadence{"scanner": "6h"}},
	}}, map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, Continuous: true, ContinuousCooldown: time.Minute},
	}, slog.Default())
	g.now = func() time.Time { return now }

	due := g.Evaluate(0, 0, 0, 0)
	if len(due) != 1 || due[0] != "scanner" {
		t.Fatalf("initial continuous boot due = %v, want scanner", due)
	}
	st := g.GetState().Continuous["scanner"]
	if want := now.Add(time.Minute); !st.NextKick.Equal(want) {
		t.Fatalf("initial continuous next kick = %v, want %v", st.NextKick, want)
	}
	g.RecordKick("scanner")
	now = now.Add(time.Minute)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous arm caused a second kick while initial boot turn had no outcome")
	}
}

func TestContinuousPerModeOnlyInSurge(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"idle":  {Cadences: map[string]config.Cadence{"scanner": "6h"}},
		"busy":  {Cadences: map[string]config.Cadence{"scanner": "1h"}},
		"surge": {Cadences: map[string]config.Cadence{"scanner": "continuous"}},
	}}, map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, ContinuousCooldown: time.Minute},
	}, slog.Default())
	g.now = func() time.Time { return now }
	g.SetMode(ModeBusy)
	if _, ok := g.GetState().Continuous["scanner"]; ok {
		t.Fatal("busy mode must not schedule continuous state for surge-only continuous cadence")
	}
	if _, _, blocker := g.continuousBlockerLocked("scanner"); blocker != "not_in_mode" {
		t.Fatalf("blocker = %q, want not_in_mode", blocker)
	}
}

func TestContinuousPerModeModeTransitionArmsAndClears(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := New(config.GovernorConfig{Modes: map[string]config.ModeConfig{
		"busy":  {Cadences: map[string]config.Cadence{"scanner": "1h"}},
		"surge": {Cadences: map[string]config.Cadence{"scanner": "continuous"}},
	}}, map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Enabled: true, ContinuousCooldown: 2 * time.Minute},
	}, slog.Default())
	g.now = func() time.Time { return now }
	g.SetMode(ModeBusy)
	g.RecordKick("scanner")
	kickAt := now
	now = now.Add(5 * time.Minute)
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now)

	now = now.Add(time.Minute)
	g.SetMode(ModeSurge)
	st := g.GetState().Continuous["scanner"]
	if want := now.Add(2 * time.Minute); !st.NextKick.Equal(want) {
		t.Fatalf("surge transition next kick = %v, want %v", st.NextKick, want)
	}
	g.SetMode(ModeBusy)
	if _, ok := g.GetState().Continuous["scanner"]; ok {
		t.Fatal("leaving the continuous mode must clear continuous state")
	}
}

func TestContinuousBoolShorthandAllNonQuietModes(t *testing.T) {
	ac := config.AgentConfig{Continuous: true}
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{mode: "idle", want: true},
		{mode: "busy", want: true},
		{mode: "surge", want: true},
		{mode: "quiet", want: false},
	} {
		if got := ac.ContinuousInMode(tc.mode, "1h"); got != tc.want {
			t.Fatalf("ContinuousInMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
	if !(config.AgentConfig{}).ContinuousInMode("quiet", "continuous") {
		t.Fatal("explicit continuous cadence must opt quiet mode in")
	}
}

func TestContinuousBusyTurnDoesNotUseCadence(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: time.Minute}, "30m")
	g.RecordKick("scanner")
	now = now.Add(time.Hour)
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous mode must not cadence-kick while the previous turn has not ended")
	}
}

func TestContinuousDoesNotScheduleWhenBlocked(t *testing.T) {
	cases := []struct {
		name    string
		agent   config.AgentConfig
		cadence config.Cadence
	}{
		{name: "paused by mode", agent: config.AgentConfig{Enabled: true}, cadence: "pause"},
		{name: "disabled", agent: config.AgentConfig{Enabled: false}, cadence: "6h"},
		{name: "operator paused", agent: config.AgentConfig{Enabled: true, Paused: true}, cadence: "6h"},
		{name: "on demand", agent: config.AgentConfig{Enabled: true, OnDemand: true}, cadence: "6h"},
	}
	for _, tc := range cases {
		now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
		g := continuousTestGovernor(&now, tc.agent, tc.cadence)
		g.RecordKick("scanner")
		g.RecordKickOutcome("scanner", KickOutcomeEnded, "", now, now.Add(time.Minute))
		if st := g.GetState().Continuous["scanner"]; !st.NextKick.IsZero() {
			t.Errorf("%s: scheduled next kick despite blocker: %+v", tc.name, st)
		}
	}
}

func TestContinuousFailedKickBacksOffAndCaps(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: time.Minute}, "6h")
	g.RecordKick("scanner")
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", now, now)

	for i := 0; i < 10; i++ {
		g.RecordKickFailure("scanner", errString("CLI did not reach input prompt"), now)
		now = g.GetState().Continuous["scanner"].BackoffUntil
	}
	st := g.GetState().Continuous["scanner"]
	if st.Backoff != continuousBackoffCap {
		t.Fatalf("backoff = %v, want capped at %v", st.Backoff, continuousBackoffCap)
	}
	if st.LastError == "" || st.Failures != 10 {
		t.Fatalf("backoff status incomplete: %+v", st)
	}
}

func TestContinuousBudgetThresholdFallsBackToCadence(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{
		Enabled:             true,
		ContinuousCooldown:  time.Minute,
		ContinuousBudgetPct: 80,
	}, "30m")
	g.SetBudgetLimit(1000)
	g.SeedBudget(800, map[string]int64{"scanner": 800}, nil, now)
	g.RecordKick("scanner")
	kickAt := now
	now = now.Add(time.Minute)
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now)

	st := g.GetState().Continuous["scanner"]
	if st.Blocked != "budget" {
		t.Fatalf("continuous blocked = %q, want budget", st.Blocked)
	}
	if !st.NextKick.IsZero() {
		t.Fatalf("continuous next kick scheduled despite budget threshold: %+v", st)
	}
	now = kickAt.Add(31 * time.Minute)
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("budget-held continuous mode should fall back to normal cadence")
	}
}

func TestContinuousCountersTrackKicksAndTokens(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: time.Minute}, "6h")
	g.SeedBudget(0, map[string]int64{"scanner": 100}, nil, now)
	g.RecordKick("scanner")
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", now, now)
	now = now.Add(time.Minute)
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous kick should be due")
	}
	g.RecordKick("scanner")
	g.UpdateBudgetFromTotals(250, map[string]int64{"scanner": 250}, nil)

	st := g.GetState().Continuous["scanner"]
	if st.Kicks != 1 {
		t.Fatalf("continuous kicks = %d, want 1", st.Kicks)
	}
	if st.TokensConsumed != 150 {
		t.Fatalf("continuous tokens = %d, want 150", st.TokensConsumed)
	}
}

func TestContinuousOffRestoresCadenceDue(t *testing.T) {
	now := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)
	g := continuousTestGovernor(&now, config.AgentConfig{Enabled: true, ContinuousCooldown: time.Hour}, "30m")
	g.RecordKick("scanner")
	kickAt := now
	g.RecordKickOutcome("scanner", KickOutcomeEnded, "", kickAt, now.Add(time.Minute))
	if contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("continuous cooldown should suppress cadence while enabled")
	}
	g.UpdateAgents(map[string]config.AgentConfig{"scanner": {Backend: "claude", Enabled: true}})
	now = kickAt.Add(31 * time.Minute)
	if !contains(g.Evaluate(0, 0, 0, 0), "scanner") {
		t.Fatal("turning continuous off should restore normal cadence scheduling")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

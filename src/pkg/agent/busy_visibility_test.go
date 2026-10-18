package agent

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

func busyVisibilityTestManager() *Manager {
	return NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), ProjectContext{})
}

func TestBusyVisibilityCounterResetsOnDeliveryAndRestart(t *testing.T) {
	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	now := time.Date(2026, 9, 29, 4, 16, 0, 0, time.UTC)

	m.markKickUndeliverableLocked(agent, now, "pane busy")
	m.markKickUndeliverableLocked(agent, now.Add(time.Minute), "pane busy")
	if agent.KicksUndeliverable != 2 {
		t.Fatalf("kicks undeliverable = %d, want 2", agent.KicksUndeliverable)
	}
	if !agent.BusySince.Equal(now) {
		t.Fatalf("busy since = %v, want %v", agent.BusySince, now)
	}

	m.resetBusyVisibilityLocked(agent)
	if agent.KicksUndeliverable != 0 || !agent.BusySince.IsZero() || agent.BusyCondition != "" {
		t.Fatalf("reset after delivery left busy state: %+v", agent)
	}

	m.markKickUndeliverableLocked(agent, now, "pane busy")
	m.invalidateKicksOnRestartLocked(agent, "test restart", false, false)
	if agent.KicksUndeliverable != 0 || !agent.BusySince.IsZero() || agent.BusyCondition != "" {
		t.Fatalf("restart reset left busy state: %+v", agent)
	}
}

func TestNewestCopilotTranscriptActivityScansMultipleSessions(t *testing.T) {
	home := t.TempDir()
	older := time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC)
	newer := older.Add(2 * time.Hour)
	writeEventsFile(t, home, "parent", older)
	writeEventsFile(t, home, "sub-agent", newer)
	if err := os.MkdirAll(filepath.Join(home, ".copilot", "session-state", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := newestCopilotTranscriptActivity(home)
	if !ok {
		t.Fatal("newestCopilotTranscriptActivity ok = false, want true")
	}
	if !got.Equal(newer) {
		t.Fatalf("newest transcript activity = %v, want %v", got, newer)
	}
}

func TestBusyConditionThresholdsAndDefaults(t *testing.T) {
	now := time.Date(2026, 9, 29, 5, 0, 0, 0, time.UTC)
	busySince := now.Add(-45 * time.Minute)
	lastActivity := now.Add(-31 * time.Minute)
	agent := &AgentProcess{
		Config:                 config.AgentConfig{},
		KicksUndeliverable:     busyUndeliverableThreshold,
		BusySince:              busySince,
		LastTranscriptActivity: lastActivity,
	}
	if got := agent.Config.EffectiveBusyNoActivityThreshold(); got != config.DefaultBusyNoActivityThreshold {
		t.Fatalf("default threshold = %v, want %v", got, config.DefaultBusyNoActivityThreshold)
	}
	condition, message := busyCondition(agent, now)
	if condition != busyNoActivityCondition {
		t.Fatalf("condition = %q (%s), want %q", condition, message, busyNoActivityCondition)
	}

	agent.KicksUndeliverable = busyUndeliverableThreshold - 1
	condition, _ = busyCondition(agent, now)
	if condition != "" {
		t.Fatalf("below kick threshold condition = %q, want empty", condition)
	}

	agent.KicksUndeliverable = busyUndeliverableThreshold
	agent.Config.BusyNoActivityThreshold = time.Hour
	condition, _ = busyCondition(agent, now)
	if condition != "" {
		t.Fatalf("below silence threshold condition = %q, want empty", condition)
	}

	agent.Config.MaxTurnDuration = 40 * time.Minute
	condition, message = busyCondition(agent, now)
	if condition != busyOverCeilingCondition {
		t.Fatalf("ceiling condition = %q (%s), want %q", condition, message, busyOverCeilingCondition)
	}

	oldKick := now.Add(-2 * time.Hour)
	idle := &AgentProcess{
		Config:   config.AgentConfig{MaxTurnDuration: 40 * time.Minute},
		LastKick: &oldKick,
	}
	condition, _ = busyCondition(idle, now)
	if condition != "" {
		t.Fatalf("idle old kick condition = %q, want empty", condition)
	}
}

func writeEventsFile(t *testing.T, home, id string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, ".copilot", "session-state", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant.message"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func kickFailedEvidence(tr *fleetreport.RuntimeTracker) []fleetreport.Evidence {
	var out []fleetreport.Evidence
	for _, ev := range tr.Evidence() {
		if ev.Class == fleetreport.ClassKickFailedRepeatedly {
			out = append(out, ev)
		}
	}
	return out
}

// Decision B on #12787: kicks that fail before busyNoActivityStall has had
// its turn are a dashboard warning, not a fleet issue. 8 undeliverable kicks
// over a 30m-silent transcript (the exact shape of the #12787 report) must
// not self-report.
func TestKickUndeliverableBeforeStallRestartIsNotSelfReported(t *testing.T) {
	clock := time.Date(2026, 10, 17, 16, 25, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return clock })
	fleetreport.SetDefaultTrackerForTest(t, tr)

	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	agent.LastTranscriptActivity = clock.Add(-config.DefaultBusyNoActivityThreshold - time.Minute)
	for i := 0; i < 8; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}
	if agent.KicksUndeliverable != 8 {
		t.Fatalf("kicks undeliverable = %d, want 8", agent.KicksUndeliverable)
	}
	if agent.BusyCondition != busyNoActivityCondition {
		t.Fatalf("busy condition = %q, want %q to stay visible on the dashboard", agent.BusyCondition, busyNoActivityCondition)
	}
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("self-reported before the stall restart was attempted: %q", evs[0].Message)
	}

	// Non-Copilot backends have no transcript cache; they too wait for the
	// restart instead of reporting on the third kick.
	agent.LastTranscriptActivity = time.Time{}
	for i := 0; i < 3; i++ {
		m.markKickUndeliverableLocked(agent, clock, "CLI did not reach input prompt")
	}
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("no-transcript agent self-reported before the stall restart: %q", evs[0].Message)
	}
}

func TestKickUndeliverableDuringLiveTurnIsNotSelfReported(t *testing.T) {
	clock := time.Date(2026, 10, 16, 3, 26, 26, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return clock })
	fleetreport.SetDefaultTrackerForTest(t, tr)

	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	// Even after a stall restart, a transcript written to seconds ago means
	// the pane is busy, not stuck (#12268).
	m.markBusyNoActivityRestartLocked(agent, clock.Add(-5*time.Minute))
	agent.LastTranscriptActivity = clock.Add(-20 * time.Second)
	for i := 0; i < 3; i++ {
		m.markKickUndeliverableLocked(agent, clock, "CLI did not reach input prompt")
	}
	if agent.KicksUndeliverable != 3 {
		t.Fatalf("kicks undeliverable = %d, want 3 (dashboard counter still tracks the episode)", agent.KicksUndeliverable)
	}
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("live turn produced kick_failed_repeatedly evidence: %q", evs[0].Message)
	}
}

// Scenario (b): the stall restart ran and the session came back — the next
// delivered kick clears the marker and the tracker, and nothing is filed.
func TestStallRestartThatRecoversClearsWithoutSelfReport(t *testing.T) {
	clock := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return clock })
	fleetreport.SetDefaultTrackerForTest(t, tr)

	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	agent.LastTranscriptActivity = clock.Add(-65 * time.Minute)
	for i := 0; i < busyNoActivityRecoveryKicks; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}

	// The restart loop marks the attempt, then the teardown resets counters.
	m.markBusyNoActivityRestartLocked(agent, clock)
	m.invalidateKicksOnRestartLocked(agent, "stalled session recovery", false, false)
	if agent.KicksUndeliverable != 0 || agent.BusyCondition != "" {
		t.Fatalf("restart reset left busy state: %+v", agent)
	}
	if agent.busyNoActivityRestartAt.IsZero() || agent.busyNoActivityRestarts != 1 {
		t.Fatalf("restart reset dropped the stall-restart marker: at=%v n=%d", agent.busyNoActivityRestartAt, agent.busyNoActivityRestarts)
	}

	// One more undeliverable kick while the new CLI boots is still below the
	// tracker's streak, then the kick lands.
	m.markKickUndeliverableLocked(agent, clock.Add(time.Minute), "pane not at a ready input prompt")
	m.recordDeliveredKickLocked(agent, "go", "governor")
	if !agent.busyNoActivityRestartAt.IsZero() || agent.busyNoActivityRestarts != 0 {
		t.Fatalf("delivered kick did not clear the stall-restart marker: %+v", agent)
	}
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("recovered stall was self-reported: %q", evs[0].Message)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 0 {
		// Nothing qualified, so nothing should surface as a clear either.
		t.Fatalf("recovered stall produced %d cleared conditions, want 0", len(cleared))
	}
}

// Scenario (c): the stall restart ran and kicks keep failing afterwards —
// exactly one kick_failed_repeatedly report, saying the restart did not help.
func TestStallRestartThatDoesNotRecoverSelfReportsOnce(t *testing.T) {
	clock := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return clock })
	fleetreport.SetDefaultTrackerForTest(t, tr)

	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	agent.LastTranscriptActivity = clock.Add(-65 * time.Minute)
	for i := 0; i < busyNoActivityRecoveryKicks; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}
	restartAt := clock
	m.markBusyNoActivityRestartLocked(agent, restartAt)
	m.invalidateKicksOnRestartLocked(agent, "stalled session recovery", false, false)

	// The restarted CLI never writes a transcript and refuses every kick.
	clock = clock.Add(12 * time.Minute)
	for i := 0; i < 4; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}
	evs := kickFailedEvidence(tr)
	if len(evs) != 1 {
		t.Fatalf("kick_failed_repeatedly evidence = %d, want exactly 1", len(evs))
	}
	msg := evs[0].Message
	for _, want := range []string{
		"after busy-no-activity restart did not recover the session",
		"kick undeliverable 4 times in a row",
		"transcript silent for 1h17m",
		"restarted 12m0s ago at " + restartAt.Format(time.RFC3339),
		"1 restart(s) this episode",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message = %q, want it to contain %q", msg, want)
		}
	}

	// Recovery later clears it through the normal delivered-kick path.
	m.recordDeliveredKickLocked(agent, "go", "governor")
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("evidence still present after recovery: %q", evs[0].Message)
	}
	if cleared := tr.TakeCleared(); len(cleared) != 1 || cleared[0].Class != fleetreport.ClassKickFailedRepeatedly {
		t.Fatalf("cleared conditions = %+v, want one kick_failed_repeatedly clear", cleared)
	}
}

// A stall restart that itself errors is reported without waiting for more
// kicks to pile up on a session that was never restarted.
func TestStallRestartErrorIsSelfReported(t *testing.T) {
	clock := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	tr := fleetreport.NewRuntimeTracker(func() time.Time { return clock })
	fleetreport.SetDefaultTrackerForTest(t, tr)

	m := busyVisibilityTestManager()
	agent := m.agents["scanner"]
	agent.KicksUndeliverable = busyNoActivityRecoveryKicks
	m.markBusyNoActivityRestartLocked(agent, clock)
	reason := busyNoActivityCondition + ": pane shows Working with no transcript activity for 1h5m; 6 kicks undeliverable"
	m.reportBusyNoActivityRestartFailed("scanner", reason, errors.New("tmux kill-session: no server running"))
	// The tracker still applies its streak: one observation is not yet
	// evidence, two more failing kicks finish it and the count carries all
	// three.
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("single restart error already qualified: %q", evs[0].Message)
	}
	for i := 0; i < 2; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}
	evs := kickFailedEvidence(tr)
	if len(evs) != 1 {
		t.Fatalf("kick_failed_repeatedly evidence = %d, want exactly 1", len(evs))
	}
	if evs[0].Count != 3 {
		t.Fatalf("count = %d, want 3 (restart error + 2 kicks)", evs[0].Count)
	}
	if !strings.Contains(evs[0].Message, "after busy-no-activity restart did not recover the session") {
		t.Fatalf("message = %q, want the post-restart wording", evs[0].Message)
	}
	m.reportBusyNoActivityRestartFailed("no-such-agent", reason, errors.New("x"))
}

func TestBusyNoActivityStallEscalatesOnlyPastRecoveryBars(t *testing.T) {
	now := time.Date(2026, 10, 16, 18, 52, 0, 0, time.UTC)
	working := "◉ Working (12m 3s · esc interrupt)"
	stuck := func() *AgentProcess {
		return &AgentProcess{
			Name:                   "quality",
			KicksUndeliverable:     14,
			BusySince:              now.Add(-65 * time.Minute),
			LastTranscriptActivity: now.Add(-95 * time.Minute),
		}
	}

	reason := busyNoActivityStall(stuck(), working, now)
	if !strings.Contains(reason, busyNoActivityCondition) || !strings.Contains(reason, "14 kicks undeliverable") {
		t.Fatalf("wedged pane reason = %q, want busy-no-activity restart reason", reason)
	}

	// No transcript cache (non-Copilot backends): silence is measured from
	// the first undeliverable kick.
	noCache := stuck()
	noCache.LastTranscriptActivity = time.Time{}
	noCache.BusySince = now.Add(-61 * time.Minute)
	if got := busyNoActivityStall(noCache, working, now); got == "" {
		t.Fatal("no-transcript wedged pane did not escalate")
	}

	cases := map[string]func(a *AgentProcess) string{
		"too few kicks": func(a *AgentProcess) string {
			a.KicksUndeliverable = busyNoActivityRecoveryKicks - 1
			return working
		},
		"transcript recently active": func(a *AgentProcess) string {
			a.LastTranscriptActivity = now.Add(-45 * time.Minute)
			return working
		},
		"longer configured threshold": func(a *AgentProcess) string {
			a.Config.BusyNoActivityThreshold = time.Hour
			return working
		},
		"pane no longer working": func(a *AgentProcess) string {
			return "❯ "
		},
		"within restart cooldown": func(a *AgentProcess) string {
			a.lastSessionStallRestart = now.Add(-time.Minute)
			return working
		},
		"no busy episode": func(a *AgentProcess) string {
			a.LastTranscriptActivity = time.Time{}
			a.BusySince = time.Time{}
			return working
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := stuck()
			pane := mutate(a)
			if got := busyNoActivityStall(a, pane, now); got != "" {
				t.Fatalf("busyNoActivityStall = %q, want no escalation", got)
			}
		})
	}
}

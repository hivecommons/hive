package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
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

// kickFailedEvidence is what the runtime has told its ConditionReporter
// about kick_failed_repeatedly and not cleared since.
func kickFailedEvidence(rec *conditionRecorder) []conditionObservation {
	return rec.active(crux.ClassKickFailedRepeatedly)
}

// Decision B on #12787: kicks that fail before busyNoActivityStall has had
// its turn are a dashboard warning, not a fleet issue. 8 undeliverable kicks
// over a 30m-silent transcript (the exact shape of the #12787 report) must
// not self-report.
func TestKickUndeliverableBeforeStallRestartIsNotSelfReported(t *testing.T) {
	clock := time.Date(2026, 10, 17, 16, 25, 0, 0, time.UTC)
	m := busyVisibilityTestManager()
	tr := recordConditions(t, m)
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
	m := busyVisibilityTestManager()
	tr := recordConditions(t, m)
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
	m := busyVisibilityTestManager()
	tr := recordConditions(t, m)
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

	// One more undeliverable kick while the new CLI boots is observed (the
	// host applies its streak before anything is filed), then the kick lands
	// and the recovery clears it.
	m.markKickUndeliverableLocked(agent, clock.Add(time.Minute), "pane not at a ready input prompt")
	if obs := tr.observations(crux.ClassKickFailedRepeatedly); len(obs) != 1 {
		t.Fatalf("post-restart kick failure observations = %d, want 1", len(obs))
	}
	m.recordDeliveredKickLocked(agent, "go", "governor")
	if !agent.busyNoActivityRestartAt.IsZero() || agent.busyNoActivityRestarts != 0 {
		t.Fatalf("delivered kick did not clear the stall-restart marker: %+v", agent)
	}
	if evs := kickFailedEvidence(tr); len(evs) != 0 {
		t.Fatalf("recovered stall left kick_failed_repeatedly open: %q", evs[0].Message)
	}
	if cleared := tr.clears(crux.ClassKickFailedRepeatedly); len(cleared) != 1 {
		t.Fatalf("recovered stall produced %d cleared conditions, want 1", len(cleared))
	}
}

// Scenario (c): the stall restart ran and kicks keep failing afterwards —
// exactly one kick_failed_repeatedly report, saying the restart did not help.
func TestStallRestartThatDoesNotRecoverSelfReportsOnce(t *testing.T) {
	clock := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	m := busyVisibilityTestManager()
	tr := recordConditions(t, m)
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
	// Every failing kick after the restart is observed; the host's streak
	// turns them into one report.
	evs := kickFailedEvidence(tr)
	if len(evs) != 4 {
		t.Fatalf("kick_failed_repeatedly observations = %d, want 4", len(evs))
	}
	msg := evs[len(evs)-1].Message
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
	if cleared := tr.clears(crux.ClassKickFailedRepeatedly); len(cleared) != 1 || cleared[0].Key != "scanner" {
		t.Fatalf("cleared conditions = %+v, want one kick_failed_repeatedly clear for scanner", cleared)
	}
}

// A stall restart that itself errors is reported without waiting for more
// kicks to pile up on a session that was never restarted.
func TestStallRestartErrorIsSelfReported(t *testing.T) {
	clock := time.Date(2026, 10, 17, 17, 0, 0, 0, time.UTC)
	m := busyVisibilityTestManager()
	tr := recordConditions(t, m)
	agent := m.agents["scanner"]
	agent.KicksUndeliverable = busyNoActivityRecoveryKicks
	m.markBusyNoActivityRestartLocked(agent, clock)
	reason := busyNoActivityCondition + ": pane shows Working with no transcript activity for 1h5m; 6 kicks undeliverable"
	m.reportBusyNoActivityRestartFailed("scanner", reason, errors.New("tmux kill-session: no server running"))
	// The restart error is observed at once; two more failing kicks follow
	// it so the host's streak sees all three.
	if evs := kickFailedEvidence(tr); len(evs) != 1 {
		t.Fatalf("restart error observations = %d, want 1", len(evs))
	}
	for i := 0; i < 2; i++ {
		m.markKickUndeliverableLocked(agent, clock, "pane not at a ready input prompt")
	}
	evs := kickFailedEvidence(tr)
	if len(evs) != 3 {
		t.Fatalf("kick_failed_repeatedly observations = %d, want 3 (restart error + 2 kicks)", len(evs))
	}
	if !strings.Contains(evs[2].Message, "after busy-no-activity restart did not recover the session") {
		t.Fatalf("message = %q, want the post-restart wording", evs[2].Message)
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

	// A live CLI stuck off its input prompt (#12916): every kick times out
	// with "CLI did not reach input prompt" yet the pane never shows Working.
	offPrompt := "GitHub Copilot v1.0.88\nLoading environment..."
	if got := busyNoActivityStall(stuck(), offPrompt, now); !strings.Contains(got, "pane shows no input prompt") {
		t.Fatalf("off-prompt wedged pane reason = %q, want no-input-prompt restart reason", got)
	}

	// A live CLI parked on a consent/blocking menu (#12921): the "❯"-selected
	// option satisfies paneShowsInputPrompt, but no kick is ever delivered.
	consent := "Bypass Permissions mode\n❯ 1. No, exit\n  2. Yes, I accept\nEnter to confirm"
	if got := busyNoActivityStall(stuck(), consent, now); !strings.Contains(got, "pane shows a consent or blocking prompt") {
		t.Fatalf("consent-screen wedged pane reason = %q, want consent/blocking-prompt restart reason", got)
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
		"bare shell left to crash recovery": func(a *AgentProcess) string {
			return "agent@hive:~$ "
		},
		"off-prompt pane too few kicks": func(a *AgentProcess) string {
			a.KicksUndeliverable = busyNoActivityRecoveryKicks - 1
			return offPrompt
		},
		"consent screen on inference backend left to dismissal": func(a *AgentProcess) string {
			a.Config.Backend = "vllm"
			return consent
		},
		"consent screen too few kicks": func(a *AgentProcess) string {
			a.KicksUndeliverable = busyNoActivityRecoveryKicks - 1
			return consent
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

// transcriptRefreshTestManager returns a Manager with one Copilot agent whose
// session-state tree lives under HOME (UID 0 agents run out of the process
// HOME), and that HOME.
func transcriptRefreshTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "copilot"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), ProjectContext{})
	return m, home
}

// AllStatuses is on the hub heartbeat's 10 s collect budget; it must copy the
// cached transcript liveness, never walk the session-state tree itself (a
// cold NFS walk of a 16k-session hive took 8–12 s). The refresher owns the
// walk.
func TestAllStatusesReadsCachedTranscriptActivityWithoutScanning(t *testing.T) {
	m, home := transcriptRefreshTestManager(t)
	mod := time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)
	writeEventsFile(t, home, "live", mod)

	if got := m.AllStatuses()["scanner"].LastTranscriptActivity; !got.IsZero() {
		t.Fatalf("AllStatuses scanned the session tree itself: LastTranscriptActivity = %v, want zero", got)
	}

	now := mod.Add(time.Minute)
	caches := map[string]*transcriptSessionCache{}
	if n := m.refreshTranscriptActivity(caches, func() time.Time { return now }, nil); n != 1 {
		t.Fatalf("refresh scanned %d agents, want 1", n)
	}
	if got := m.AllStatuses()["scanner"].LastTranscriptActivity; !got.Equal(mod) {
		t.Fatalf("LastTranscriptActivity after refresh = %v, want %v", got, mod)
	}
	// Inside the scan interval the agent is not re-scanned.
	if n := m.refreshTranscriptActivity(caches, func() time.Time { return now.Add(time.Second) }, nil); n != 0 {
		t.Fatalf("refresh inside the interval scanned %d agents, want 0", n)
	}
}

func TestRunTranscriptActivityRefreshScansOnTickAndStopsOnCancel(t *testing.T) {
	m, home := transcriptRefreshTestManager(t)
	first := time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)
	writeEventsFile(t, home, "s1", first)

	// The fake clock is read by the refresher goroutine and advanced by the
	// test, so it lives behind an atomic.
	var clock atomic.Int64
	clock.Store(first.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runTranscriptActivityRefresh(ctx, ticks, now)
	}()

	// The initial scan runs before the loop waits for its first tick, so a
	// tick send is only accepted once it has finished.
	later := first.Add(time.Hour)
	writeEventsFile(t, home, "s2", later)
	tick := first.Add(transcriptLivenessScanInterval)
	clock.Store(tick.UnixNano())
	ticks <- tick
	// Each further tick send proves the previous pass completed; two more
	// guarantee a pass that both started after s2 was written and finished
	// before the read below, whatever the initial scan observed.
	for range 2 {
		tick = tick.Add(transcriptLivenessScanInterval)
		clock.Store(tick.UnixNano())
		ticks <- tick
	}
	if got := m.AllStatuses()["scanner"].LastTranscriptActivity; !got.Equal(later) {
		t.Fatalf("LastTranscriptActivity after tick = %v, want %v", got, later)
	}

	cancel()
	<-done
}

// Between full rescans only sessions touched within transcriptHotSessionAge
// are stat'd; a cold session's cached mtime is trusted. The full rescan still
// notices a cold session that was resumed.
func TestNewestCopilotTranscriptActivityCachedSkipsColdSessions(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 21, 17, 0, 0, 0, time.UTC)
	hot := now.Add(-time.Minute)
	cold := now.Add(-transcriptHotSessionAge - time.Hour)
	writeEventsFile(t, home, "hot", hot)
	writeEventsFile(t, home, "cold", cold)
	if err := os.MkdirAll(filepath.Join(home, ".copilot", "session-state", "no-events"), 0o755); err != nil {
		t.Fatal(err)
	}

	cache := &transcriptSessionCache{}
	got, ok := newestCopilotTranscriptActivityCached(home, cache, now)
	if !ok || !got.Equal(hot) {
		t.Fatalf("full scan = %v, %v; want %v, true", got, ok, hot)
	}
	if !cache.lastFull.Equal(now) || len(cache.sessions) != 3 {
		t.Fatalf("cache after full scan = %+v, want lastFull=%v and 3 sessions", cache, now)
	}

	// The cold session is resumed: on the next (partial) pass it is not
	// stat'd, so the cached mtime wins and the newest stays the hot one.
	resumed := now.Add(time.Second)
	writeEventsFile(t, home, "cold", resumed)
	got, ok = newestCopilotTranscriptActivityCached(home, cache, now.Add(transcriptLivenessScanInterval))
	if !ok || !got.Equal(hot) {
		t.Fatalf("partial scan = %v, %v; want cached %v, true", got, ok, hot)
	}

	// The hot session IS re-stat'd on a partial pass.
	hotter := now.Add(2 * time.Second)
	writeEventsFile(t, home, "hot", hotter)
	got, _ = newestCopilotTranscriptActivityCached(home, cache, now.Add(2*transcriptLivenessScanInterval))
	if !got.Equal(hotter) {
		t.Fatalf("partial scan missed the hot session update: %v, want %v", got, hotter)
	}

	// A session directory that had no events.jsonl is re-stat'd on a partial
	// pass too, so a just-started session is seen as soon as its first event
	// lands rather than at the next full rescan.
	started := now.Add(3 * time.Second)
	writeEventsFile(t, home, "no-events", started)
	got, _ = newestCopilotTranscriptActivityCached(home, cache, now.Add(3*transcriptLivenessScanInterval))
	if !got.Equal(started) {
		t.Fatalf("partial scan missed the new session's first event: %v, want %v", got, started)
	}

	// The full rescan picks the resumed cold session up and drops a removed one.
	if err := os.RemoveAll(filepath.Join(home, ".copilot", "session-state", "no-events")); err != nil {
		t.Fatal(err)
	}
	full := now.Add(transcriptFullRescanInterval)
	got, _ = newestCopilotTranscriptActivityCached(home, cache, full)
	if !got.Equal(hotter) {
		t.Fatalf("full rescan newest = %v, want %v", got, hotter)
	}
	if !cache.sessions["cold"].Equal(resumed) {
		t.Fatalf("full rescan did not re-stat the cold session: %v, want %v", cache.sessions["cold"], resumed)
	}
	if _, still := cache.sessions["no-events"]; still || !cache.lastFull.Equal(full) {
		t.Fatalf("cache after full rescan = %+v, want no-events dropped and lastFull=%v", cache, full)
	}
}

// A pass over many agents must walk several at once; sequential walks are what
// pushed a 15-agent pass to 13 s against a 10 s interval.
func TestScanTranscriptActivityWalksAgentsConcurrently(t *testing.T) {
	orig := scanAgentTranscript
	t.Cleanup(func() { scanAgentTranscript = orig })

	started := make(chan struct{}, transcriptScanConcurrency)
	release := make(chan struct{})
	scanAgentTranscript = func(scan transcriptActivityScan, _ *transcriptSessionCache, _ time.Time) (time.Time, bool) {
		started <- struct{}{}
		<-release
		return time.Unix(int64(scan.uid), 0), true
	}
	var scans []transcriptActivityScan
	for i := range 2 * transcriptScanConcurrency {
		scans = append(scans, transcriptActivityScan{name: string(rune('a' + i)), uid: i + 1})
	}
	done := make(chan []transcriptActivityResult)
	go func() { done <- scanTranscriptActivity(scans, map[string]*transcriptSessionCache{}, time.Time{}) }()

	for range transcriptScanConcurrency {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("agent walks did not overlap")
		}
	}
	close(release)
	results := <-done
	if len(results) != len(scans) {
		t.Fatalf("got %d results, want %d", len(results), len(scans))
	}
	for i, r := range results {
		if r.name != scans[i].name || !r.ok || r.at.Unix() != int64(scans[i].uid) {
			t.Fatalf("result %d = %+v, want agent %s", i, r, scans[i].name)
		}
	}
}

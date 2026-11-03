package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

func TestRestartTelemetryPrunesRollingWindowAndReset(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	m.SeedRestartTelemetry("scanner", 7, []RestartEvent{
		{At: now.Add(-25 * time.Hour), Reason: "old"},
		{At: now.Add(-2 * time.Hour), Reason: "crash"},
	}, "crash")

	total, last24h, lastAt, reason, ok := m.RestartTelemetry("scanner")
	if !ok {
		t.Fatal("telemetry missing")
	}
	if total != 7 || last24h != 1 || reason != "crash" || lastAt.IsZero() {
		t.Fatalf("telemetry = total %d last24h %d lastAt %v reason %q", total, last24h, lastAt, reason)
	}
	if err := m.ResetRestartCount("scanner"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	total, last24h, _, _, _ = m.RestartTelemetry("scanner")
	if total != 0 || last24h != 0 {
		t.Fatalf("after reset total=%d last24h=%d, want zero", total, last24h)
	}
}

func TestRestartStormBreakerBacksOffAutomaticLoops(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	m.SeedRestartTelemetry("scanner", 3, []RestartEvent{
		{At: now.Add(-3 * time.Minute), Reason: "crash"},
		{At: now.Add(-2 * time.Minute), Reason: "crash"},
		{At: now.Add(-time.Minute), Reason: "crash"},
	}, "crash")

	plan := m.planAutomaticRestart("scanner", "crashed agent recovery", "", now)
	if !plan.allowed || plan.backoff != 0 || plan.pauseReason != "" {
		t.Fatalf("first post-fast restart plan = %+v, want allowed", plan)
	}

	plan = m.planAutomaticRestart("scanner", "crashed agent recovery", "", now.Add(30*time.Second))
	if plan.allowed || plan.backoff <= 0 || plan.pauseReason != "" {
		t.Fatalf("inside backoff plan = %+v, want suppressed with backoff", plan)
	}
}

func TestRestartStormBreakerPausesAfterWindowCap(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	var events []RestartEvent
	for i := 0; i < restartStormPauseAfter; i++ {
		events = append(events, RestartEvent{At: now.Add(-time.Duration(i) * time.Minute), Reason: "copilot hang diagnostic timed out"})
	}
	m.SeedRestartTelemetry("scanner", restartStormPauseAfter, events, "copilot hang diagnostic timed out")

	plan := m.planAutomaticRestart("scanner", "copilot hang diagnostic timed out", "copilot hung with no output (diagnostic timed out)", now)
	if plan.allowed || plan.backoff != 0 || plan.pauseReason == "" {
		t.Fatalf("storm plan = %+v, want pause", plan)
	}
	if want := "restart-storm: copilot hung with no output (diagnostic timed out)"; plan.pauseReason != want {
		t.Fatalf("pause reason = %q, want %q", plan.pauseReason, want)
	}
}

func TestRestartStormBreakerPausesCopilotWhenDataFull(t *testing.T) {
	old := statfsForPath
	statfsForPath = func(_ string, st *syscall.Statfs_t) error {
		st.Blocks = 100
		st.Bavail = 1
		return nil
	}
	t.Cleanup(func() { statfsForPath = old })
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "copilot"}}, slog.Default(), ProjectContext{})

	plan := m.planAutomaticRestart("scanner", "crashed agent recovery", "agent CLI crashed or tmux session disappeared", time.Now())
	if plan.pauseTrigger != DiskFullTrigger || plan.pauseReason == "" || plan.allowed {
		t.Fatalf("plan = %+v, want disk-full pause", plan)
	}
}

func TestRestartStormDiskFullReasonAndPaneTail(t *testing.T) {
	reason := diskFullPauseReason("copilot hang diagnostic timed out", "TAR_ENTRY_ERROR(Unknown system error -122)")
	if reason != "disk-full: TAR_ENTRY_ERROR(Unknown system error -122)" {
		t.Fatalf("diskFullPauseReason = %q", reason)
	}
	tail := compactPaneTail("\nfirst\n\nsecond\nthird\n", 2)
	if tail != "second | third" {
		t.Fatalf("compactPaneTail = %q", tail)
	}
}

func TestResetAndResumeClearRestartStormBreaker(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	m.mu.Lock()
	a := m.agents["scanner"]
	a.restartStormBackoffUntil = time.Now().Add(time.Hour)
	a.tokenRestartAttempts = 3
	a.tokenRestartGaveUp = true
	m.mu.Unlock()

	if err := m.ResetRestartCount("scanner"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	m.mu.RLock()
	if !m.agents["scanner"].restartStormBackoffUntil.IsZero() || m.agents["scanner"].tokenRestartAttempts != 0 || m.agents["scanner"].tokenRestartGaveUp {
		t.Fatalf("reset did not clear breaker fields: %+v", m.agents["scanner"])
	}
	m.mu.RUnlock()

	m.mu.Lock()
	m.agents["scanner"].Paused = true
	m.agents["scanner"].Config.Paused = true
	m.agents["scanner"].PausedTrigger = RestartStormTrigger
	m.agents["scanner"].PausedReason = "restart-storm: test"
	m.agents["scanner"].restartStormBackoffUntil = time.Now().Add(time.Hour)
	m.agents["scanner"].tokenRestartAttempts = 3
	m.agents["scanner"].tokenRestartGaveUp = true
	m.mu.Unlock()
	if err := m.Resume(context.Background(), "scanner", "dashboard-api", "operator resume"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	m.mu.RLock()
	if !m.agents["scanner"].restartStormBackoffUntil.IsZero() || m.agents["scanner"].tokenRestartAttempts != 0 || m.agents["scanner"].tokenRestartGaveUp {
		t.Fatalf("resume did not clear breaker fields: %+v", m.agents["scanner"])
	}
	m.mu.RUnlock()
}

func TestCompactPaneTailRedactsSecrets(t *testing.T) {
	pane := "starting\nvisit https://github.com/login/device\nfirst copy your one-time code: ABCD-1234\nANTHROPIC_API_KEY='sk-ant-abcdef123456'\ntoken ghp_abcdefghijklmnop1234\nlast line\n"
	tail := compactPaneTail(pane, 8)
	for _, secret := range []string{"ABCD-1234", "sk-ant-abcdef123456", "ghp_abcdefghijklmnop1234"} {
		if strings.Contains(tail, secret) {
			t.Fatalf("compactPaneTail leaked %q: %q", secret, tail)
		}
	}
	if !strings.Contains(tail, "starting") || !strings.Contains(tail, "last line") {
		t.Fatalf("compactPaneTail dropped non-secret lines: %q", tail)
	}
}

func TestRestartStormPauseDiagnosticsCarryWindowSummary(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	var events []RestartEvent
	for i := 0; i < restartStormPauseAfter; i++ {
		events = append(events, RestartEvent{At: now.Add(-time.Duration(i) * time.Minute), Reason: "crashed agent recovery"})
	}
	events = append(events, RestartEvent{At: now.Add(-3 * time.Hour), Reason: "too old"})
	m.SeedRestartTelemetry("scanner", len(events), events, "crashed agent recovery")
	m.mu.Lock()
	m.agents["scanner"].LastError = "bare shell prompt"
	m.mu.Unlock()

	plan := m.planAutomaticRestart("scanner", "crashed agent recovery", "agent CLI crashed or tmux session disappeared", now)
	if plan.pauseReason == "" {
		t.Fatalf("plan = %+v, want pause", plan)
	}
	want := "restart-storm: agent CLI crashed or tmux session disappeared\n" +
		"10 restarts in the last 1h0m0s (pause at 10)\n" +
		"- crashed agent recovery x10\n" +
		"last error: bare shell prompt"
	if plan.pauseDiagnostics != want {
		t.Fatalf("pauseDiagnostics = %q, want %q", plan.pauseDiagnostics, want)
	}
}

func TestRestartStormPauseDiagnosticsSkipLastErrorWhenItIsTheCause(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	now := time.Now()
	var events []RestartEvent
	for i := 0; i < restartStormPauseAfter; i++ {
		events = append(events, RestartEvent{At: now.Add(-time.Duration(i) * time.Minute), Reason: "credential refresh"})
	}
	m.SeedRestartTelemetry("scanner", len(events), events, "credential refresh")
	m.mu.Lock()
	m.agents["scanner"].LastError = "  token expired  "
	m.mu.Unlock()

	// Callers such as the credential path pass agent.LastError as the detail,
	// so the pause reason and the last error are the same text.
	plan := m.planAutomaticRestart("scanner", "credential refresh", m.agents["scanner"].LastError, now)
	if plan.pauseReason == "" {
		t.Fatalf("plan = %+v, want pause", plan)
	}
	if strings.Contains(plan.pauseDiagnostics, "last error:") {
		t.Fatalf("pauseDiagnostics = %q, want no duplicated last error line", plan.pauseDiagnostics)
	}
	if !strings.HasPrefix(plan.pauseDiagnostics, "restart-storm: token expired\n") {
		t.Fatalf("pauseDiagnostics = %q, want restart-storm cause first", plan.pauseDiagnostics)
	}
}

func TestRestartStormDiagnosticsWithoutAgentIsTheReason(t *testing.T) {
	const reason = "restart-storm: crashed"
	if got := restartStormDiagnostics(reason, nil, time.Now()); got != reason {
		t.Fatalf("restartStormDiagnostics(nil agent) = %q, want %q", got, reason)
	}
}

func TestRestartStormDiagnosticsBoundsReasonListAndSkipsUnstampedEvents(t *testing.T) {
	now := time.Now()
	agent := &AgentProcess{}
	// Seven distinct reasons: five are listed, two are folded into the tail.
	// The first reason repeats so the per-reason count is exercised too.
	reasons := []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	for _, r := range reasons {
		agent.RestartEvents = append(agent.RestartEvents, RestartEvent{At: now.Add(-time.Minute), Reason: r})
	}
	agent.RestartEvents = append(agent.RestartEvents,
		RestartEvent{At: now.Add(-time.Minute), Reason: "r1"},
		// Unstamped and out-of-window events contribute neither to the count
		// nor to the reason list.
		RestartEvent{Reason: "unstamped"},
		RestartEvent{At: now.Add(-restartStormWindow - time.Minute), Reason: "too old"},
	)

	got := restartStormDiagnostics("restart-storm: r1", agent, now)
	want := "restart-storm: r1\n" +
		fmt.Sprintf("8 restarts in the last %s (pause at %d)\n", restartStormWindow, restartStormPauseAfter) +
		"- r1 x2\n- r2 x1\n- r3 x1\n- r4 x1\n- r5 x1\n" +
		"... and 2 more reasons"
	if got != want {
		t.Fatalf("restartStormDiagnostics =\n%q\nwant\n%q", got, want)
	}
	for _, absent := range []string{"r6", "r7", "unstamped", "too old", "last error:"} {
		if strings.Contains(got, absent) {
			t.Fatalf("diagnostics must not mention %q:\n%s", absent, got)
		}
	}
}

func TestApplyRestartStormPlanFallsBackToPauseReasonForDiagnostics(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"scanner": {Backend: "claude"}}, slog.Default(), ProjectContext{})
	rec := recordConditions(t, m)

	// A plan built without diagnostics (older callers, or a plan whose
	// pauseDiagnostics was never populated) must still self-report the
	// pause reason rather than an empty excerpt.
	plan := restartStormPlan{pauseTrigger: RestartStormTrigger, pauseReason: "restart-storm: crashed"}
	handled, restarted, err := m.applyRestartStormPlan("scanner", "crashed agent recovery", plan, func() error {
		t.Fatal("a paused plan must not restart")
		return nil
	})
	if !handled || restarted || err != nil {
		t.Fatalf("applyRestartStormPlan = handled %v restarted %v err %v, want handled without restart", handled, restarted, err)
	}
	got := rec.observations(crux.ClassRestartStorm)
	want := []conditionObservation{{Class: crux.ClassRestartStorm, Key: "scanner", Message: "restart storm: crashed agent recovery", Diagnostics: "restart-storm: crashed"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("restart-storm observations = %+v, want %+v", got, want)
	}
}

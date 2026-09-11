package main

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// defaultPersistPaths pins the canonical /data file set the persistence loop
// writes. A typo here silently forks history files: the hive keeps writing the
// new path while every reader (and every prior boot's data) still points at
// the old one, so the golden values are asserted verbatim.
func TestDefaultPersistPathsPinsCanonicalDataFiles(t *testing.T) {
	got := defaultPersistPaths()
	want := persistPaths{
		ReachState:            "/data/reach-state.json",
		SparklineHistory:      "/data/sparkline-history.json",
		ModeHistory:           "/data/mode-history.json",
		TokenSparklineHistory: "/data/token-sparkline-history.json",
		FactHistory:           "/data/fact-history.json",
		CostHistory:           "/data/cost-history.json",
		TrendHistory:          "/data/trend-history.json",
		BudgetWindowHistory:   "/data/budget-window-history.json",
		ConvergenceSoak:       "/data/convergence-soak-history.json",
	}
	if got != want {
		t.Errorf("defaultPersistPaths() = %+v, want %+v", got, want)
	}
}

// An empty author must never classify as ours: enumeration responses with a
// dropped author field would otherwise feed every such PR into the fix-loop
// ledger as hive work.
func TestIsHiveAgentAuthorEmptyAuthorIsNotOurs(t *testing.T) {
	cfg := &config.Config{}
	if isHiveAgentAuthor(cfg, "") {
		t.Error(`isHiveAgentAuthor(cfg, "") = true, want false`)
	}
}

// With no explicit ai_author, the App bot identity (EffectiveAIAuthor) is what
// actually authors this hive's PRs — the classifier must recognize it via the
// EffectiveAIAuthor leg, and must still exclude dependency bots even though
// they carry the same "[bot]" suffix.
func TestIsHiveAgentAuthorRecognizesEffectiveAppBotAuthor(t *testing.T) {
	cfg := &config.Config{
		GitHub: config.GitHubConfig{
			AppID:          12345,
			InstallationID: 67,
			AppSlug:        "test-hive",
		},
	}
	if eff := cfg.EffectiveAIAuthor(); eff != "test-hive[bot]" {
		t.Fatalf("EffectiveAIAuthor() = %q, want %q (test premise broken)", eff, "test-hive[bot]")
	}
	if !isHiveAgentAuthor(cfg, "test-hive[bot]") {
		t.Error("App bot author (EffectiveAIAuthor) not classified as ours")
	}
	if isHiveAgentAuthor(cfg, "renovate[bot]") {
		t.Error("dependency bot classified as ours despite the [bot] suffix")
	}
	if isHiveAgentAuthor(cfg, "some-human") {
		t.Error("plain human author classified as ours")
	}
}

// BackendAuth carriage (#6558): a classified failure must ride to the hub with
// status, since, and the offending line; an ok or never-classified state must
// leave all three empty so the hub reads no-signal exactly like a legacy spoke.
func TestAgentActivityForBackendAuthOnlyCarriesFailures(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	since := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	failing := &agent.AgentProcess{BackendAuth: agent.BackendAuthState{
		Status:    agent.BackendAuthTokenExpired,
		Since:     since,
		LastError: "401 token expired",
	}}
	act := agentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", failing, nil)
	if act.BackendAuthStatus != agent.BackendAuthTokenExpired ||
		!act.BackendAuthSince.Equal(since) ||
		act.BackendAuthLastError != "401 token expired" {
		t.Errorf("failing backend auth did not ride through: status=%q since=%v lastError=%q",
			act.BackendAuthStatus, act.BackendAuthSince, act.BackendAuthLastError)
	}

	for name, proc := range map[string]*agent.AgentProcess{
		"explicit ok":      {BackendAuth: agent.BackendAuthState{Status: agent.BackendAuthOK, Since: since, LastError: "stale"}},
		"never classified": {},
	} {
		act := agentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)
		if act.BackendAuthStatus != "" || !act.BackendAuthSince.IsZero() || act.BackendAuthLastError != "" {
			t.Errorf("%s: backend auth fields not empty: status=%q since=%v lastError=%q",
				name, act.BackendAuthStatus, act.BackendAuthSince, act.BackendAuthLastError)
		}
	}
}

// A started process must report its real start time (the zero-time case for a
// never-started process is asserted in TestAgentActivityForRidesPauseProvenance).
func TestAgentActivityForCarriesStartedAt(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	started := time.Date(2026, 9, 10, 6, 30, 0, 0, time.UTC)
	proc := &agent.AgentProcess{StartedAt: &started}
	act := agentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", proc, nil)
	if !act.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", act.StartedAt, started)
	}
}

// Restart telemetry rides the manager, not the process snapshot: total count,
// the 24h window, and the last restart's time and reason must reach the hub so
// crash-loop detection sees the same numbers the spoke recorded.
func TestAgentActivityForCarriesRestartTelemetry(t *testing.T) {
	cfg := activityTestConfig()
	mgr := agent.NewManager(cfg.Agents, restoreTestLogger(), agent.ProjectContext{})
	lastAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	mgr.SeedRestartTelemetry("scanner", 5,
		[]agent.RestartEvent{{At: lastAt, Reason: "pane stall"}}, "seed fallback")

	act := agentActivityFor(mgr, cfg, governor.State{}, "busy", "scanner", &agent.AgentProcess{}, nil)
	if act.Restarts.Total != 5 {
		t.Errorf("Restarts.Total = %d, want 5", act.Restarts.Total)
	}
	if act.Restarts.Last24h != 1 {
		t.Errorf("Restarts.Last24h = %d, want 1", act.Restarts.Last24h)
	}
	if act.Restarts.LastReason != "pane stall" {
		t.Errorf("Restarts.LastReason = %q, want %q", act.Restarts.LastReason, "pane stall")
	}
	if want := lastAt.Format(time.RFC3339); act.Restarts.LastRestartAt != want {
		t.Errorf("Restarts.LastRestartAt = %q, want %q", act.Restarts.LastRestartAt, want)
	}
}

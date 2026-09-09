package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tracing"
	"github.com/hivecommons/hive/pkg/watchdog"
)

type persistStateFakeFleet struct {
	names []string
	conds map[string][]watchdog.Condition
}

func (f *persistStateFakeFleet) AgentNames() []string { return append([]string(nil), f.names...) }
func (f *persistStateFakeFleet) Observe(string) (watchdog.Observation, error) {
	return watchdog.Observation{}, nil
}
func (f *persistStateFakeFleet) IsPaused(string) bool                    { return false }
func (f *persistStateFakeFleet) Restart(context.Context, string) error   { return nil }
func (f *persistStateFakeFleet) Pause(string, string, string) error      { return nil }
func (f *persistStateFakeFleet) LastProduction(string) (time.Time, bool) { return time.Time{}, false }
func (f *persistStateFakeFleet) QueuedWork(string) (int, bool)           { return 0, true }
func (f *persistStateFakeFleet) SetConditions(name string, conds []watchdog.Condition) {
	f.conds[name] = append([]watchdog.Condition(nil), conds...)
}

func TestPersistStateRoundTripUsesInjectedPaths(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	oldRuntimeConfig := config.RuntimeConfigFile
	oldDashboardOverlay := config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		config.RuntimeConfigFile = oldRuntimeConfig
		config.DashboardOverlayFile = oldDashboardOverlay
	})

	statePath := filepath.Join(dir, "hive-state.json")
	cfgPath := filepath.Join(dir, "hive.yaml")
	level := 4
	staleTimeout := 37
	cfg := &config.Config{
		SourcePath: cfgPath,
		Project:    config.ProjectConfig{Org: "testorg", Name: "testhive", Repos: []string{"repo"}},
		Agents: map[string]config.AgentConfig{
			"scanner": {
				Role:            "scanner",
				Backend:         "claude",
				Model:           "sonnet",
				DisplayName:     "Scanner Agent",
				Description:     "Finds actionable work",
				Enabled:         true,
				ClearOnKick:     true,
				StaleTimeout:    staleTimeout,
				RestartStrategy: "always",
				LaunchCmd:       "hive-agent scanner",
			},
			"worker": {
				Role:    "worker",
				Backend: "claude",
				Model:   "sonnet",
				Enabled: true,
			},
		},
		Governor: config.GovernorConfig{Modes: map[string]config.ModeConfig{
			"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "5m"}},
			"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "15m"}},
			"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "30m"}},
			"idle":  {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1h"}},
		}},
		ACMMLevel: &level,
	}
	if err := os.WriteFile(cfgPath, []byte("project:\n  org: testorg\nagents:\n  scanner:\n    role: scanner\n"), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{ACMMLevel: level})
	pausedAt := time.Date(2026, 9, 9, 12, 1, 0, 0, time.UTC)
	lastKick := time.Date(2026, 9, 9, 12, 2, 0, 0, time.UTC)
	restartAt := time.Date(2026, 9, 9, 12, 3, 0, 0, time.UTC)
	if err := mgr.PauseBy("scanner", "dashboard-api", "owner maintenance", "owner@example"); err != nil {
		t.Fatalf("PauseBy: %v", err)
	}
	mgr.SeedPauseState("scanner", pausedAt, "dashboard-api", "owner maintenance", "owner@example")
	if err := mgr.Pause("worker", agent.BreakerTrigger, "fleet breaker engaged"); err != nil {
		t.Fatalf("Pause worker: %v", err)
	}
	mgr.SeedPauseState("worker", pausedAt, agent.BreakerTrigger, "fleet breaker engaged", "")
	if err := mgr.PinCLI("scanner", "copilot-cli-v1"); err != nil {
		t.Fatalf("PinCLI: %v", err)
	}
	if err := mgr.PinModel("scanner", "claude-sonnet-5"); err != nil {
		t.Fatalf("PinModel: %v", err)
	}
	if err := mgr.SetBackendOverride("scanner", "copilot"); err != nil {
		t.Fatalf("SetBackendOverride: %v", err)
	}
	mgr.SeedLastKick("scanner", lastKick)
	mgr.SeedKickHistory("scanner", []agent.KickRecord{{Timestamp: lastKick, Agent: "scanner", Snippet: "please scan"}})
	mgr.SeedRestartTelemetry("scanner", 3, []agent.RestartEvent{{At: restartAt, Reason: "watchdog"}}, "watchdog")
	sinceOutput := 2 * time.Minute
	mgr.SeedTurnLoss("scanner", agent.TurnLoss{
		Interruptions: 2,
		Producing:     1,
		UpperBound:    7 * time.Minute,
		Bytes:         2048,
		Recent: []agent.TurnInterruption{{
			At:          restartAt,
			Reason:      "restart",
			SinceKick:   5 * time.Minute,
			SinceOutput: &sinceOutput,
			Producing:   true,
			Bytes:       1024,
		}},
	})
	mgr.RestoreBreaker(true, []string{"worker"})

	gov := governor.New(cfg.Governor, cfg.Agents, logger)
	gov.SetBudgetLimit(9000)
	gov.SetBudgetIgnoreAll(true)
	gov.SetBudgetIgnored([]string{"scanner"})
	gov.SeedBudget(1234, map[string]int64{"scanner": 1111}, map[string]int64{"claude-sonnet-5": 2222}, restartAt)
	gov.SeedBudgetWindowBaseline(500)
	gov.SeedLastKicks(map[string]time.Time{"scanner": lastKick})
	gov.SeedKickHistory([]governor.KickRecord{{Timestamp: lastKick, Agent: "scanner"}})
	gov.SeedLastEval(restartAt)
	gov.Evaluate(25, 0, 0, 0)

	dashSrv := dashboard.NewServer(0, logger)
	dashSrv.SeedTokenSparklineHistory([]dashboard.TokenSparklineEntry{{Timestamp: 1, Input: 2, Output: 3, ByAgent: map[string]int64{"scanner": 5}}})
	dashSrv.SeedFactHistory([]dashboard.FactHistoryEntry{{Timestamp: 2, Count: 7}})
	dashSrv.SeedCostHistory([]dashboard.CostHistoryEntry{{Timestamp: 3, USD: 1.25, Agents: map[string]float64{"scanner": 0.75}}})
	dashSrv.SeedTrendHistory([]dashboard.TrendHistoryEntry{{Timestamp: 4, GovIssues: 5, GovPrs: 6, Repos: map[string]dashboard.TrendRepoSnap{"repo": {Issues: 1, PRs: 2}}}})
	dashSrv.SeedBudgetWindowHistory([]dashboard.BudgetWindowEntry{{WindowStart: 10, WindowEnd: 20, Limit: 9000, Used: 8000, PctUsed: 88.8, Exhausted: false}})
	dashSrv.SeedConvergenceSoak([]dashboard.ConvergenceSoakEntry{{Timestamp: 5, Commit: "abc1234", Mode: "shadow", Generation: 2, RawIssues: 9, Admitted: 8, Blocked: 1}})

	wdBackoff := restartAt.Add(10 * time.Minute)
	wdHealthy := restartAt.Add(-time.Minute)
	wdState := map[string]watchdog.PersistedAgent{"scanner": {
		Failures:     2,
		BackoffUntil: &wdBackoff,
		HealthySince: &wdHealthy,
		CrashLooping: true,
		Conditions: []watchdog.Condition{{
			Type:               watchdog.ConditionReady,
			Status:             watchdog.ConditionFalse,
			Reason:             "ShellPrompt",
			Message:            "agent is back at a shell",
			LastTransitionTime: restartAt,
		}},
	}}
	wdSettings := watchdog.DefaultSettings()
	wdSettings.Mode = watchdog.ModeHeal
	wd := watchdog.New(wdSettings, &persistStateFakeFleet{names: []string{"scanner"}, conds: map[string][]watchdog.Condition{}}, nil, logger)
	wd.Restore(wdState)

	paths := persistStateTestPaths(dir)
	_, span := tracing.StartSpan(context.Background(), "governor.persist_state_test")
	span.End()

	persistStateWithPaths(mgr, gov, cfg, statePath, logger, dashSrv, wd, paths)

	got, err := snapshot.LoadState(statePath, logger)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got == nil {
		t.Fatal("LoadState returned nil")
	}
	assertAgentState(t, got.Agents["scanner"], snapshot.AgentState{
		Paused:            true,
		PausedAt:          &pausedAt,
		PausedReason:      "owner maintenance",
		PausedTrigger:     "dashboard-api",
		PausedBy:          "owner@example",
		PinnedCLI:         "copilot-cli-v1",
		PinnedModel:       "claude-sonnet-5",
		ModelOverride:     "claude-sonnet-5",
		BackendOverride:   "copilot",
		RestartCount:      3,
		RestartEvents:     []snapshot.AgentRestartEvent{{At: restartAt, Reason: "watchdog"}},
		LastRestartReason: "watchdog",
		DisplayName:       "Scanner Agent",
		Description:       "Finds actionable work",
		Enabled:           boolPtr(true),
		ClearOnKick:       boolPtr(true),
		StaleTimeout:      intPtr(staleTimeout),
		RestartStrategy:   "always",
		LaunchCmd:         "hive-agent scanner",
		LastKick:          &lastKick,
		KickHistory:       []snapshot.AgentKickEntry{{Timestamp: lastKick, Agent: "scanner", Snippet: "please scan"}},
		TurnLoss: &snapshot.AgentTurnLoss{
			Interruptions: 2,
			Producing:     1,
			UpperBoundS:   420,
			Bytes:         2048,
			Recent: []snapshot.AgentTurnInterruption{{
				At:           restartAt,
				Reason:       "restart",
				SinceKickS:   300,
				SinceOutputS: floatPtr(120),
				Producing:    true,
				Bytes:        1024,
			}},
		},
	})

	if got.GovernorMode != string(governor.ModeSurge) {
		t.Fatalf("GovernorMode = %q, want %q", got.GovernorMode, governor.ModeSurge)
	}
	if got.BudgetLimit != 9000 || !got.BudgetIgnoreAll || !reflect.DeepEqual(got.BudgetIgnored, []string{"scanner"}) {
		t.Fatalf("budget controls not persisted: %+v", got)
	}
	if got.BudgetSpend != 1234 || got.BudgetWindowBaseline != 500 || !got.BudgetResetAt.Equal(restartAt) {
		t.Fatalf("budget window not persisted: spend=%d baseline=%d reset=%v", got.BudgetSpend, got.BudgetWindowBaseline, got.BudgetResetAt)
	}
	if !reflect.DeepEqual(got.BudgetByAgent, map[string]int64{"scanner": 1111}) || !reflect.DeepEqual(got.BudgetByModel, map[string]int64{"claude-sonnet-5": 2222}) {
		t.Fatalf("budget breakdowns not persisted: agents=%v models=%v", got.BudgetByAgent, got.BudgetByModel)
	}
	if !got.LastKicks["scanner"].Equal(lastKick) || len(got.KickHistory) != 1 || !got.KickHistory[0].Timestamp.Equal(lastKick) || got.KickHistory[0].Agent != "scanner" {
		t.Fatalf("governor kick history not persisted: last=%v history=%v", got.LastKicks, got.KickHistory)
	}
	if !got.LastEval.After(restartAt.Add(-time.Second)) {
		t.Fatalf("LastEval was not updated by Evaluate/persistState: %v", got.LastEval)
	}
	if got.ACMMLevel == nil || *got.ACMMLevel != level {
		t.Fatalf("ACMMLevel = %v, want %d", got.ACMMLevel, level)
	}
	if got.Breaker == nil || !got.Breaker.Engaged || !reflect.DeepEqual(got.Breaker.Paused, []string{"worker"}) {
		t.Fatalf("breaker not persisted: %+v", got.Breaker)
	}
	if !reflect.DeepEqual(got.Watchdog, wdState) {
		t.Fatalf("watchdog mismatch:\n got: %#v\nwant: %#v", got.Watchdog, wdState)
	}
	if got.CadenceOverrides["surge"]["scanner"] != "5m" || got.CadenceOverrides["idle"]["scanner"] != "1h" {
		t.Fatalf("cadence overrides not persisted: %#v", got.CadenceOverrides)
	}

	assertJSONRoundTrip(t, dir, paths.SparklineHistory, gov.EvalHistory())
	assertJSONRoundTrip(t, dir, paths.ModeHistory, gov.ModeHistory())
	assertJSONRoundTrip(t, dir, paths.TokenSparklineHistory, dashSrv.TokenSparklineHistory())
	assertJSONRoundTrip(t, dir, paths.FactHistory, dashSrv.FactHistory())
	assertJSONRoundTrip(t, dir, paths.CostHistory, dashSrv.CostHistory())
	assertJSONRoundTrip(t, dir, paths.TrendHistory, dashSrv.TrendHistory())
	assertJSONRoundTrip(t, dir, paths.BudgetWindowHistory, dashSrv.BudgetWindowHistory())
	assertJSONRoundTrip(t, dir, paths.ConvergenceSoak, dashSrv.ConvergenceSoakHistory())
	if _, err := os.Stat(paths.ReachState); err != nil {
		t.Fatalf("reach state was not written to injected path %q: %v", paths.ReachState, err)
	}
}

func persistStateTestPaths(dir string) persistPaths {
	return persistPaths{
		ReachState:            filepath.Join(dir, "reach-state.json"),
		SparklineHistory:      filepath.Join(dir, "sparkline-history.json"),
		ModeHistory:           filepath.Join(dir, "mode-history.json"),
		TokenSparklineHistory: filepath.Join(dir, "token-sparkline-history.json"),
		FactHistory:           filepath.Join(dir, "fact-history.json"),
		CostHistory:           filepath.Join(dir, "cost-history.json"),
		TrendHistory:          filepath.Join(dir, "trend-history.json"),
		BudgetWindowHistory:   filepath.Join(dir, "budget-window-history.json"),
		ConvergenceSoak:       filepath.Join(dir, "convergence-soak-history.json"),
	}
}

func assertAgentState(t *testing.T, got, want snapshot.AgentState) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("agent state mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func assertJSONRoundTrip[T any](t *testing.T, dir, path string, want []T) {
	t.Helper()
	if !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
		t.Fatalf("persist path %q is outside fixture dir %q", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	wantData, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want %s: %v", filepath.Base(path), err)
	}
	var gotJSON any
	var wantJSON any
	if err := json.Unmarshal(data, &gotJSON); err != nil {
		t.Fatalf("unmarshal got %s: %v", filepath.Base(path), err)
	}
	if err := json.Unmarshal(wantData, &wantJSON); err != nil {
		t.Fatalf("unmarshal want %s: %v", filepath.Base(path), err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("%s mismatch:\n got: %#v\nwant: %#v", filepath.Base(path), gotJSON, wantJSON)
	}
}

func floatPtr(v float64) *float64 { return &v }

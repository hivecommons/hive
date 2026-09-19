package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/snapshot"
)

type bootStateFake struct {
	deps bootStateDeps

	saved   *snapshot.PersistedState
	loadErr error

	savedStates  []*snapshot.PersistedState
	savedConfigs int
	saveStateErr error
	saveCfgErr   error
}

func newBootStateFake(saved *snapshot.PersistedState) *bootStateFake {
	f := &bootStateFake{saved: saved}
	f.deps = bootStateDeps{
		loadState: func(*slog.Logger) (*snapshot.PersistedState, error) { return f.saved, f.loadErr },
		saveState: func(s *snapshot.PersistedState, _ *slog.Logger) error {
			f.savedStates = append(f.savedStates, s)
			return f.saveStateErr
		},
		saveConfig: func(*config.Config) error { f.savedConfigs++; return f.saveCfgErr },
	}
	return f
}

func bootStateConfig() *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "boot-state-test"
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"acme/widgets"}
	cfg.Agents = map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
		"fixer":   {Enabled: true, Backend: "copilot", Model: "gpt-5.4"},
	}
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"BUSY": {Threshold: 5},
	}
	return cfg
}

func newBootStateBoot(t *testing.T, cfg *config.Config) *boot {
	t.Helper()
	b, _ := newDepsTestBoot(t, cfg)
	b.gov = governor.New(cfg.Governor, cfg.EnabledAgents(), b.logger)
	b.agentMgr = agent.NewManager(cfg.Agents, b.logger, agent.ProjectContext{})
	b.ghClient = fakeGitHubClient(t)
	return b
}

func TestBootStateWithNoSnapshotAppliesConfigBudget(t *testing.T) {
	cfg := bootStateConfig()
	cfg.Governor.Budget.TotalTokens = 9000
	b := newBootStateBoot(t, cfg)
	f := newBootStateFake(nil)

	b.bootStateWith(f.deps)

	if b.saved != nil {
		t.Fatal("saved must stay nil without a snapshot")
	}
	if got := b.gov.GetBudget().WeeklyLimit; got != 9000 {
		t.Fatalf("budget limit = %d, want config's 9000", got)
	}
	if len(f.savedStates) != 0 || f.savedConfigs != 0 {
		t.Fatal("no snapshot must mean no writes")
	}
}

func TestBootStateWithLoadErrorIsWarningOnly(t *testing.T) {
	b := newBootStateBoot(t, bootStateConfig())
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootStateFake(nil)
	f.loadErr = errors.New("corrupt json")

	b.bootStateWith(f.deps)

	if !strings.Contains(log.String(), "failed to load persisted state") {
		t.Fatalf("log:\n%s", log.String())
	}
	if b.saved != nil {
		t.Fatal("saved must be nil on load error")
	}
}

func TestBootStateWithRestoresGovernorAndAgents(t *testing.T) {
	cfg := bootStateConfig()
	b := newBootStateBoot(t, cfg)
	kickAt := time.Now().Add(-time.Hour).Truncate(time.Second)
	evalAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	lvl := 4
	saved := &snapshot.PersistedState{
		Agents: map[string]snapshot.AgentState{
			"scanner": {Paused: true, PausedReason: "fleet breaker", PausedTrigger: agent.BreakerTrigger},
			"fixer":   {Paused: true, PausedReason: "operator hold"},
			"ghost":   {Paused: true},
		},
		// The breaker lists both, but only re-adopts the agent whose persisted
		// pause was the breaker's — fixer's operator hold stays independent.
		Breaker:         &snapshot.BreakerState{Engaged: true, Paused: []string{"scanner", "fixer"}},
		GovernorMode:    string(governor.ModeSurge),
		BudgetLimit:     5000,
		BudgetIgnoreAll: true,
		BudgetIgnored:   []string{"fixer"},
		BudgetSpend:     1200,
		BudgetByAgent:   map[string]int64{"scanner": 1200},
		BudgetResetAt:   kickAt,
		CadenceOverrides: map[string]map[string]config.Cadence{
			"BUSY":    {"scanner": config.Cadence("10m")},
			"UNKNOWN": {"scanner": config.Cadence("1m")},
		},
		LastKicks:   map[string]time.Time{"scanner": kickAt},
		KickHistory: []snapshot.GovKickEntry{{Timestamp: kickAt, Agent: "scanner", Outcome: "ended"}},
		LastEval:    evalAt,
		ACMMLevel:   &lvl,
	}
	f := newBootStateFake(saved)

	b.bootStateWith(f.deps)

	if b.saved != saved {
		t.Fatal("saved snapshot not handed off")
	}
	if !b.agentMgr.IsPaused("scanner") || !b.agentMgr.IsPaused("fixer") {
		t.Fatal("persisted pauses not restored")
	}
	if engaged, held := b.agentMgr.BreakerState(); !engaged || len(held) != 1 || held[0] != "scanner" {
		t.Fatalf("breaker = %v %v", engaged, held)
	}
	st := b.gov.GetState()
	if st.Mode != governor.ModeSurge {
		t.Fatalf("mode = %v", st.Mode)
	}
	if !st.LastEval.Equal(evalAt) {
		t.Fatalf("last eval = %v, want %v", st.LastEval, evalAt)
	}
	if got := st.LastKick["scanner"]; !got.Equal(kickAt) {
		t.Fatalf("last kick = %v, want %v", got, kickAt)
	}
	budget := b.gov.GetBudget()
	if budget.WeeklyLimit != 5000 || budget.CurrentSpend != 1200 || !budget.IgnoreAll || len(budget.IgnoredAgents) != 1 {
		t.Fatalf("budget = %+v", budget)
	}
	if got := len(b.gov.KickHistory()); got != 1 {
		t.Fatalf("kick history = %d entries", got)
	}
	if got := cfg.Governor.Modes["BUSY"].Cadences["scanner"]; got != config.Cadence("10m") {
		t.Fatalf("cadence override = %q", got)
	}
	if _, ok := cfg.Governor.Modes["UNKNOWN"]; ok {
		t.Fatal("override for an unconfigured mode must be dropped")
	}
	if cfg.ACMMLevel == nil || *cfg.ACMMLevel != 4 {
		t.Fatal("ACMM level not restored")
	}
	if len(f.savedStates) != 0 || f.savedConfigs != 0 {
		t.Fatal("restore without config_overrides must not write")
	}
}

func TestBootStateWithConfigACMMLevelWins(t *testing.T) {
	cfg := bootStateConfig()
	want := 2
	cfg.ACMMLevel = &want
	b := newBootStateBoot(t, cfg)
	savedLvl := 5
	f := newBootStateFake(&snapshot.PersistedState{ACMMLevel: &savedLvl})

	b.bootStateWith(f.deps)

	if *cfg.ACMMLevel != 2 {
		t.Fatalf("config ACMM level overwritten: %d", *cfg.ACMMLevel)
	}
}

func TestBootStateWithMigratesConfigOverrides(t *testing.T) {
	cfg := bootStateConfig()
	cfg.Governor.Labels.Exempt = []string{"wip"}
	cfg.Governor.Labels.AutoMerge = "ship-it"
	b := newBootStateBoot(t, cfg)
	saved := &snapshot.PersistedState{
		ConfigOverrides: &snapshot.ConfigOverrides{ProjectRepos: []string{"acme/gadgets", "acme/gizmos"}},
	}
	f := newBootStateFake(saved)

	b.bootStateWith(f.deps)

	if got := strings.Join(cfg.Project.Repos, ","); got != "acme/gadgets,acme/gizmos" {
		t.Fatalf("repos = %q", got)
	}
	if got := b.ghClient.AutoMergeLabel(); got != "ship-it" {
		t.Fatalf("auto-merge label not re-applied: %q", got)
	}
	if f.savedConfigs != 1 {
		t.Fatalf("hive.yaml saved %d times, want 1", f.savedConfigs)
	}
	if len(f.savedStates) != 1 || f.savedStates[0] != saved {
		t.Fatalf("snapshot re-saved %d times", len(f.savedStates))
	}
	if saved.ConfigOverrides != nil {
		t.Fatal("config_overrides must be stripped before re-save")
	}
}

func TestBootStateWithMigrationWriteErrorsAreLogged(t *testing.T) {
	b := newBootStateBoot(t, bootStateConfig())
	var log strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&log, nil))
	f := newBootStateFake(&snapshot.PersistedState{ConfigOverrides: &snapshot.ConfigOverrides{}})
	f.saveCfgErr = errors.New("disk full")
	f.saveStateErr = errors.New("still full")

	b.bootStateWith(f.deps)

	for _, want := range []string{"failed to save migrated config", "failed to re-save state after migration"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log missing %q:\n%s", want, log.String())
		}
	}
}

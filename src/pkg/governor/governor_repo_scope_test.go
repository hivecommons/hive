package governor

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func repoScopeTestConfig() config.GovernorConfig {
	return config.GovernorConfig{
		CadenceScope: config.CadenceScopePerRepo,
		Modes: map[string]config.ModeConfig{
			"idle":  {Cadences: map[string]config.Cadence{"scanner": "1h", "supervisor": "1h"}},
			"quiet": {Cadences: map[string]config.Cadence{"scanner": "30m", "supervisor": "30m"}},
			"busy":  {Cadences: map[string]config.Cadence{"scanner": "5m", "supervisor": "5m"}},
			"surge": {Cadences: map[string]config.Cadence{"scanner": "1m", "supervisor": "1m"}},
		},
	}
}

func TestRepoScopedAgentCadencesAreIndependentPerRepo(t *testing.T) {
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	now := base
	g := New(repoScopeTestConfig(), map[string]config.AgentConfig{
		"scanner": {CadenceScope: config.CadenceScopePerRepo},
	}, testLogger())
	g.now = func() time.Time { return now }

	depths := map[string]RepoSnapshot{
		"org/hot":  {Issues: 11},
		"org/cold": {},
	}
	firstDue := g.EvaluateWithRepoDepths(11, 0, 0, 0, depths)
	wantFirst := []string{config.CadenceTargetKey("scanner", "org/hot")}
	if !reflect.DeepEqual(firstDue, wantFirst) {
		t.Fatalf("initial due targets = %v, want most urgent repo target %v", firstDue, wantFirst)
	}
	g.RecordKickForRepo("scanner", "org/hot")

	now = base.Add(30 * time.Minute)
	secondDue := g.EvaluateWithRepoDepths(11, 0, 0, 0, depths)
	wantSecond := []string{config.CadenceTargetKey("scanner", "org/hot")}
	if !reflect.DeepEqual(secondDue, wantSecond) {
		t.Fatalf("due targets after 30m = %v, want only hot repo busy cadence %v", secondDue, wantSecond)
	}

	state := g.GetState()
	if state.Cadences[config.CadenceTargetKey("scanner", "org/hot")].Interval != 5*time.Minute {
		t.Fatalf("hot repo cadence = %v, want busy 5m", state.Cadences[config.CadenceTargetKey("scanner", "org/hot")].Interval)
	}
	if state.Cadences[config.CadenceTargetKey("scanner", "org/cold")].Interval != time.Hour {
		t.Fatalf("cold repo cadence = %v, want idle 1h", state.Cadences[config.CadenceTargetKey("scanner", "org/cold")].Interval)
	}
}

func TestHiveWideAgentKeepsSingleAggregateCadence(t *testing.T) {
	g := New(repoScopeTestConfig(), map[string]config.AgentConfig{
		"supervisor": {},
	}, testLogger())
	g.SetRepoCount(10)

	g.EvaluateWithRepoDepths(25, 0, 0, 0, map[string]RepoSnapshot{
		"org/hot":  {Issues: 25},
		"org/cold": {},
	})
	g.RecordKick("supervisor")

	state := g.GetState()
	if len(state.Cadences) != 1 {
		t.Fatalf("cadence entries = %v, want exactly one aggregate supervisor entry", state.Cadences)
	}
	if _, ok := state.Cadences["supervisor"]; !ok {
		t.Fatalf("aggregate supervisor cadence missing: %v", state.Cadences)
	}
	if _, ok := state.Cadences[config.CadenceTargetKey("supervisor", "org/hot")]; ok {
		t.Fatalf("hive-wide supervisor was silently converted to repo scoped: %v", state.Cadences)
	}
	if len(state.LastKick) != 1 || state.LastKick["supervisor"].IsZero() {
		t.Fatalf("last-kick entries = %v, want exactly one aggregate supervisor timestamp", state.LastKick)
	}
}

func TestRepoScopedAgentNeutralizesThresholdScalingForCadenceMode(t *testing.T) {
	cfg := repoScopeTestConfig()
	cfg.ThresholdScaling = config.ThresholdScalingLinear
	g := New(cfg, map[string]config.AgentConfig{
		"scanner": {CadenceScope: config.CadenceScopePerRepo},
	}, testLogger())
	g.SetRepoCount(10)

	g.EvaluateWithRepoDepths(21, 0, 0, 0, map[string]RepoSnapshot{
		"org/hot": {Issues: 21},
	})

	state := g.GetState()
	key := config.CadenceTargetKey("scanner", "org/hot")
	if state.RepoModes["org/hot"] != ModeSurge {
		t.Fatalf("repo mode = %s, want SURGE from unscaled per-repo threshold", state.RepoModes["org/hot"])
	}
	if state.Cadences[key].Interval != time.Minute {
		t.Fatalf("repo-scoped cadence interval = %v, want surge 1m; aggregate mode was %s", state.Cadences[key].Interval, state.Mode)
	}
}

func TestRepoScopedLastKickMigrationPreservesAggregateAgents(t *testing.T) {
	oldScanner := time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC)
	oldSupervisor := oldScanner.Add(10 * time.Minute)
	g := New(repoScopeTestConfig(), map[string]config.AgentConfig{
		"scanner":    {CadenceScope: config.CadenceScopePerRepo},
		"supervisor": {},
	}, testLogger())
	g.SeedLastKicks(map[string]time.Time{
		"scanner":    oldScanner,
		"supervisor": oldSupervisor,
	})

	g.EvaluateWithRepoDepths(12, 0, 0, 0, map[string]RepoSnapshot{
		"org/alpha": {Issues: 11},
		"org/beta":  {Issues: 1},
	})

	lastKick := g.GetState().LastKick
	cases := []struct {
		key  string
		want time.Time
	}{
		{config.CadenceTargetKey("scanner", "org/alpha"), oldScanner},
		{config.CadenceTargetKey("scanner", "org/beta"), oldScanner},
		{"supervisor", oldSupervisor},
	}
	for _, tc := range cases {
		if got := lastKick[tc.key]; !got.Equal(tc.want) {
			t.Fatalf("lastKick[%q] = %v, want %v (all entries: %v)", tc.key, got, tc.want, lastKick)
		}
	}
	for _, absent := range []string{
		"scanner",
		config.CadenceTargetKey("supervisor", "org/alpha"),
		config.CadenceTargetKey("supervisor", "org/beta"),
	} {
		if _, ok := lastKick[absent]; ok {
			t.Fatalf("lastKick[%q] present after migration; entries = %v", absent, lastKick)
		}
	}
}

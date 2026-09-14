package governor

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestCadenceScopeAggregateMatchesHistoricalMode(t *testing.T) {
	cfg := config.GovernorConfig{CadenceScope: config.CadenceScopeAggregate}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())
	g.SetRepoCount(10)

	g.EvaluateWithRepoDepths(15, 10, 7, 0, map[string]RepoSnapshot{
		"hot":  {Issues: 25},
		"cold": {Issues: 0},
	})

	state := g.GetState()
	if state.Mode != ModeQuiet {
		t.Fatalf("aggregate mode = %s, want QUIET from total depth 25 against scaled quiet threshold 20", state.Mode)
	}
	if len(state.RepoModes) != 0 {
		t.Fatalf("aggregate scope should not populate per-repo modes, got %v", state.RepoModes)
	}
}

func TestCadenceScopePerRepoComputesIndependentModes(t *testing.T) {
	cfg := config.GovernorConfig{CadenceScope: config.CadenceScopePerRepo}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())
	g.SetRepoCount(10)

	g.EvaluateWithRepoDepths(37, 0, 0, 0, map[string]RepoSnapshot{
		"hot":  {Issues: 25},
		"warm": {Issues: 11},
		"cold": {Issues: 1},
		"zero": {},
	})

	got := g.GetState().RepoModes
	want := map[string]Mode{
		"hot":  ModeSurge,
		"warm": ModeBusy,
		"cold": ModeIdle,
		"zero": ModeIdle,
	}
	for repo, mode := range want {
		if got[repo] != mode {
			t.Errorf("%s mode = %s, want %s (all repos sum to the same aggregate queue)", repo, got[repo], mode)
		}
	}
	if g.GetState().Mode != ModeQuiet {
		t.Errorf("hive-wide aggregate mode = %s, want QUIET for non-repo-scoped agents", g.GetState().Mode)
	}
}

func TestCadenceScopePerRepoNeutralizesThresholdScaling(t *testing.T) {
	cfg := config.GovernorConfig{
		CadenceScope:     config.CadenceScopePerRepo,
		ThresholdScaling: config.ThresholdScalingLinear,
	}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())
	g.SetRepoCount(10)

	g.EvaluateWithRepoDepths(21, 0, 0, 0, map[string]RepoSnapshot{
		"hot": {Issues: 21},
	})

	if got := g.GetState().RepoModes["hot"]; got != ModeSurge {
		t.Fatalf("per-repo mode with repoCount=10 and linear scaling = %s, want SURGE from the base surge threshold", got)
	}
	if got := g.thresholdFor("surge"); got != config.DefaultThresholdSurge {
		t.Fatalf("per-repo surge threshold = %d, want unscaled base %d", got, config.DefaultThresholdSurge)
	}
}

func TestRepoDepthsFromActionableSumsToAggregate(t *testing.T) {
	actionable := &ghpkg.ActionableResult{
		Issues: ghpkg.IssueResult{Count: 3, Items: []ghpkg.Issue{
			{Repo: "alpha"},
			{Repo: "alpha"},
			{Repo: "beta"},
		}},
		PRs: ghpkg.PRResult{Count: 2, Items: []ghpkg.PullRequest{
			{Repo: "alpha"},
			{Repo: "gamma"},
		}},
		TotalByRepo: map[string]ghpkg.RepoCounts{
			"alpha": {},
			"beta":  {},
			"gamma": {},
			"zero":  {},
		},
	}

	depths := RepoDepthsFromActionable(actionable)
	total := 0
	for _, depth := range depths {
		total += depth.Issues + depth.PRs
	}
	want := actionable.Issues.Count + actionable.PRs.Count
	if total != want {
		t.Fatalf("per-repo depths sum = %d, want aggregate actionable total %d", total, want)
	}
	if depths["alpha"].Issues != 2 || depths["alpha"].PRs != 1 {
		t.Fatalf("alpha depth = %+v, want 2 issues and 1 PR", depths["alpha"])
	}
}

func TestCadenceScopePerRepoZeroActionableRepoIsIdle(t *testing.T) {
	actionable := &ghpkg.ActionableResult{
		TotalByRepo: map[string]ghpkg.RepoCounts{
			"zero": {Issues: 100, PRs: 50},
		},
	}
	cfg := config.GovernorConfig{CadenceScope: config.CadenceScopePerRepo}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())

	g.EvaluateWithRepoDepths(0, 0, 0, 0, RepoDepthsFromActionable(actionable))

	if got := g.GetState().RepoModes["zero"]; got != ModeIdle {
		t.Fatalf("zero-actionable repo mode = %s, want IDLE despite gross TotalByRepo counts", got)
	}
}

func TestRepoDepthsFromActionableDoesNotInventFailedRepos(t *testing.T) {
	actionable := &ghpkg.ActionableResult{
		TotalByRepo: map[string]ghpkg.RepoCounts{
			"scanned": {},
		},
	}

	depths := RepoDepthsFromActionable(actionable)
	if _, ok := depths["scanned"]; !ok {
		t.Fatal("successfully scanned zero-actionable repo vanished from depths")
	}
	if _, ok := depths["failed"]; ok {
		t.Fatal("repo absent from TotalByRepo was silently invented as idle")
	}
}

func TestCadenceScopePerRepoHeldItemsRemainExcluded(t *testing.T) {
	actionable := &ghpkg.ActionableResult{
		Issues: ghpkg.IssueResult{Count: 1, Items: []ghpkg.Issue{{Repo: "alpha"}}},
		Hold:   ghpkg.HoldResult{Total: 50},
		TotalByRepo: map[string]ghpkg.RepoCounts{
			"alpha": {Issues: 51},
		},
	}
	cfg := config.GovernorConfig{CadenceScope: config.CadenceScopePerRepo}
	g := New(cfg, map[string]config.AgentConfig{}, testLogger())

	g.EvaluateWithRepoDepths(actionable.Issues.Count, actionable.PRs.Count, actionable.Hold.Total, 0, RepoDepthsFromActionable(actionable))

	if got := g.GetState().RepoModes["alpha"]; got != ModeIdle {
		t.Fatalf("held work leaked into per-repo mode: got %s, want IDLE from one actionable item", got)
	}
}

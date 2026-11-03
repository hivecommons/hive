package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestCELPersistedSettingsReloadWithoutReplayingSeenItems(t *testing.T) {
	resetCELCache()
	t.Cleanup(resetCELCache)
	dir := t.TempDir()
	oldRuntime, oldOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile, config.DashboardOverlayFile = filepath.Join(dir, "runtime.yaml"), filepath.Join(dir, "overlay.yaml")
	t.Cleanup(func() { config.RuntimeConfigFile, config.DashboardOverlayFile = oldRuntime, oldOverlay })
	cfg := &config.Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    config.ProjectConfig{Org: "org", Repos: []string{"repo"}},
		Triggers:   []config.TriggerRule{{Name: "first-seen", Expr: `event.kind == "issue.opened" && event.repo == "repo"`, Agent: "reviewer"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{
			"reviewer": {Enabled: true, Backend: "claude"},
			"scanner":  {Enabled: true, Backend: "claude"},
		}},
	}
	gov := newTestGovernor(cfg.Agents)
	var seen celSeenSet
	run := func(number int) []string {
		result := &github.ActionableResult{}
		result.Issues.Items = append(result.Issues.Items, github.Issue{Repo: "repo", Number: number})
		return celMatchedAgentsOnce(celEngineFor(cfg, celTestLogger()), result, cfg, gov, func(string) bool { return false }, celTestLogger(), &seen)
	}
	if got := run(1); !reflect.DeepEqual(got, []string{"reviewer"}) {
		t.Fatalf("initial existing actionable issue: %v", got)
	}
	oldEngine := celEngineFor(cfg, celTestLogger())
	next := cfg.TriggerRulesSnapshot()
	next[0].Agent = "scanner"
	if err := cfg.SaveTriggerRules(config.TriggerRulesVersion(cfg.TriggerRulesSnapshot()), next); err != nil {
		t.Fatal(err)
	}
	if celEngineFor(cfg, celTestLogger()) == oldEngine {
		t.Fatal("next evaluation did not recompile saved settings")
	}
	if got := run(1); len(got) != 0 {
		t.Fatalf("editing rules reset first-seen history: %v", got)
	}
	if got := run(2); !reflect.DeepEqual(got, []string{"scanner"}) {
		t.Fatalf("new actionable issue did not use updated rule: %v", got)
	}
	currentEngine := celEngineFor(cfg, celTestLogger())
	cfg.SourcePath = ""
	if err := cfg.SaveTriggerRules(config.TriggerRulesVersion(next), nil); err == nil {
		t.Fatal("expected persistence failure")
	}
	if celEngineFor(cfg, celTestLogger()) != currentEngine {
		t.Fatal("failed save changed the live compiled engine")
	}
	if got := run(3); !reflect.DeepEqual(got, []string{"scanner"}) {
		t.Fatalf("failed save changed runtime routing: %v", got)
	}
}

package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestTriggerRulesCompareAndSave(t *testing.T) {
	dir := t.TempDir()
	oldRuntime, oldOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile, DashboardOverlayFile = filepath.Join(dir, "runtime.yaml"), filepath.Join(dir, "overlay.yaml")
	t.Cleanup(func() { RuntimeConfigFile, DashboardOverlayFile = oldRuntime, oldOverlay })
	cfg := &Config{
		SourcePath:         filepath.Join(dir, "hive.yaml"),
		Project:            ProjectConfig{Org: "org", Repos: []string{"repo"}},
		AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{"scanner": {Backend: "claude", Enabled: true}}},
	}
	version := TriggerRulesVersion(cfg.TriggerRulesSnapshot())
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- cfg.SaveTriggerRules(version, []TriggerRule{{Name: name, Agent: "scanner", Expr: "true"}})
		}(name)
	}
	wg.Wait()
	close(errs)
	saved, conflicts := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			saved++
		case errors.Is(err, ErrTriggerRulesChanged):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if saved != 1 || conflicts != 1 {
		t.Fatalf("concurrent stale writes: saved=%d conflicts=%d", saved, conflicts)
	}
	before := cfg.TriggerRulesSnapshot()
	snapshot := cfg.TriggerRulesSnapshot()
	snapshot[0].Expr = "false"
	if !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), before) {
		t.Fatal("snapshot aliases live rules")
	}
	cfg.SourcePath = ""
	if err := cfg.SaveTriggerRules(TriggerRulesVersion(before), nil); err == nil {
		t.Fatal("missing source path must fail")
	}
	if !reflect.DeepEqual(cfg.TriggerRulesSnapshot(), before) {
		t.Fatal("failed persistence published tentative rules")
	}
	if TriggerRulesVersion(nil) != TriggerRulesVersion([]TriggerRule{}) {
		t.Fatal("nil and empty rule lists must share a revision")
	}
}

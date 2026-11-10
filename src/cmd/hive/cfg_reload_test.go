package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// A save that ran from the pre-reload config while the reload was parked
// left hive.yaml without the operator's edit; applying the reload must save
// the reloaded config back (hivecommons/hive#17467).
func TestApplyPendingReload_SavesBackAfterStaleSaveWhileParked(t *testing.T) {
	b, log, _ := newLoopBoot(t, 60, 0)
	b.reloadApply = func(newCfg *config.Config) (int, int) {
		*b.cfg = *newCfg
		return 0, 0
	}
	var seq uint64
	b.configSaveSeq = func() uint64 { return seq }
	var saved [][]string
	b.saveConfig = func() error {
		saved = append(saved, append([]string(nil), b.cfg.Project.Repos...))
		return nil
	}

	newCfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
	newCfg.Project.Repos = []string{"reloaded"}

	b.lockCfg()
	b.submitConfigReload(newCfg)
	seq++ // e.g. a heartbeat callback's cfg.Save() from the stale config
	b.unlockCfg()

	if !sameStringSlice(b.cfg.Project.Repos, []string{"reloaded"}) {
		t.Fatalf("repos = %v, want the reloaded config applied", b.cfg.Project.Repos)
	}
	if len(saved) != 1 || !sameStringSlice(saved[0], []string{"reloaded"}) {
		t.Fatalf("saves after reload = %v, want exactly one save of the reloaded config", saved)
	}
	if !strings.Contains(log.String(), "saving the reloaded config back to disk") {
		t.Fatalf("write-back not logged:\n%s", log.String())
	}
}

func TestApplyPendingReload_NoSaveBackWithoutStaleSave(t *testing.T) {
	b, _, _ := newLoopBoot(t, 60, 0)
	b.reloadApply = func(newCfg *config.Config) (int, int) {
		*b.cfg = *newCfg
		return 0, 0
	}
	b.configSaveSeq = func() uint64 { return 7 }
	saves := 0
	b.saveConfig = func() error { saves++; return nil }

	newCfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
	newCfg.Project.Repos = []string{"reloaded"}

	// Deferred and immediate paths both leave disk alone when nothing saved.
	b.lockCfg()
	b.submitConfigReload(newCfg)
	b.unlockCfg()
	b.submitConfigReload(newCfg)

	if saves != 0 {
		t.Fatalf("saves = %d, want 0 when no save ran while the reload was pending", saves)
	}
	if !sameStringSlice(b.cfg.Project.Repos, []string{"reloaded"}) {
		t.Fatalf("repos = %v, want the reloaded config applied", b.cfg.Project.Repos)
	}
}

// parkedSaveSeq belongs to the oldest unapplied reload: a save between two
// parked reloads still counts, since the disk may hold neither edit.
func TestApplyPendingReload_SaveSeqAnchoredToOldestParkedReload(t *testing.T) {
	b, _, _ := newLoopBoot(t, 60, 0)
	b.reloadApply = func(newCfg *config.Config) (int, int) {
		*b.cfg = *newCfg
		return 0, 0
	}
	var seq uint64
	b.configSaveSeq = func() uint64 { return seq }
	saves := 0
	b.saveConfig = func() error { saves++; return nil }

	first := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
	first.Project.Repos = []string{"first"}
	second := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
	second.Project.Repos = []string{"second"}

	b.lockCfg()
	b.submitConfigReload(first)
	seq++
	b.submitConfigReload(second)
	b.unlockCfg()

	if saves != 1 {
		t.Fatalf("saves = %d, want 1", saves)
	}
	if !sameStringSlice(b.cfg.Project.Repos, []string{"second"}) {
		t.Fatalf("repos = %v, want the newest reload applied", b.cfg.Project.Repos)
	}
}

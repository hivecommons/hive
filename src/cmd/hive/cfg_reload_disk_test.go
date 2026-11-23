package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19561: a hand edit to the runtime config file must
// survive the governor tick's persist and the shutdown persist.

func reloadDiskTestYAML(repo string) string {
	return `
project:
  org: testorg
  repos:
    - ` + repo + `
github:
  token: ghp_tok
agents:
  worker:
    backend: claude
    enabled: true
`
}

// newReloadDiskBoot loads a real config file into a loop boot whose
// reloadApply swaps the config, with every PVC layer in a temp dir.
func newReloadDiskBoot(t *testing.T) (*boot, *strings.Builder, string) {
	t.Helper()
	dir := t.TempDir()
	oldRuntime, oldOverlay, oldAgents := config.RuntimeConfigFile, config.DashboardOverlayFile, config.DefaultAgentOverlayDir
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	config.DefaultAgentOverlayDir = filepath.Join(dir, "agent-configs")
	t.Cleanup(func() {
		config.RuntimeConfigFile, config.DashboardOverlayFile, config.DefaultAgentOverlayDir = oldRuntime, oldOverlay, oldAgents
	})
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Cleanup(config.SetSATokenFileForTest(filepath.Join(dir, "no-such-sa-token")))
	// A refused save notifies the external-change observer; keep one left
	// behind by a bootWatchers test from reloading a stale watcher.
	config.SetExternalChangeObserver(nil)
	t.Cleanup(func() { config.SetExternalChangeObserver(nil) })

	path := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(path, []byte(reloadDiskTestYAML("repo-boot")), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadWithDashboardOverlay(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	b, log, _ := newLoopBoot(t, 60, 0)
	b.cfg = cfg
	b.configPath = path
	b.reloadApply = func(newCfg *config.Config) (int, int) {
		*b.cfg = *newCfg
		return 0, 0
	}
	return b, log, path
}

func savePersist(t *testing.T) func(*boot) {
	return func(b *boot) {
		if err := b.cfg.Save(); err != nil {
			t.Errorf("persist save: %v", err)
		}
	}
}

func reposOnDisk(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg.Project.Repos
}

// Edit during a held cfgReloadMu, with no reload delivered (the watcher
// missed it): a stale save without the lock must not clobber it, and the
// tick's persist reloads it and keeps it.
func TestTickPersistKeepsEditMadeWhileCfgLockHeld(t *testing.T) {
	b, log, path := newReloadDiskBoot(t)

	b.lockCfg()
	if err := os.WriteFile(path, []byte(reloadDiskTestYAML("repo-edited")), 0o600); err != nil {
		t.Fatal(err)
	}
	// A saver that runs without cfgReloadMu (heartbeat callback, dashboard).
	if err := b.cfg.Save(); !errors.Is(err, config.ErrConfigChangedOnDisk) {
		t.Fatalf("stale save = %v, want ErrConfigChangedOnDisk", err)
	}
	b.persistCycleLocked(savePersist(t))
	b.unlockCfg()

	if got := b.cfg.Project.Repos; !sameStringSlice(got, []string{"repo-edited"}) {
		t.Fatalf("in-memory repos = %v, want the edit", got)
	}
	if got := reposOnDisk(t, path); !sameStringSlice(got, []string{"repo-edited"}) {
		t.Fatalf("on-disk repos = %v, want the edit kept", got)
	}
	if !strings.Contains(log.String(), "reloading it before the persist") {
		t.Fatalf("reload before persist not logged:\n%s", log.String())
	}
}

// The issue's first scenario: the watcher delivered the reload mid-tick and
// it was parked; it must be applied before the tick's persist.
func TestTickPersistAppliesParkedReloadOfEdit(t *testing.T) {
	b, log, path := newReloadDiskBoot(t)

	b.lockCfg()
	if err := os.WriteFile(path, []byte(reloadDiskTestYAML("repo-edited")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.LoadWithDashboardOverlay(path)
	if err != nil {
		t.Fatal(err)
	}
	b.submitConfigReload(reloaded)
	if !strings.Contains(log.String(), "config reload deferred") {
		t.Fatalf("reload not parked:\n%s", log.String())
	}
	if err := b.cfg.Save(); !errors.Is(err, config.ErrConfigChangedOnDisk) {
		t.Fatalf("stale save while parked = %v, want ErrConfigChangedOnDisk", err)
	}
	b.persistCycleLocked(savePersist(t))
	b.unlockCfg()

	if got := b.cfg.Project.Repos; !sameStringSlice(got, []string{"repo-edited"}) {
		t.Fatalf("in-memory repos = %v, want the parked reload applied", got)
	}
	if got := reposOnDisk(t, path); !sameStringSlice(got, []string{"repo-edited"}) {
		t.Fatalf("on-disk repos = %v, want the edit kept", got)
	}
}

// Edit then graceful shutdown: the shutdown persist keeps the edit.
func TestShutdownPersistKeepsEditedFile(t *testing.T) {
	b, _, path := newReloadDiskBoot(t)
	if err := os.WriteFile(path, []byte(reloadDiskTestYAML("repo-edited")), 0o600); err != nil {
		t.Fatal(err)
	}

	// The run loop's ctx.Done branch.
	b.lockCfg()
	b.persistCycleLocked(savePersist(t))
	b.unlockCfg()

	if got := reposOnDisk(t, path); !sameStringSlice(got, []string{"repo-edited"}) {
		t.Fatalf("on-disk repos = %v, want the edit kept through shutdown", got)
	}
}

// An edit that does not load is left alone by the shutdown persist rather
// than replaced with the in-memory config.
func TestShutdownPersistLeavesUnloadableEditUntouched(t *testing.T) {
	b, log, path := newReloadDiskBoot(t)
	broken := []byte("project: [this is not a config\n")
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}

	b.lockCfg()
	b.persistCycleLocked(func(b *boot) {
		if err := b.cfg.Save(); !errors.Is(err, config.ErrConfigChangedOnDisk) {
			t.Errorf("save = %v, want ErrConfigChangedOnDisk", err)
		}
	})
	b.unlockCfg()

	if got, _ := os.ReadFile(path); !bytes.Equal(got, broken) {
		t.Fatalf("unloadable edit overwritten:\n%s", got)
	}
	if !strings.Contains(log.String(), "does not load") {
		t.Fatalf("unloadable edit not logged:\n%s", log.String())
	}
	if got := b.cfg.Project.Repos; !sameStringSlice(got, []string{"repo-boot"}) {
		t.Fatalf("in-memory repos = %v, want the boot config kept", got)
	}
}

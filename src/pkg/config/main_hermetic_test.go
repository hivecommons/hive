package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain makes the whole package hermetic against live hive state.
//
// The production defaults for the PVC layers all live under /data:
//
//	DefaultAgentOverlayDir  /data/agent-configs        (per-agent overlays)
//	RuntimeConfigFile       /data/hive.yaml.runtime    (PVC runtime layer)
//	DashboardOverlayFile    /data/hive.yaml.dashboard  (dashboard saves)
//
// On a live hive host those paths hold the REAL roster and config. Without
// this redirect, every test that calls Load() with fixture YAML silently
// absorbs the live agent overlays (backend, enabled, clear_on_kick, policies
// all overwritten by the host's roster) and fails — and worse, any test that
// reaches Save() writes to the live dashboard overlay. CI containers have no
// /data, which is the only reason the suite ever passed there.
//
// Individual tests that need a specific path still override these vars
// themselves (with t.Cleanup restoring them to the values set here), which
// remains safe.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "config-hermetic-*")
	if err != nil {
		panic("config TestMain: " + err.Error())
	}
	DefaultAgentOverlayDir = filepath.Join(dir, "agent-configs")
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// breakPVCLayers points RuntimeConfigFile and DashboardOverlayFile at paths
// whose parent directory does not exist, so every PVC-layer write in Save()
// fails. Tests asserting that Save() returns an error when the primary
// config path is unwritable need this: with working PVC layers, Save()
// deliberately returns nil ("state persisted to the PVC layers instead").
func breakPVCLayers(t *testing.T) {
	t.Helper()
	origRuntime, origOverlay := RuntimeConfigFile, DashboardOverlayFile
	missing := filepath.Join(t.TempDir(), "missing-parent")
	RuntimeConfigFile = filepath.Join(missing, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(missing, "hive.yaml.dashboard")
	t.Cleanup(func() { RuntimeConfigFile, DashboardOverlayFile = origRuntime, origOverlay })
}

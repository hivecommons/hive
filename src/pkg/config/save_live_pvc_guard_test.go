package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The production PVC paths must never be written by a test binary: when the
// suite runs inside a hive pod (the hub's PR precheck, an agent running
// go test) /data is real and writable, and a test config would become the
// hive's boot config.
func TestGuardLivePVCPathUnderTest(t *testing.T) {
	if !guardLivePVCPathUnderTest(defaultRuntimeConfigFile, defaultRuntimeConfigFile, "RuntimeConfigFile") {
		t.Fatal("default RuntimeConfigFile must be guarded under go test")
	}
	if !guardLivePVCPathUnderTest(defaultDashboardOverlayFile, defaultDashboardOverlayFile, "DashboardOverlayFile") {
		t.Fatal("default DashboardOverlayFile must be guarded under go test")
	}
	redirected := filepath.Join(t.TempDir(), "hive.yaml.runtime")
	if guardLivePVCPathUnderTest(redirected, defaultRuntimeConfigFile, "RuntimeConfigFile") {
		t.Fatal("a redirected path must not be guarded")
	}
}

// Save with the package vars at their defaults still persists the primary
// source path and reports success, but leaves the PVC layers alone.
func TestSaveSkipsLivePVCLayersUnderTest(t *testing.T) {
	origRuntime, origOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile, DashboardOverlayFile = defaultRuntimeConfigFile, defaultDashboardOverlayFile
	t.Cleanup(func() { RuntimeConfigFile, DashboardOverlayFile = origRuntime, origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	// A sentinel next to the defaults would need /data; instead prove the
	// guard fired by checking the primary path was written and no error
	// surfaced even though the PVC paths are (on CI) unwritable.
	dir := t.TempDir()
	cfg := &Config{
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"testrepo"}, PrimaryRepo: "testrepo"},
		Agents:     map[string]AgentConfig{"scanner": {Backend: "copilot", Model: "m"}},
		SourcePath: filepath.Join(dir, "hive.yaml"),
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(cfg.SourcePath); err != nil {
		t.Fatalf("primary config not written: %v", err)
	}
}

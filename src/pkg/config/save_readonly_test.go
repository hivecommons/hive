package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests pin the #3961 config half: on deployments that mount the config
// read-only (a ConfigMap mounted straight at /etc/hive/hive.yaml), Save()'s
// source-path write can never succeed. The old saveLocked returned at that
// point — skipping the PVC runtime config and the dashboard overlay, the two
// layers that actually survive a pod restart (its own comment said "Continue
// below so the PVC backup and dashboard overlay are still written", but the
// code returned). Every runtime change — pause state, operator model/backend
// ownership, ACMM level — therefore evaporated on every restart while every
// save spammed "failed to persist".

// pointDurableLayersAtTemp redirects the PVC layer paths to a temp dir and
// restores them on cleanup.
func pointDurableLayersAtTemp(t *testing.T) (runtimePath, overlayPath string) {
	t.Helper()
	dir := t.TempDir()
	oldRuntime, oldOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() {
		RuntimeConfigFile, DashboardOverlayFile = oldRuntime, oldOverlay
	})
	return RuntimeConfigFile, DashboardOverlayFile
}

// readOnlySourcePath creates a hive.yaml inside a directory made read-only,
// so both the inode-preserving open and the create fallback fail — exactly
// what a read-only ConfigMap mount produces.
func readOnlySourcePath(t *testing.T) string {
	t.Helper()
	roDir := filepath.Join(t.TempDir(), "etc-hive")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(roDir, "hive.yaml")
	if err := os.WriteFile(src, []byte("# seed\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })
	return src
}

// TestSave_ReadOnlySourcePersistsRuntimeConfig is the restart-survival round
// trip: apply a runtime change with the source path unwritable, then reload
// the PVC runtime config from disk (what the entrypoint boots from in both
// K8s steady state and Docker/LXC) and assert the change is live.
func TestSave_ReadOnlySourcePersistsRuntimeConfig(t *testing.T) {
	runtimePath, _ := pointDurableLayersAtTemp(t)
	src := readOnlySourcePath(t)

	cfg := &Config{
		SourcePath: src,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}

	changed, err := cfg.SetAgentPausedAndSave("scanner", true)
	if !changed {
		t.Fatal("SetAgentPausedAndSave reported no change")
	}
	if err != nil {
		t.Fatalf("Save with read-only source must succeed via the PVC layers, got: %v", err)
	}

	// The read-only seed must be untouched...
	if data, rerr := os.ReadFile(src); rerr != nil || string(data) != "# seed\n" {
		t.Errorf("read-only source changed or unreadable: %q, %v", data, rerr)
	}

	// ...and the runtime config must carry the change across a "restart"
	// (re-read from disk, as the entrypoint's boot path does; unmarshaled
	// directly since a unit-test config has no GitHub credentials to pass
	// full Load validation).
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		t.Fatalf("runtime config not written: %v", err)
	}
	var reloaded struct {
		Agents map[string]struct {
			Paused bool `yaml:"paused"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("re-loading runtime config: %v", err)
	}
	if !reloaded.Agents["scanner"].Paused {
		t.Error("pause state did not survive: runtime config has paused=false after reload (#3961)")
	}
}

// TestSave_ReadOnlySourceKubernetesWritesOverlay: in Kubernetes mode the
// dashboard overlay is a boot input too (first boot / reprovision merge), so
// it must also be written when the source path is unwritable.
func TestSave_ReadOnlySourceKubernetesWritesOverlay(t *testing.T) {
	_, overlayPath := pointDurableLayersAtTemp(t)
	src := readOnlySourcePath(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1") // IsKubernetesPod() → true

	cfg := &Config{
		SourcePath: src,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Model: "sonnet", Enabled: true},
		},
	}
	cfg.Agents["scanner"] = AgentConfig{Backend: "copilot", Model: "gpt-5", Enabled: true}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save with read-only source must succeed via the PVC layers, got: %v", err)
	}

	data, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatalf("dashboard overlay not written: %v", err)
	}
	var overlay struct {
		Agents map[string]struct {
			Backend string `yaml:"backend"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &overlay); err != nil {
		t.Fatalf("overlay unmarshal: %v", err)
	}
	if overlay.Agents["scanner"].Backend != "copilot" {
		t.Errorf("overlay scanner backend = %q, want copilot", overlay.Agents["scanner"].Backend)
	}
}

// TestSave_AllLayersUnwritableStillFails: when the durable layers cannot be
// written either, Save must keep failing loudly — the state genuinely will
// not survive a restart, and reporting success would recreate the silent
// revert this fix removes (just one layer down).
func TestSave_AllLayersUnwritableStillFails(t *testing.T) {
	dir := t.TempDir()
	roPVC := filepath.Join(dir, "data")
	if err := os.MkdirAll(roPVC, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roPVC, 0o755) })

	oldRuntime, oldOverlay := RuntimeConfigFile, DashboardOverlayFile
	RuntimeConfigFile = filepath.Join(roPVC, "hive.yaml.runtime")
	DashboardOverlayFile = filepath.Join(roPVC, "hive.yaml.dashboard")
	t.Cleanup(func() {
		RuntimeConfigFile, DashboardOverlayFile = oldRuntime, oldOverlay
	})

	src := readOnlySourcePath(t)
	cfg := &Config{
		SourcePath: src,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	if err := cfg.Save(); err == nil {
		t.Fatal("Save() = nil with source AND durable layers unwritable — the caller must be told the state will not survive a restart")
	}
}

// TestSave_WritableSourceUnaffected is the positive control: a writable
// source path keeps today's behavior — source written, Save nil.
func TestSave_WritableSourceUnaffected(t *testing.T) {
	pointDurableLayersAtTemp(t)
	src := filepath.Join(t.TempDir(), "hive.yaml")

	cfg := &Config{
		SourcePath: src,
		Project:    ProjectConfig{Org: "testorg", Repos: []string{"repo1"}},
		Agents: map[string]AgentConfig{
			"scanner": {Backend: "claude", Enabled: true},
		},
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil || len(data) == 0 {
		t.Fatalf("source config not written: %v", err)
	}
}

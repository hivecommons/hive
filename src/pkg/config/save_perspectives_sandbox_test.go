package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hivecommons/hive#18797: review.perspectives_source and the agent sandbox
// block are set from the dashboard, so one save must leave /data/hive.yaml.runtime
// and /data/hive.yaml.dashboard carrying the same values. A hand edit of one
// file was reverted or resurrected by the other.
func TestSaveKeepsRuntimeAndDashboardOverlayConsistentForLinkedSettings(t *testing.T) {
	dir := t.TempDir()
	origOverlay, origRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = origOverlay, origRuntime })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	on := true
	cfg := &Config{
		SourcePath: filepath.Join(dir, "hive.yaml"),
		Project:    ProjectConfig{Org: "testorg"},
		AgentRuntimeConfig: AgentRuntimeConfig{
			Agents: map[string]AgentConfig{"reviewer": {Role: "reviewer", Sandbox: &AgentSandboxOverride{
				Enabled: &on, Image: "registry.example/sbx:1", Runtime: SandboxRuntimeJob,
				Job: &SandboxJobConfig{WorkspaceClaim: "hive-data", EnvFromSecrets: []string{"gh-token"}},
			}}},
			AgentSandbox: AgentSandboxConfig{Image: "registry.example/default:1"},
		},
	}
	cfg.Review.PerspectivesSource = &DefinitionSourceConfig{Owner: "acme", Repo: "rules", Path: ".hive/p.yaml", Ref: "main"}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, p := range []string{RuntimeConfigFile, DashboardOverlayFile} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s not written: %v", p, err)
		}
		for _, want := range []string{"perspectives_source:", "repo: rules", "registry.example/sbx:1", "registry.example/default:1", "gh-token", "workspace_claim: hive-data"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s is missing %q", p, want)
			}
		}
	}

	// Clearing the source clears it from both files in the same save.
	cfg.Review.PerspectivesSource = nil
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save after unlink: %v", err)
	}
	for _, p := range []string{RuntimeConfigFile, DashboardOverlayFile} {
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "perspectives_source") {
			t.Errorf("%s still carries perspectives_source after an unlink", p)
		}
	}
}

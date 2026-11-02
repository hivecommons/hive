package config

import "testing"

func TestSnapshotIsolatedAndPreservesRuntimePolicy(t *testing.T) {
	level, hold := 5, false
	cfg := &Config{ACMMLevel: &level, SourcePath: "/example/hive.yaml", ReviewerAutoscaleActive: 3}
	cfg.Project.Repos = []string{"org/repo"}
	cfg.Agents = map[string]AgentConfig{"scanner": {Enabled: true}}
	cfg.GitHub.selfAuthorizationHoldEnvOverride = &hold
	cfg.GitHub.reporterTrustHoldEnvOverride = &hold
	snapshot, err := cfg.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Project.Repos[0] = "changed/repo"
	cfg.Agents["scanner"] = AgentConfig{Enabled: false}
	level, hold = 1, true
	if snapshot.Project.Repos[0] != "org/repo" || !snapshot.Agents["scanner"].Enabled || *snapshot.ACMMLevel != 5 {
		t.Fatal("snapshot aliases live configuration")
	}
	if *snapshot.GitHub.selfAuthorizationHoldEnvOverride || *snapshot.GitHub.reporterTrustHoldEnvOverride {
		t.Fatal("runtime policy override lost or aliased")
	}
	if snapshot.SourcePath != cfg.SourcePath || snapshot.ReviewerAutoscaleActive != 3 {
		t.Fatal("runtime fields lost")
	}
}

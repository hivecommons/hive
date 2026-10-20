package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestContributeHonourTargetBranchEnabled(t *testing.T) {
	if !(HubConfig{}).ContributeHonourTargetBranchEnabled() {
		t.Fatal("unset must default to honouring the declared target branch")
	}
	on, off := true, false
	if !(HubConfig{ContributeHonourTargetBranch: &on}).ContributeHonourTargetBranchEnabled() {
		t.Fatal("explicit true must enable")
	}
	if (HubConfig{ContributeHonourTargetBranch: &off}).ContributeHonourTargetBranchEnabled() {
		t.Fatal("explicit false must disable")
	}
	var cfg Config
	if err := yaml.Unmarshal([]byte("hub:\n  contribute_honour_target_branch: false\n"), &cfg); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if cfg.Hub.ContributeHonourTargetBranchEnabled() {
		t.Fatal("yaml false must disable")
	}
}

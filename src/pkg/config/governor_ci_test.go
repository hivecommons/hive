package config

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGovernorCIWorkflowApprovalConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
		enabled    bool
		actors     []string
	}{
		{"defaults", "{}", true, []string{"dependabot[bot]", "renovate[bot]"}},
		{"disabled", "governor:\n  ci:\n    auto_approve_workflows: false\n    auto_approve_workflow_actors: []", false, []string{}},
		{"custom", "governor:\n  ci:\n    auto_approve_workflows: true\n    auto_approve_workflow_actors: [trusted-bot]", true, []string{"trusted-bot"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Governor.CI.AutoApproveWorkflowsEnabled() != tc.enabled || !reflect.DeepEqual(cfg.Governor.CI.WorkflowApprovalActors(), tc.actors) {
				t.Fatalf("CI config=%+v actors=%v", cfg.Governor.CI, cfg.Governor.CI.WorkflowApprovalActors())
			}
			// Saving configuration must preserve an explicitly empty trust
			// list rather than restoring the default bots after a reload.
			for _, codec := range []struct {
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{{yaml.Marshal, yaml.Unmarshal}, {json.Marshal, json.Unmarshal}} {
				data, err := codec.marshal(cfg.Governor.CI)
				if err != nil {
					t.Fatal(err)
				}
				var restored GovernorCIConfig
				if err := codec.unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				if restored.AutoApproveWorkflowsEnabled() != tc.enabled || !reflect.DeepEqual(restored.WorkflowApprovalActors(), tc.actors) {
					t.Fatalf("round trip changed trust: %s -> %+v", data, restored)
				}
			}
		})
	}
}

package defsrc

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestApplyToConfig_DiscoveredKeepsOwnerControlledFields: an agent approved
// from a repo's .hive/agents/ (hivecommons/hive#18491) re-applies its
// presentation from the repo, but backend, model, mode and tools stay what the
// owner set, even though the live definition names other values.
func TestApplyToConfig_DiscoveredKeepsOwnerControlledFields(t *testing.T) {
	f := &stubFetcher{body: validDef + "  tools:\n    preset: full\n"}
	r := NewResolver(f, allowAll, nil)
	ownerTools := &config.ToolsConfig{Preset: "readonly"}
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		Agents: map[string]config.AgentConfig{
			"scanner": {
				DisplayName: "Baked Scanner",
				Backend:     "copilot",
				Model:       "gpt-5",
				Mode:        "ADVISORY",
				Tools:       ownerTools,
				DefinitionSource: &config.DefinitionSourceConfig{
					Owner: "hivecommons", Repo: "hive", Path: ".hive/agents/scanner.yaml", Discovered: true,
				},
			},
			"linked": {
				Backend:          "copilot",
				DefinitionSource: &config.DefinitionSourceConfig{Owner: "hivecommons", Repo: "hive", Path: "agents/linked.yaml"},
			},
		},
	}}

	ApplyToConfig(context.Background(), cfg, r, nil)

	sc := cfg.Agents["scanner"]
	if sc.DisplayName != "Live Scanner" {
		t.Errorf("presentation not re-applied: %q", sc.DisplayName)
	}
	if sc.Backend != "copilot" || sc.Model != "gpt-5" || sc.Mode != "ADVISORY" {
		t.Errorf("owner-controlled fields changed: backend=%q model=%q mode=%q", sc.Backend, sc.Model, sc.Mode)
	}
	if sc.Tools != ownerTools {
		t.Errorf("tools changed: %+v", sc.Tools)
	}
	if !sc.DefinitionSource.Discovered {
		t.Error("discovered marker lost")
	}

	// A keep-linked import is unchanged: its definition still sets backend.
	if got := cfg.Agents["linked"].Backend; got != "claude" {
		t.Errorf("keep-linked backend = %q, want claude from the definition", got)
	}
}

func TestKeepOwnerControlledFields(t *testing.T) {
	tools := &config.ToolsConfig{Preset: "readonly"}
	merged := config.AgentConfig{Backend: "claude", Model: "opus", Mode: "ISSUES_ONLY", DisplayName: "New"}
	owner := config.AgentConfig{Backend: "copilot", Model: "gpt-5", Mode: "ADVISORY", Tools: tools, DisplayName: "Old"}
	got := KeepOwnerControlledFields(merged, owner)
	if got.Backend != "copilot" || got.Model != "gpt-5" || got.Mode != "ADVISORY" || got.Tools != tools {
		t.Errorf("owner fields not kept: %+v", got)
	}
	if got.DisplayName != "New" {
		t.Errorf("non-owner field overwritten: %q", got.DisplayName)
	}
}

package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestApplyPackClearsPackModelOnGatewayAgent covers #9945: an agent moved onto
// a model gateway kept the pack's copilot model (`claude-sonnet-4-6`), which
// outranks the gateway's default_model and 403s as not entitled. The pack
// apply that runs on every restart re-seeded it even after the operator
// deleted the `model:` line. It must clear a pack-owned model instead.
func TestApplyPackClearsPackModelOnGatewayAgent(t *testing.T) {
	tests := []struct {
		name     string
		backend  string
		gateways []config.GatewayConfig
	}{
		{"litellm backend", "litellm", nil},
		{"named gateway backend", "cb", []config.GatewayConfig{{
			Name: "cb", Kind: config.GatewayKindLiteLLM, Endpoint: "https://litellm.example.test", DefaultModel: "rits/zai-org/glm-5-3",
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFullServer(t)
			enableScanner(t, srv)
			srv.deps.Config.Governor.Gateways = tt.gateways
			sc := srv.deps.Config.Agents["scanner"]
			sc.Backend = tt.backend
			sc.BackendOwner = config.FieldOwnerOperator
			sc.Model = "claude-sonnet-4-6"
			sc.ModelOwner = config.FieldOwnerPack
			srv.deps.Config.Agents["scanner"] = sc

			// Twice: the second apply must not re-seed the pack model.
			for i := 0; i < 2; i++ {
				if _, err := srv.ApplyPack(3); err != nil {
					t.Fatalf("ApplyPack(3) #%d: %v", i, err)
				}
				got := srv.deps.Config.Agents["scanner"]
				if got.Backend != tt.backend {
					t.Fatalf("apply #%d: backend = %q, want %q", i, got.Backend, tt.backend)
				}
				if got.Model != "" {
					t.Errorf("apply #%d: model = %q, want \"\" so the gateway default_model applies", i, got.Model)
				}
			}
		})
	}
}

// TestApplyPackKeepsOperatorModelOnGatewayAgent: a model the operator picked
// for a gateway agent is theirs; the pack apply never clears it.
func TestApplyPackKeepsOperatorModelOnGatewayAgent(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	sc := srv.deps.Config.Agents["scanner"]
	sc.Backend = "litellm"
	sc.BackendOwner = config.FieldOwnerOperator
	sc.Model = "rits/zai-org/glm-5-3"
	sc.ModelOwner = config.FieldOwnerOperator
	srv.deps.Config.Agents["scanner"] = sc

	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	if got := srv.deps.Config.Agents["scanner"].Model; got != "rits/zai-org/glm-5-3" {
		t.Errorf("operator gateway model = %q, want rits/zai-org/glm-5-3", got)
	}
}

// TestApplyPackSeedsModelOnPackBackend: an agent still on the pack's own
// backend keeps receiving the pack model.
func TestApplyPackSeedsModelOnPackBackend(t *testing.T) {
	srv := newFullServer(t)
	enableScanner(t, srv)
	sc := srv.deps.Config.Agents["scanner"]
	sc.Backend = "copilot"
	sc.BackendOwner = config.FieldOwnerPack
	sc.Model = ""
	sc.ModelOwner = config.FieldOwnerPack
	srv.deps.Config.Agents["scanner"] = sc
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	got := srv.deps.Config.Agents["scanner"]
	if got.Backend != "copilot" || got.Model != "claude-sonnet-4-6" {
		t.Errorf("scanner on pack backend: backend=%q model=%q, want copilot with the pack model claude-sonnet-4-6", got.Backend, got.Model)
	}
}

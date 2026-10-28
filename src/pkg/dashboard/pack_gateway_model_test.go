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

// TestApplyPackModelOnKeptOrReplacedBackend covers #13288: a hand-set CLI
// backend that is kept must not receive the pack's model id.
func TestApplyPackModelOnKeptOrReplacedBackend(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		model     string
		wantModel string
	}{
		{"kept codex keeps its model", "codex", "gpt-5-codex", "gpt-5-codex"},
		{"kept codex without model is not seeded", "codex", "", ""},
		{"pack backend is seeded", "copilot", "", "claude-sonnet-4-6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newFullServer(t)
			enableScanner(t, srv)
			sc := srv.deps.Config.Agents["scanner"]
			sc.Backend = tt.backend
			sc.BackendOwner = config.FieldOwnerOperator
			sc.Model = tt.model
			sc.ModelOwner = config.FieldOwnerPack
			srv.deps.Config.Agents["scanner"] = sc
			if _, err := srv.ApplyPack(3); err != nil {
				t.Fatalf("ApplyPack(3): %v", err)
			}
			got := srv.deps.Config.Agents["scanner"]
			if got.Backend != tt.backend || got.Model != tt.wantModel {
				t.Errorf("backend=%q model=%q, want %q / %q", got.Backend, got.Model, tt.backend, tt.wantModel)
			}
		})
	}
}

// gatewayHive is the #14678 shape: an L2 hive whose operator moved the agents
// it already had onto one inference gateway before the pack grew `reviewer`.
func gatewayHive(t *testing.T, backends ...string) *Server {
	t.Helper()
	srv := newFullServer(t)
	for i, name := range []string{"scanner", "quality", "guide"} {
		srv.deps.Config.Agents[name] = config.AgentConfig{
			ID: name, Role: name, Enabled: true,
			Backend: backends[i%len(backends)], BackendOwner: config.FieldOwnerOperator,
		}
	}
	return srv
}

// TestApplyPackCreatesAgentOnHiveGateway covers #14678: the L2 `reviewer`,
// added to a hive whose agents all run on a LiteLLM gateway, was created on
// the pack's copilot backend with a copilot model id.
func TestApplyPackCreatesAgentOnHiveGateway(t *testing.T) {
	srv := gatewayHive(t, "litellm")
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	got, ok := srv.deps.Config.Agents["reviewer"]
	if !ok {
		t.Fatal("L2 pack did not create `reviewer`")
	}
	if got.Backend != "litellm" || got.Model != "" {
		t.Errorf("created reviewer backend=%q model=%q, want litellm with no model so the gateway default_model applies", got.Backend, got.Model)
	}
	if got.BackendIsOperatorOwned() {
		t.Error("created reviewer backend is operator-owned; the operator never picked it")
	}
}

// TestApplyPackMovesPackOwnedAgentOntoHiveGateway: a hive that already got
// the copilot reviewer before the fix is repaired on its next restart.
func TestApplyPackMovesPackOwnedAgentOntoHiveGateway(t *testing.T) {
	srv := gatewayHive(t, "litellm")
	srv.deps.Config.Agents["reviewer"] = config.AgentConfig{
		ID: "reviewer", Role: "reviewer", Enabled: true,
		Backend: "copilot", BackendOwner: config.FieldOwnerPack,
		Model: "claude-fable-5", ModelOwner: config.FieldOwnerPack,
	}
	for i := 0; i < 2; i++ {
		if _, err := srv.ApplyPack(2); err != nil {
			t.Fatalf("ApplyPack(2) #%d: %v", i, err)
		}
		got := srv.deps.Config.Agents["reviewer"]
		if got.Backend != "litellm" || got.Model != "" {
			t.Errorf("apply #%d: reviewer backend=%q model=%q, want litellm with no model", i, got.Backend, got.Model)
		}
	}
}

// TestApplyPackKeepsPackBackendWhenGatewayNotShared: the pack backend stays
// whenever "the hive's gateway" is not a single, unambiguous choice, and an
// operator's own copilot pick is never moved.
func TestApplyPackKeepsPackBackendWhenGatewayNotShared(t *testing.T) {
	tests := []struct {
		name     string
		backends []string
		reviewer *config.AgentConfig
	}{
		{"agents disagree on the gateway", []string{"litellm", "vllm"}, nil},
		{"no agent on a gateway", []string{"claude"}, nil},
		{"one agent on a gateway, the rest on a CLI", []string{"litellm", "copilot"}, nil},
		{"operator picked copilot", []string{"litellm"}, &config.AgentConfig{
			ID: "reviewer", Role: "reviewer", Enabled: true,
			Backend: "copilot", BackendOwner: config.FieldOwnerOperator,
		}},
		{"legacy unowned copilot", []string{"litellm"}, &config.AgentConfig{
			ID: "reviewer", Role: "reviewer", Enabled: true, Backend: "copilot",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := gatewayHive(t, tt.backends...)
			if tt.reviewer != nil {
				srv.deps.Config.Agents["reviewer"] = *tt.reviewer
			}
			if _, err := srv.ApplyPack(2); err != nil {
				t.Fatalf("ApplyPack(2): %v", err)
			}
			if got := srv.deps.Config.Agents["reviewer"].Backend; got != "copilot" {
				t.Errorf("reviewer backend = %q, want the pack's copilot", got)
			}
		})
	}
}

// TestApplyPackDoesNotMigrateFleetForOneGatewayAgent: an operator who moved a
// single agent onto a gateway has not moved the hive, so the pack-owned CLI
// agents stay where the pack put them (#14678 review).
func TestApplyPackDoesNotMigrateFleetForOneGatewayAgent(t *testing.T) {
	srv := newFullServer(t)
	for _, name := range []string{"scanner", "quality"} {
		srv.deps.Config.Agents[name] = config.AgentConfig{
			ID: name, Role: name, Enabled: true,
			Backend: "copilot", BackendOwner: config.FieldOwnerPack,
			Model: "claude-fable-5", ModelOwner: config.FieldOwnerPack,
		}
	}
	srv.deps.Config.Agents["guide"] = config.AgentConfig{
		ID: "guide", Role: "guide", Enabled: true,
		Backend: "litellm", BackendOwner: config.FieldOwnerOperator,
	}
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	for _, name := range []string{"scanner", "quality", "reviewer"} {
		if got := srv.deps.Config.Agents[name].Backend; got != "copilot" {
			t.Errorf("%s backend = %q, want the pack's copilot: one gateway agent is not the hive's gateway", name, got)
		}
	}
}

// packCLIAgent is an agent the pack created on its own copilot CLI, as the L2
// pack does for supervisor/brainstorm (and did for reviewer before #14678).
func packCLIAgent(name string) config.AgentConfig {
	return config.AgentConfig{
		ID: name, Role: name, Enabled: true,
		Backend: "copilot", BackendOwner: config.FieldOwnerPack,
		Model: "claude-sonnet-4-6", ModelOwner: config.FieldOwnerPack,
	}
}

// TestApplyPackGatewayWithPreexistingPackCLIAgents is the real L2 roster
// behind #14678: supervisor and brainstorm already exist pack-owned on
// copilot when reviewer arrives, and the operator moved only
// scanner/quality/guide. Pack-owned CLI placements do not veto the move, so
// reviewer is created on the gateway and the two older ones follow it — in
// one apply, even though each sees the other still on copilot.
func TestApplyPackGatewayWithPreexistingPackCLIAgents(t *testing.T) {
	tests := []struct {
		name        string
		preexisting []string
	}{
		{"reviewer created", []string{"supervisor", "brainstorm"}},
		{"reviewer repaired alongside two others", []string{"supervisor", "brainstorm", "reviewer"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := gatewayHive(t, "litellm")
			for _, name := range tt.preexisting {
				srv.deps.Config.Agents[name] = packCLIAgent(name)
			}
			for i := 0; i < 2; i++ {
				if _, err := srv.ApplyPack(2); err != nil {
					t.Fatalf("ApplyPack(2) #%d: %v", i, err)
				}
				for _, name := range []string{"supervisor", "brainstorm", "reviewer"} {
					got, ok := srv.deps.Config.Agents[name]
					if !ok {
						t.Fatalf("apply #%d: L2 pack did not create %s", i, name)
					}
					if got.Backend != "litellm" || got.Model != "" {
						t.Errorf("apply #%d: %s backend=%q model=%q, want litellm with no model", i, name, got.Backend, got.Model)
					}
				}
			}
		})
	}
}

// TestApplyPackKeepsPackBackendWhenOperatorPicksAreMinority: three operator
// gateway agents that are at most half of the other enabled agents is not
// "the hive moved", so the pack's CLI placements stay. Four pack CLI agents
// so that every pack agent, which does not count itself, still sees three
// operator picks among six others.
func TestApplyPackKeepsPackBackendWhenOperatorPicksAreMinority(t *testing.T) {
	srv := gatewayHive(t, "litellm")
	packCLI := []string{"supervisor", "brainstorm", "ci-maintainer", "triage"}
	for _, name := range packCLI {
		srv.deps.Config.Agents[name] = packCLIAgent(name)
	}
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	for _, name := range append(packCLI, "reviewer") {
		if got := srv.deps.Config.Agents[name].Backend; got != "copilot" {
			t.Errorf("%s backend = %q, want the pack's copilot: operator picks are not a majority", name, got)
		}
	}
}

// TestApplyPackGatewayHoldsAcrossLevels: the pack agents L2 moved onto the
// gateway are pack-owned, so they must count toward the majority when L3
// places `ci-maintainer` — otherwise three operator picks among six other
// agents is no longer a majority and #14678 reproduces one level up.
func TestApplyPackGatewayHoldsAcrossLevels(t *testing.T) {
	srv := gatewayHive(t, "litellm")
	if _, err := srv.ApplyPack(2); err != nil {
		t.Fatalf("ApplyPack(2): %v", err)
	}
	for _, name := range []string{"supervisor", "brainstorm", "reviewer"} {
		if got := srv.deps.Config.Agents[name]; got.Backend != "litellm" || got.BackendIsOperatorOwned() {
			t.Fatalf("after L2: %s backend=%q operator-owned=%v, want pack-owned litellm", name, got.Backend, got.BackendIsOperatorOwned())
		}
	}
	if _, err := srv.ApplyPack(3); err != nil {
		t.Fatalf("ApplyPack(3): %v", err)
	}
	got, ok := srv.deps.Config.Agents["ci-maintainer"]
	if !ok {
		t.Fatal("L3 pack did not create `ci-maintainer`")
	}
	if got.Backend != "litellm" || got.Model != "" {
		t.Errorf("ci-maintainer backend=%q model=%q, want litellm with no model: the agents L2 moved onto the gateway count toward the hive's gateway", got.Backend, got.Model)
	}
}

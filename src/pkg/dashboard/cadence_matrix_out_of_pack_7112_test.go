package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// #7112: #7110 stopped the sidebar/card filter hiding a running agent outside
// the ACMM pack, but the Governor cadence matrix still re-derived the RAW pack
// roster (acmmPackAgents / buildACMMPackAgents) with no "active outside pack"
// escape hatch, so a running out-of-pack agent's cadence row was still dropped.
//
// The client fix makes renderCadenceMatrix follow status.agents — already gated
// server-side WITH the exception by buildAgentsWithHidden — instead of the raw
// roster. This Go test pins the SERVER-SIDE invariant the JS filter now relies
// on: the set the matrix follows (status.agents) contains every running/enabled
// out-of-pack agent, and a genuinely pack-inactive "ghost" agent is absent from
// it (so the matrix still suppresses the ghost by construction).
//
// matrixVisible models exactly what renderCadenceMatrix computes: the cadence
// rows (buildCadenceMatrix) intersected with status.agents.
func TestCadenceMatrixFollowsStatusAgents_7112(t *testing.T) {
	level := 5
	supervisorCfg := config.AgentConfig{Backend: "unsloth-local", Enabled: true}
	reviewCfg := config.AgentConfig{Backend: "unsloth-local", Enabled: true, DisplayName: "Review"}
	// linter is a custom agent outside every pack roster AND disabled — a
	// genuine pack-inactive "ghost". It is config-only (no runtime entry), so
	// buildAgentsWithHidden records it as hidden/disabled and keeps it OUT of
	// status.agents, while buildCadenceMatrix still emits a row for it (it
	// iterates cfg.Agents). The matrix must therefore drop it.
	linterCfg := config.AgentConfig{Backend: "unsloth-local", Enabled: false, DisplayName: "Linter"}
	cfg := &config.Config{
		ACMMLevel: &level,
		Agents: map[string]config.AgentConfig{
			"supervisor": supervisorCfg,
			"review":     reviewCfg,
			"linter":     linterCfg,
		},
		Governor: config.GovernorConfig{
			Gateways: []config.GatewayConfig{{
				Name:     "unsloth-local",
				Kind:     config.GatewayKindCustom,
				Endpoint: "http://host.docker.internal:8888",
			}},
		},
	}
	statuses := map[string]*agent.AgentProcess{
		"supervisor": {
			Name:         "supervisor",
			Config:       supervisorCfg,
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
		"review": {
			Name:         "review",
			Config:       reviewCfg,
			State:        agent.StateRunning,
			OutputBuffer: agent.NewRingBuffer(10),
		},
	}

	govState := governor.State{Mode: governor.ModeIdle}
	agents, _ := buildAgentsWithHidden(statuses, cfg, govState)
	statusAgentSet := map[string]bool{}
	for _, a := range agents {
		statusAgentSet[a.Name] = true
	}

	// review is running outside every pack roster — it MUST be in status.agents
	// (the escape hatch). If activeOutsidePack ever stops surfacing it, this is
	// the first thing to fail, and the matrix loses its row with it.
	if !statusAgentSet["review"] {
		t.Fatalf("running out-of-pack agent 'review' must be in status.agents; got %v", agentNamesFromFrontend(agents))
	}
	// linter is out-of-pack AND disabled: a ghost. It must NOT be surfaced.
	if statusAgentSet["linter"] {
		t.Fatalf("pack-inactive 'linter' must be hidden from status.agents; got %v", agentNamesFromFrontend(agents))
	}

	matrix := buildCadenceMatrix(cfg, statuses, "idle")
	matrixVisible := map[string]bool{}
	for _, row := range matrix {
		if statusAgentSet[row.Agent] {
			matrixVisible[row.Agent] = true
		}
	}

	// Invariant 1 — the matrix's filter source never omits a status.agents
	// member: every surfaced agent that has a cadence row keeps that row.
	matrixRowSet := map[string]bool{}
	for _, row := range matrix {
		matrixRowSet[row.Agent] = true
	}
	for name := range statusAgentSet {
		if matrixRowSet[name] && !matrixVisible[name] {
			t.Fatalf("agent %q is in status.agents and has a cadence row but was dropped from the matrix", name)
		}
	}

	// Invariant 2 — running out-of-pack agent keeps its cadence row.
	if !matrixVisible["review"] {
		t.Fatalf("out-of-pack running agent 'review' must keep its cadence-matrix row; visible=%v", keys(matrixVisible))
	}
	// Invariant 3 — the pack-inactive ghost is still suppressed.
	if matrixVisible["linter"] {
		t.Fatalf("pack-inactive ghost 'linter' must NOT get a cadence-matrix row; visible=%v", keys(matrixVisible))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

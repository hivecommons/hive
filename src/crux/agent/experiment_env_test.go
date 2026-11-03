package agent

import (
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// TestExperimentEnvPairsOnlyForExperimenter pins RFC 11585 part 5's
// containment rule: the engine/dispatcher/root variables reach an agent with
// the experimenter role and no one else, so the strategist launches exactly
// as before.
func TestExperimentEnvPairsOnlyForExperimenter(t *testing.T) {
	hiveEnv := map[string]string{
		"HIVE_NOUS_DATA_DIR":     "/data/nous",
		config.NousDispatcherEnv: config.NousDispatcherSDK,
		config.NousEngineEnv:     "ignored-hive-level-engine",
	}
	m := &Manager{logger: discardLogger(), project: ProjectContext{ExperimentEnv: hiveEnv}}

	collect := func(agent *AgentProcess) map[string]string {
		got := map[string]string{}
		for _, p := range m.agentEnvPairs(agent) {
			switch p.Key {
			case config.NousEngineEnv, config.NousDispatcherEnv, "HIVE_NOUS_DATA_DIR":
				got[p.Key] = p.Value
			}
		}
		return got
	}

	for _, role := range []string{"strategist", "scanner", ""} {
		if got := collect(&AgentProcess{Name: "strategist", Config: config.AgentConfig{Role: role}}); len(got) != 0 {
			t.Fatalf("role %q received experiment env: %v", role, got)
		}
	}

	got := collect(&AgentProcess{Name: "lab", Config: config.AgentConfig{Role: config.RoleExperimenter}})
	want := map[string]string{
		config.NousEngineEnv:     config.DefaultExperimentEngine,
		config.NousDispatcherEnv: config.NousDispatcherSDK,
		"HIVE_NOUS_DATA_DIR":     "/data/nous",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	// The agent's own engine wins over anything hive-level; ordering is
	// deterministic (engine first, then sorted hive keys).
	pairs := m.experimentEnvPairs(&AgentProcess{Config: config.AgentConfig{Role: config.RoleExperimenter, Engine: "nous"}})
	if len(pairs) != 3 || pairs[0].Key != config.NousEngineEnv || pairs[0].Value != "nous" ||
		pairs[1].Key != "HIVE_NOUS_DATA_DIR" || pairs[2].Key != config.NousDispatcherEnv {
		t.Fatalf("pairs = %+v", pairs)
	}

	// No hive-level env at all still exports the engine name.
	m.project.ExperimentEnv = nil
	pairs = m.experimentEnvPairs(&AgentProcess{Config: config.AgentConfig{Role: config.RoleExperimenter}})
	if len(pairs) != 1 || pairs[0].Value != config.DefaultExperimentEngine {
		t.Fatalf("pairs without hive env = %+v", pairs)
	}
	if m.experimentEnvPairs(nil) != nil {
		t.Fatal("nil agent must export nothing")
	}
}

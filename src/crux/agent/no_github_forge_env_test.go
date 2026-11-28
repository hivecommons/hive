package agent

import (
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// hivecommons/hive#20222: on a hive whose repos are all off GitHub the agent
// gets HIVE_NO_GITHUB_FORGE, so the gh wrapper names the forge tools instead of
// reporting a token-delivery fault. A GitHub hive exports nothing.
func TestAgentEnvPairs_NoGitHubForge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		forge   func() string
		want    string
		present bool
	}{
		{name: "nil resolver", forge: nil},
		{name: "github hive", forge: func() string { return "" }},
		{name: "forgejo-only hive", forge: func() string { return "forgejo" }, want: "forgejo", present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pausedProject()
			p.NonGitHubForge = tc.forge
			m := NewManager(map[string]config.AgentConfig{
				"reviewer": {Backend: "claude", Model: "claude-sonnet-5"},
			}, discardLogger(), p)
			m.mu.RLock()
			agent := m.agents["reviewer"]
			m.mu.RUnlock()
			if agent == nil {
				t.Fatal("fixture agent not registered")
			}
			env := map[string]string{}
			for _, pair := range m.agentEnvPairs(agent) {
				env[pair.Key] = pair.Value
			}
			got, ok := env["HIVE_NO_GITHUB_FORGE"]
			if ok != tc.present || got != tc.want {
				t.Errorf("HIVE_NO_GITHUB_FORGE = %q (present=%v), want %q (present=%v)", got, ok, tc.want, tc.present)
			}
		})
	}
}

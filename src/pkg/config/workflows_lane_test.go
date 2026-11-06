package config

import "testing"

// Danathar/arch-bootc#634, Danathar/exult#374: ci-maintainer (ISSUES_AND_PRS,
// contributor-tier token, no Workflows permission) parked approved workflow
// fixes for the owner while scanner (ISSUES_PRS_MERGE, trusted tier) was
// landing workflow changes in the same repos.
func TestWorkflowsLaneFor(t *testing.T) {
	fleet := func(agents map[string]AgentConfig) *Config {
		return &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: agents}}
	}
	live := fleet(map[string]AgentConfig{
		"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
		"quality":       {Mode: "ISSUES_AND_PRS", Enabled: true},
		"reviewer":      {Mode: "ADVISORY", Enabled: true},
		"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true},
		"scanner-2":     {Mode: "ISSUES_PRS_MERGE", Enabled: true, ReplicaOf: "scanner"},
	})
	for _, tc := range []struct {
		name  string
		cfg   *Config
		agent string
		repo  string
		want  string
	}{
		{name: "contributor-tier lane hands to scanner", cfg: live, agent: "ci-maintainer", want: "scanner"},
		{name: "another contributor-tier lane too", cfg: live, agent: "quality", want: "scanner"},
		{name: "scanner pushes workflows itself", cfg: live, agent: "scanner", want: ""},
		{name: "a replica of scanner pushes them too", cfg: live, agent: "scanner-2", want: ""},
		{name: "no capable lane: the owner applies it", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"scanner":       {Mode: "ISSUES_AND_PRS", Enabled: true},
		}), agent: "ci-maintainer", want: ""},
		{name: "a disabled merge lane does not count", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"scanner":       {Mode: "ISSUES_PRS_MERGE"},
		}), agent: "ci-maintainer", want: ""},
		{name: "tools preset outranks the mode field", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_PRS_MERGE", Enabled: true, Tools: &ToolsConfig{Preset: "issues-prs"}},
			"merger":        {Mode: "ISSUES_AND_PRS", Enabled: true, Tools: &ToolsConfig{Preset: "full"}},
		}), agent: "ci-maintainer", want: "merger"},
		{name: "first capable lane by name", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"zeta":          {Mode: "ISSUES_PRS_MERGE", Enabled: true},
			"alpha":         {Mode: "ISSUES_PRS_MERGE", Enabled: true},
		}), agent: "ci-maintainer", want: "alpha"},
		{name: "nil config", cfg: nil, agent: "ci-maintainer", want: ""},
		// Adjudicator on hivecommons/hive#16692: a merge lane scoped to other
		// repositories is refused on this one, so it is not chosen here and
		// the next one that serves it is.
		{name: "a merge lane scoped elsewhere is skipped", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"alpha":         {Mode: "ISSUES_PRS_MERGE", Enabled: true, Repos: []string{"other"}},
			"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true},
		}), agent: "ci-maintainer", repo: "o/arch-bootc", want: "scanner"},
		{name: "the scoped lane is chosen in its own repo", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"alpha":         {Mode: "ISSUES_PRS_MERGE", Enabled: true, Repos: []string{"o/arch-bootc"}},
			"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true},
		}), agent: "ci-maintainer", repo: "o/arch-bootc", want: "alpha"},
		{name: "no merge lane serves the repo: the owner applies it", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true, Repos: []string{"o/other"}},
		}), agent: "ci-maintainer", repo: "o/arch-bootc", want: ""},
		{name: "no repo: scope is not asked", cfg: fleet(map[string]AgentConfig{
			"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
			"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true, Repos: []string{"o/other"}},
		}), agent: "ci-maintainer", want: "scanner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.WorkflowsLaneFor(tc.agent, tc.repo); got != tc.want {
				t.Fatalf("WorkflowsLaneFor(%q, %q) = %q, want %q", tc.agent, tc.repo, got, tc.want)
			}
		})
	}
}

func TestAgentPushesWorkflows(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"scanner":       {Mode: "ISSUES_PRS_MERGE", Enabled: true},
		"scanner-2":     {Mode: "ISSUES_PRS_MERGE", Enabled: true, ReplicaOf: "scanner"},
		"ci-maintainer": {Mode: "ISSUES_AND_PRS", Enabled: true},
	}}}
	for agent, want := range map[string]bool{"scanner": true, "scanner-2": true, "ci-maintainer": false, "unknown": false, "": false} {
		if got := cfg.AgentPushesWorkflows(agent); got != want {
			t.Errorf("AgentPushesWorkflows(%q) = %v, want %v", agent, got, want)
		}
	}
	if (*Config)(nil).AgentPushesWorkflows("scanner") {
		t.Error("nil config must answer false")
	}
}

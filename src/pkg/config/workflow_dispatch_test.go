package config

import "testing"

func TestWorkflowDispatchAgent(t *testing.T) {
	off := false
	base := func() *Config {
		c := &Config{}
		c.Agents = map[string]AgentConfig{
			"scanner":   {Enabled: true, Role: "scanner"},
			"zed":       {Enabled: true, Role: "feature"},
			"alpha":     {Enabled: true, Role: "feature"},
			"alpha-2":   {Enabled: true, Role: "feature", ReplicaOf: "alpha"},
			"scribe":    {Enabled: false, Role: "writer"},
			"architect": {Enabled: true},
		}
		return c
	}
	cases := []struct {
		name    string
		mut     func(c *Config)
		profile string
		want    string
		ok      bool
	}{
		{"role fallback picks the first feature agent by name", nil, "techdocs-drafter", "alpha", true},
		{"a disabled writer is skipped", nil, "techdocs-drafter", "alpha", true},
		{"enabled writer outranks feature", func(c *Config) { c.Agents["scribe"] = AgentConfig{Enabled: true, Role: "writer"} }, "techdocs-drafter", "scribe", true},
		{"agent named after the profile", func(c *Config) { c.Agents["techdocs-drafter"] = AgentConfig{Enabled: true} }, "techdocs-drafter", "techdocs-drafter", true},
		{"configured agent wins", func(c *Config) {
			c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "architect"}}
		}, "techdocs-drafter", "architect", true},
		{"configured typo never falls back", func(c *Config) {
			c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "nobody"}}
		}, "techdocs-drafter", "nobody", false},
		{"disabled profile", func(c *Config) {
			c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-verifier": {Disabled: true}}
		}, "techdocs-verifier", "", false},
		{"no agent fills it", func(c *Config) {
			c.Agents = map[string]AgentConfig{"scanner": {Enabled: true, Role: "scanner"}}
		}, "techdocs-drafter", "", false},
		{"empty profile", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			if tc.mut != nil {
				tc.mut(c)
			}
			got, ok := c.WorkflowDispatchAgent(tc.profile)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("WorkflowDispatchAgent(%q) = %q %v; want %q %v", tc.profile, got, ok, tc.want, tc.ok)
			}
		})
	}
	var nilCfg *Config
	if _, ok := nilCfg.WorkflowDispatchAgent("x"); ok {
		t.Fatal("nil config resolved an agent")
	}
	if !(WorkflowDispatchConfig{}).DispatchEnabled() {
		t.Fatal("dispatch must default on")
	}
	if (WorkflowDispatchConfig{Enabled: &off}).DispatchEnabled() {
		t.Fatal("enabled: false must turn dispatch off")
	}
}

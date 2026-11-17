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
		{"the issue worker is the last resort", func(c *Config) {
			c.Agents = map[string]AgentConfig{"scanner": {Enabled: true, Role: "scanner"}}
		}, "techdocs-drafter", "scanner", true},
		{"no agent fills it", func(c *Config) {
			c.Agents = map[string]AgentConfig{"supervisor": {Enabled: true}, "guide": {Enabled: true}, "brainstorm": {Enabled: true}}
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
	disabled := base()
	disabled.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-verifier": {Disabled: true}}
	if got, ok := disabled.WorkflowDispatchAgentForManual("techdocs-verifier"); !ok || got != "alpha" {
		t.Fatalf("manual send to a disabled profile = %q %v; want alpha", got, ok)
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

// standardFleet is the roster a default hive runs: no writer, docs or
// feature agent (hivecommons/hive#18690).
func standardFleet() *Config {
	c := &Config{}
	c.Project.Org = "cncf"
	c.Agents = map[string]AgentConfig{}
	for _, name := range []string{"brainstorm", "ci-maintainer", "guide", "quality", "reviewer", "scanner", "supervisor"} {
		c.Agents[name] = AgentConfig{Enabled: true}
	}
	return c
}

// The resolution order: pin, agent named after the profile, writer / docs /
// feature, then the last resort (the issue worker for a profile that opens a
// pull request, the reviewer for one that reviews), which must serve the
// bound repository. Nothing else is ever picked.
func TestResolveWorkflowDispatchFallbackOrder(t *testing.T) {
	drafter := WorkflowDispatchNeed{Repo: "cncf/techdocs"}
	verifier := WorkflowDispatchNeed{Repo: "cncf/techdocs", Reviews: true}
	cases := []struct {
		name       string
		mut        func(c *Config)
		profile    string
		need       WorkflowDispatchNeed
		wantAgent  string
		wantSource string
	}{
		{"standard fleet: the drafter falls back to the scanner", nil, "techdocs-drafter", drafter, "scanner", WorkflowDispatchSourceFallback},
		{"standard fleet: the verifier falls back to the reviewer", nil, "techdocs-verifier", verifier, "reviewer", WorkflowDispatchSourceFallback},
		{"a pin outranks everything", func(c *Config) {
			c.Agents["techdocs-drafter"] = AgentConfig{Enabled: true}
			c.Agents["writer"] = AgentConfig{Enabled: true}
			c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "quality"}}
		}, "techdocs-drafter", drafter, "quality", WorkflowDispatchSourcePinned},
		{"an agent named after the profile outranks the roles", func(c *Config) {
			c.Agents["techdocs-drafter"] = AgentConfig{Enabled: true}
			c.Agents["writer"] = AgentConfig{Enabled: true}
		}, "techdocs-drafter", drafter, "techdocs-drafter", WorkflowDispatchSourceNamed},
		{"writer outranks the last resort", func(c *Config) { c.Agents["writer"] = AgentConfig{Enabled: true} }, "techdocs-drafter", drafter, "writer", WorkflowDispatchSourceRole},
		{"docs outranks feature", func(c *Config) {
			c.Agents["featurework"] = AgentConfig{Enabled: true, Role: "feature"}
			c.Agents["scribe"] = AgentConfig{Enabled: true, Role: "docs"}
		}, "techdocs-drafter", drafter, "scribe", WorkflowDispatchSourceRole},
		{"feature outranks the last resort, for the verifier too", func(c *Config) {
			c.Agents["featurework"] = AgentConfig{Enabled: true, Role: "feature"}
		}, "techdocs-verifier", verifier, "featurework", WorkflowDispatchSourceRole},
		{"the last resort goes by role, not name", func(c *Config) {
			delete(c.Agents, "scanner")
			c.Agents["fixer"] = AgentConfig{Enabled: true, Role: "scanner"}
		}, "techdocs-drafter", drafter, "fixer", WorkflowDispatchSourceFallback},
		{"a scanner scoped to another repo is passed over for one that serves this repo", func(c *Config) {
			c.Agents["scanner"] = AgentConfig{Enabled: true, Repos: []string{"cncf/website"}}
			c.Agents["techdocs-scanner"] = AgentConfig{Enabled: true, Role: "scanner", Repos: []string{"techdocs"}}
		}, "techdocs-drafter", drafter, "techdocs-scanner", WorkflowDispatchSourceFallback},
		{"a scanner that does not serve the repo is not picked", func(c *Config) {
			c.Agents["scanner"] = AgentConfig{Enabled: true, Repos: []string{"cncf/website"}}
		}, "techdocs-drafter", drafter, "", ""},
		{"a disabled scanner is not picked", func(c *Config) { c.Agents["scanner"] = AgentConfig{Enabled: false} }, "techdocs-drafter", drafter, "", ""},
		{"a scanner replica is not picked", func(c *Config) {
			delete(c.Agents, "scanner")
			c.Agents["scanner-2"] = AgentConfig{Enabled: true, Role: "scanner", ReplicaOf: "scanner"}
		}, "techdocs-drafter", drafter, "", ""},
		{"the drafter never falls back to the reviewer", func(c *Config) { delete(c.Agents, "scanner") }, "techdocs-drafter", drafter, "", ""},
		{"the verifier never falls back to the scanner", func(c *Config) { delete(c.Agents, "reviewer") }, "techdocs-verifier", verifier, "", ""},
		{"supervisor, guide and brainstorm are never picked", func(c *Config) {
			for _, name := range []string{"ci-maintainer", "quality", "reviewer", "scanner"} {
				delete(c.Agents, name)
			}
		}, "techdocs-drafter", drafter, "", ""},
		{"a pin that names no enabled agent never falls back", func(c *Config) {
			c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "nobody"}}
		}, "techdocs-drafter", drafter, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := standardFleet()
			if tc.mut != nil {
				tc.mut(c)
			}
			got := c.ResolveWorkflowDispatch(tc.profile, tc.need, false)
			if got.Agent != tc.wantAgent || got.Source != tc.wantSource {
				t.Fatalf("ResolveWorkflowDispatch(%q) = %+v; want agent %q source %q", tc.profile, got, tc.wantAgent, tc.wantSource)
			}
			if got.OK() != (tc.wantAgent != "") {
				t.Fatalf("OK() = %v for %+v", got.OK(), got)
			}
			if !got.OK() && got.Problem == "" {
				t.Fatalf("nothing fills the profile but no reason is given: %+v", got)
			}
			if got.OK() && got.Problem != "" {
				t.Fatalf("a filled profile carries a problem: %+v", got)
			}
		})
	}
}

// The reason names the profile and what is missing, so the dashboard can
// show it as is.
func TestResolveWorkflowDispatchProblem(t *testing.T) {
	c := standardFleet()
	delete(c.Agents, "scanner")
	got := c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs"}, false)
	if want := "no agent fills techdocs-drafter: this hive has no enabled techdocs-drafter, writer, docs, feature or scanner agent"; got.Problem != want {
		t.Fatalf("problem = %q; want %q", got.Problem, want)
	}
	c = standardFleet()
	c.Agents["scanner"] = AgentConfig{Enabled: true, Repos: []string{"cncf/website"}}
	got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs"}, false)
	if want := "no agent fills techdocs-drafter: scanner does not work on cncf/techdocs"; got.Problem != want {
		t.Fatalf("scoped problem = %q; want %q", got.Problem, want)
	}
	c = standardFleet()
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "nobody"}}
	got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{}, false)
	if want := `the pinned agent "nobody" is not an enabled agent of this hive`; got.Problem != want || got.Pinned != "nobody" {
		t.Fatalf("pin problem = %+v; want %q", got, want)
	}
	// A profile turned off for automatic dispatch is a choice, not a gap;
	// the owner's manual send still resolves it.
	c = standardFleet()
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Disabled: true}}
	if got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{}, false); got.OK() || !got.Disabled || got.Problem != "" {
		t.Fatalf("disabled profile = %+v", got)
	}
	if got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{}, true); got.Agent != "scanner" {
		t.Fatalf("manual send to a disabled profile = %+v; want scanner", got)
	}
}

// The dashboard's picker saves workflow.dispatch.profiles.<profile>.agent:
// the pin is validated, keeps the profile's other settings, and clearing it
// drops an entry that holds nothing else.
func TestSetWorkflowDispatchAgent(t *testing.T) {
	c := standardFleet()
	if _, err := c.SetWorkflowDispatchAgent("", "scanner"); err == nil {
		t.Fatal("an empty profile was accepted")
	}
	for _, bad := range []string{"nobody", "scanner-2"} {
		c.Agents["scanner-2"] = AgentConfig{Enabled: true, ReplicaOf: "scanner"}
		if _, err := c.SetWorkflowDispatchAgent("techdocs-drafter", bad); err == nil {
			t.Fatalf("%q was accepted as a pin", bad)
		}
	}
	if c.Workflow.Dispatch.Profiles != nil {
		t.Fatalf("a refused pin wrote the config: %+v", c.Workflow.Dispatch.Profiles)
	}
	changed, err := c.SetWorkflowDispatchAgent(" techdocs-drafter ", " quality ")
	if err != nil || !changed || c.Workflow.Dispatch.Profiles["techdocs-drafter"].Agent != "quality" {
		t.Fatalf("pin: %v %v %+v", changed, err, c.Workflow.Dispatch.Profiles)
	}
	if got := c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{}, false); got.Agent != "quality" || got.Source != WorkflowDispatchSourcePinned {
		t.Fatalf("the pin does not resolve: %+v", got)
	}
	if changed, err = c.SetWorkflowDispatchAgent("techdocs-drafter", "quality"); err != nil || changed {
		t.Fatalf("the same pin again: %v %v", changed, err)
	}
	if changed, err = c.SetWorkflowDispatchAgent("techdocs-drafter", ""); err != nil || !changed || c.Workflow.Dispatch.Profiles != nil {
		t.Fatalf("unpin: %v %v %+v", changed, err, c.Workflow.Dispatch.Profiles)
	}
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-verifier": {Agent: "reviewer", PromptTemplate: "verify ${REF}", Disabled: true}}
	if changed, err = c.SetWorkflowDispatchAgent("techdocs-verifier", ""); err != nil || !changed {
		t.Fatalf("unpin with other settings: %v %v", changed, err)
	}
	if got := c.Workflow.Dispatch.Profiles["techdocs-verifier"]; got.Agent != "" || got.PromptTemplate != "verify ${REF}" || !got.Disabled {
		t.Fatalf("unpin dropped the profile's other settings: %+v", got)
	}
	var nilCfg *Config
	if _, err := nilCfg.SetWorkflowDispatchAgent("p", "a"); err == nil {
		t.Fatal("nil config accepted a pin")
	}
	if got := nilCfg.WorkflowDispatchAgentChoices(); got != nil {
		t.Fatalf("nil config choices = %v", got)
	}
	if got := standardFleet().WorkflowDispatchAgentChoices(); len(got) != 7 || got[0] != "brainstorm" || got[6] != "supervisor" {
		t.Fatalf("choices = %v", got)
	}
}

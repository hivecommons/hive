package config

import (
	"strings"
	"testing"
)

// workingLevel is an ACMM level at which every agent but the supervisor
// defaults to ISSUES_AND_PRS, so a test roster may open pull requests
// unless it says otherwise.
func workingLevel() *int {
	level := 5
	return &level
}

// advisory is an enabled agent that may only read and advise.
var advisory = AgentConfig{Enabled: true, Mode: "ADVISORY"}

func TestWorkflowDispatchAgent(t *testing.T) {
	off := false
	base := func() *Config {
		c := &Config{ACMMLevel: workingLevel()}
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
			c.Agents = map[string]AgentConfig{"supervisor": {Enabled: true}, "guide": advisory, "brainstorm": advisory}
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
// feature agent (hivecommons/hive#18690). At workingLevel the working
// agents may open pull requests; brainstorm and guide are advisory here and
// the supervisor always is.
func standardFleet() *Config {
	c := &Config{ACMMLevel: workingLevel()}
	c.Project.Org = "cncf"
	c.Agents = map[string]AgentConfig{}
	for _, name := range []string{"ci-maintainer", "quality", "reviewer", "scanner", "supervisor"} {
		c.Agents[name] = AgentConfig{Enabled: true}
	}
	c.Agents["brainstorm"], c.Agents["guide"] = advisory, advisory
	return c
}

// The resolution order: pin, agent named after the profile, writer / docs /
// feature, then the last resort (the issue worker for a profile that opens a
// pull request, the reviewer for one that reviews, else the first agent
// that may do the work), which must serve the bound repository. An agent
// that may not do what the profile needs is never picked.
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
		{"a scanner that does not serve the repo is passed over for the first agent that may open pull requests", func(c *Config) {
			c.Agents["scanner"] = AgentConfig{Enabled: true, Repos: []string{"cncf/website"}}
		}, "techdocs-drafter", drafter, "ci-maintainer", WorkflowDispatchSourceFallback},
		{"a disabled scanner is not picked", func(c *Config) { c.Agents["scanner"] = AgentConfig{Enabled: false} }, "techdocs-drafter", drafter, "ci-maintainer", WorkflowDispatchSourceFallback},
		{"a scanner replica is not picked", func(c *Config) {
			delete(c.Agents, "scanner")
			c.Agents["scanner-2"] = AgentConfig{Enabled: true, Role: "scanner", ReplicaOf: "scanner"}
		}, "techdocs-drafter", drafter, "ci-maintainer", WorkflowDispatchSourceFallback},
		{"without a scanner the drafter goes to the first agent that may open pull requests", func(c *Config) { delete(c.Agents, "scanner") }, "techdocs-drafter", drafter, "ci-maintainer", WorkflowDispatchSourceFallback},
		{"without a reviewer the verifier goes to the first agent that may comment", func(c *Config) { delete(c.Agents, "reviewer") }, "techdocs-verifier", verifier, "ci-maintainer", WorkflowDispatchSourceFallback},
		{"advisory agents are never picked", func(c *Config) {
			for _, name := range []string{"ci-maintainer", "quality", "reviewer", "scanner"} {
				delete(c.Agents, name)
			}
		}, "techdocs-drafter", drafter, "", ""},
		// hivecommons/hive#18749: the scanner is advisory and one agent may
		// open pull requests, so that agent is the drafter.
		{"an advisory scanner is passed over for the agent that may open pull requests", func(c *Config) {
			for _, name := range []string{"ci-maintainer", "reviewer", "scanner"} {
				c.Agents[name] = advisory
			}
			c.Agents["quality"] = AgentConfig{Enabled: true, Mode: "ISSUES_AND_PRS"}
		}, "techdocs-drafter", drafter, "quality", WorkflowDispatchSourceFallback},
		{"an advisory reviewer with the converse capability may be the verifier", func(c *Config) {
			talks := true
			c.Agents["reviewer"] = AgentConfig{Enabled: true, Mode: "ADVISORY", Converse: &talks}
		}, "techdocs-verifier", verifier, "reviewer", WorkflowDispatchSourceFallback},
		{"an advisory agent named after the profile is passed over", func(c *Config) {
			c.Agents["techdocs-drafter"] = advisory
		}, "techdocs-drafter", drafter, "scanner", WorkflowDispatchSourceFallback},
		{"an advisory writer is passed over", func(c *Config) {
			c.Agents["writer"] = advisory
		}, "techdocs-drafter", drafter, "scanner", WorkflowDispatchSourceFallback},
		{"an agent the hand-off already went to is not picked again", func(c *Config) {}, "techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs", Exclude: []string{"scanner"}}, "ci-maintainer", WorkflowDispatchSourceFallback},
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
	c.Agents = map[string]AgentConfig{}
	got := c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs"}, false)
	if want := "no agent fills techdocs-drafter: this hive has no enabled agent"; got.Problem != want {
		t.Fatalf("problem = %q; want %q", got.Problem, want)
	}
	c = standardFleet()
	c.Agents = map[string]AgentConfig{"scanner": {Enabled: true, Repos: []string{"cncf/website"}}, "guide": advisory}
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

// advisoryFleet is the roster of hivecommons/hive#18749: every agent runs
// ADVISORY except quality, the one agent that may open pull requests.
func advisoryFleet() *Config {
	c := &Config{ACMMLevel: workingLevel()}
	c.Project.Org = "cncf"
	c.Agents = map[string]AgentConfig{}
	for _, name := range []string{"brainstorm", "ci-maintainer", "guide", "reviewer", "scanner", "supervisor"} {
		c.Agents[name] = advisory
	}
	c.Agents["quality"] = AgentConfig{Enabled: true, Mode: "ISSUES_AND_PRS"}
	return c
}

// A profile resolves only to an agent whose mode lets it do what the
// profile needs: on a hive whose scanner is advisory the drafter is the one
// agent that may open pull requests, and its mode rides along for the
// prompt (hivecommons/hive#18749).
func TestResolveWorkflowDispatchRequiresCapability(t *testing.T) {
	drafter := WorkflowDispatchNeed{Repo: "cncf/techdocs"}
	verifier := WorkflowDispatchNeed{Repo: "cncf/techdocs", Reviews: true}
	c := advisoryFleet()
	got := c.ResolveWorkflowDispatch("techdocs-drafter", drafter, false)
	if got.Agent != "quality" || got.Source != WorkflowDispatchSourceFallback || got.Mode != "ISSUES_AND_PRS" || got.Problem != "" {
		t.Fatalf("drafter on a mixed-mode hive = %+v; want quality", got)
	}
	if got.Needs != "open pull requests in cncf/techdocs" {
		t.Fatalf("needs = %q", got.Needs)
	}
	// The verifier needs comment rights only; the advisory reviewer has
	// none, so it goes to the agent that does.
	if got = c.ResolveWorkflowDispatch("techdocs-verifier", verifier, false); got.Agent != "quality" || got.Needs != "comment in cncf/techdocs" {
		t.Fatalf("verifier on a mixed-mode hive = %+v; want quality", got)
	}

	// No agent may write: the profile is unfilled and the reason says so.
	c.Agents["quality"] = advisory
	got = c.ResolveWorkflowDispatch("techdocs-drafter", drafter, false)
	if want := "no agent on this hive may open pull requests in cncf/techdocs; agents are advisory-only"; got.OK() || got.Problem != want {
		t.Fatalf("advisory-only hive = %+v; want problem %q", got, want)
	}
	if got = c.ResolveWorkflowDispatch("techdocs-verifier", verifier, false); got.OK() || got.Problem != "no agent on this hive may comment in cncf/techdocs; agents are advisory-only" {
		t.Fatalf("advisory-only verifier = %+v", got)
	}
	if got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{}, true); got.OK() || got.Problem != "no agent on this hive may open pull requests in the bound repository; agents are advisory-only" {
		t.Fatalf("advisory-only hive, repository unknown = %+v", got)
	}
	// Issue-only agents are not advisory, and still may not open one.
	c.Agents["scanner"] = AgentConfig{Enabled: true, Mode: "ISSUES_ONLY"}
	if got = c.ResolveWorkflowDispatch("techdocs-drafter", drafter, false); got.OK() || got.Problem != "no agent on this hive may open pull requests in cncf/techdocs; no agent's mode allows it" {
		t.Fatalf("issues-only hive = %+v", got)
	}
	// ... but it may comment, so it can be the verifier.
	if got = c.ResolveWorkflowDispatch("techdocs-verifier", verifier, false); got.Agent != "scanner" {
		t.Fatalf("issues-only verifier = %+v; want scanner", got)
	}
}

// A pin that names an agent without the capability is a visible problem,
// never a silent pick and never a fallback to another agent.
func TestResolveWorkflowDispatchPinWithoutCapability(t *testing.T) {
	drafter := WorkflowDispatchNeed{Repo: "cncf/techdocs"}
	c := advisoryFleet()
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "scanner"}}
	got := c.ResolveWorkflowDispatch("techdocs-drafter", drafter, false)
	if want := `the pinned agent "scanner" may not open pull requests in cncf/techdocs (mode ADVISORY)`; got.OK() || got.Pinned != "scanner" || got.Problem != want {
		t.Fatalf("advisory pin = %+v; want problem %q", got, want)
	}
	if agent, ok := c.WorkflowDispatchAgentForManual("techdocs-drafter"); ok || agent != "scanner" {
		t.Fatalf("manual send to an advisory pin = %q %v", agent, ok)
	}
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-verifier": {Agent: "reviewer"}}
	got = c.ResolveWorkflowDispatch("techdocs-verifier", WorkflowDispatchNeed{Repo: "cncf/techdocs", Reviews: true}, false)
	if want := `the pinned agent "reviewer" may not comment in cncf/techdocs (mode ADVISORY, no converse capability)`; got.Problem != want {
		t.Fatalf("advisory verifier pin = %+v; want problem %q", got, want)
	}
	// A pinned agent the hand-off already went to is not replaced.
	c.Workflow.Dispatch.Profiles = map[string]WorkflowDispatchProfile{"techdocs-drafter": {Agent: "quality"}}
	got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs", Exclude: []string{"quality"}}, false)
	if got.OK() || !strings.Contains(got.Problem, `the pinned agent "quality" did not start the hand-off`) {
		t.Fatalf("excluded pin = %+v", got)
	}
	// With nothing pinned and the only capable agent already tried, the
	// reason names it.
	c.Workflow.Dispatch.Profiles = nil
	got = c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs", Exclude: []string{"quality"}}, false)
	if got.OK() || got.Problem != "no agent other than quality may open pull requests in cncf/techdocs" {
		t.Fatalf("every capable agent tried = %+v", got)
	}
}

// The mode a session runs in is the agent's configured mode, else the
// default for its name at the hive's ACMM level; the repository gate is the
// pull-request relay's; and the picker's candidates say who qualifies.
func TestWorkflowDispatchCapabilityChecks(t *testing.T) {
	c := advisoryFleet()
	if got := c.AgentInteractionMode("quality").String(); got != "ISSUES_AND_PRS" {
		t.Fatalf("configured mode = %s", got)
	}
	c.Agents["fresh"] = AgentConfig{Enabled: true}
	if got := c.AgentInteractionMode("fresh").String(); got != "ISSUES_AND_PRS" {
		t.Fatalf("default mode at the working level = %s", got)
	}
	c.Agents["odd"] = AgentConfig{Enabled: true, Mode: "nonsense"}
	if got := c.AgentInteractionMode("odd").String(); got != "ISSUES_AND_PRS" {
		t.Fatalf("an unparseable mode must fall back to the default, got %s", got)
	}
	var nilCfg *Config
	if got := nilCfg.AgentInteractionMode("x").String(); got != "ADVISORY" {
		t.Fatalf("nil config mode = %s", got)
	}
	if ok, why := nilCfg.WorkflowDispatchAgentQualifies("x", WorkflowDispatchNeed{}); ok || why == "" {
		t.Fatalf("nil config qualifies = %v %q", ok, why)
	}
	if ok, why := c.WorkflowDispatchAgentQualifies("nobody", WorkflowDispatchNeed{}); ok || why != "is not an enabled agent of this hive" {
		t.Fatalf("unknown agent qualifies = %v %q", ok, why)
	}
	if ok, why := c.WorkflowDispatchAgentQualifies("scanner", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); ok || why != "may not open pull requests in cncf/techdocs (mode ADVISORY)" {
		t.Fatalf("advisory scanner qualifies = %v %q", ok, why)
	}
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); !ok || why != "" {
		t.Fatalf("quality qualifies = %v %q", ok, why)
	}

	// The relay's repository gate: a hive below the level at which the
	// agent's name may push refuses the pull request whatever its mode.
	if err := c.PRRepoPolicyRefusal("quality", "cncf/techdocs"); err != nil {
		t.Fatalf("repository gate at the working level: %v", err)
	}
	low := 2
	c.ACMMLevel = &low
	if err := c.PRRepoPolicyRefusal("quality", "cncf/techdocs"); err == nil || !strings.Contains(err.Error(), "effective ACMM L2 does not allow quality to open PRs") {
		t.Fatalf("repository gate at L2 = %v", err)
	}
	if ok, why := c.WorkflowDispatchAgentQualifies("quality", WorkflowDispatchNeed{Repo: "cncf/techdocs"}); ok || !strings.Contains(why, "the repository's ACMM level does not allow it") {
		t.Fatalf("quality below the repository gate = %v %q", ok, why)
	}
	got := c.ResolveWorkflowDispatch("techdocs-drafter", WorkflowDispatchNeed{Repo: "cncf/techdocs"}, false)
	if got.OK() || got.Problem != "no agent on this hive may open pull requests in cncf/techdocs; no agent's mode allows it" {
		t.Fatalf("drafter below the repository gate = %+v", got)
	}
	c.ACMMLevel = workingLevel()

	cands := c.WorkflowDispatchCandidates(WorkflowDispatchNeed{Repo: "cncf/techdocs"})
	byName := map[string]WorkflowDispatchAgentChoice{}
	for _, cand := range cands {
		byName[cand.Agent] = cand
	}
	if len(cands) != len(c.WorkflowDispatchAgentChoices()) {
		t.Fatalf("candidates = %+v", cands)
	}
	if q := byName["quality"]; !q.Qualifies || q.Reason != "" || q.Mode != "ISSUES_AND_PRS" {
		t.Fatalf("quality candidate = %+v", q)
	}
	if s := byName["scanner"]; s.Qualifies || s.Mode != "ADVISORY" || !strings.Contains(s.Reason, "may not open pull requests") {
		t.Fatalf("scanner candidate = %+v", s)
	}
	if got := nilCfg.WorkflowDispatchCandidates(WorkflowDispatchNeed{}); len(got) != 0 {
		t.Fatalf("nil config candidates = %v", got)
	}
}

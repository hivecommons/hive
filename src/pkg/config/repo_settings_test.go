package config

import (
	"errors"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAgentsMdMaxBytesFor(t *testing.T) {
	c := &Config{
		Project: ProjectConfig{Org: "acme", AgentsMdMaxBytes: 16384},
		Repos: map[string]RepoSettings{
			"widgets":      {AgentsMdMaxBytes: 65536},
			"Other/Gadget": {AgentsMdMaxBytes: 4096},
			"inherits":     {},
		},
	}
	cases := map[string]int{
		"widgets":      65536, // bare key, bare lookup
		"acme/widgets": 65536, // bare key, qualified lookup
		"ACME/Widgets": 65536, // case-folded
		"other/gadget": 4096,  // qualified key
		"inherits":     16384, // entry without the knob inherits project
		"unlisted":     16384, // no entry inherits project
	}
	for repo, want := range cases {
		if got := c.AgentsMdMaxBytesFor(repo); got != want {
			t.Errorf("AgentsMdMaxBytesFor(%q) = %d, want %d", repo, got, want)
		}
	}
	if got := c.AgentsMdMaxBytesFor(""); got != 16384 {
		t.Errorf("blank repo = %d, want project value", got)
	}
	var nilCfg *Config
	if got := nilCfg.AgentsMdMaxBytesFor("widgets"); got != 0 {
		t.Errorf("nil config = %d, want 0", got)
	}
	if _, ok := (&Config{}).RepoSettingsFor("widgets"); ok {
		t.Error("empty repos map must report no entry")
	}
}

func TestReposAgentsMdMaxBytesYAML(t *testing.T) {
	var c Config
	src := "project:\n  org: acme\n  repos: [widgets]\nrepos:\n  widgets:\n    agents_md_max_bytes: 49152\n"
	if err := yaml.Unmarshal([]byte(src), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.AgentsMdMaxBytesFor("widgets"); got != 49152 {
		t.Errorf("yaml per-repo cap = %d, want 49152", got)
	}
}

func TestParentAutoCloseFor(t *testing.T) {
	var cfg Config
	if err := yaml.Unmarshal([]byte("project: {org: acme}\nrepos:\n  off: {parent_autoclose: false}\n  epics: {parent_autoclose: epics_too}\n  trackers: {parent_autoclose: Trackers_Too}\n  on: {parent_autoclose: true}\n  junk: {parent_autoclose: maybe}\n  other: {}\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"off": ParentAutoCloseOff, "acme/epics": ParentAutoCloseEpicsToo, "trackers": ParentAutoCloseTrackersToo, "on": ParentAutoCloseOn,
		"junk": ParentAutoCloseOn, "other": ParentAutoCloseOn, "unlisted": ParentAutoCloseOn,
	}
	for repo, want := range cases {
		if got := cfg.ParentAutoCloseFor(repo); got != want {
			t.Errorf("%s = %q, want %q", repo, got, want)
		}
	}
}

func TestSetParentAutoClose(t *testing.T) {
	newCfg := func() *Config {
		return &Config{Project: ProjectConfig{Org: "acme", Repos: []string{"widgets", "acme/gadgets"}}}
	}
	t.Run("sets and reads back live", func(t *testing.T) {
		c := newCfg()
		if err := c.SetParentAutoClose("acme/Widgets", " Epics_Too "); err != nil {
			t.Fatal(err)
		}
		if got := c.ParentAutoCloseFor("widgets"); got != ParentAutoCloseEpicsToo {
			t.Errorf("effective = %q, want epics_too", got)
		}
		if !c.ParentAutoCloseExplicit("widgets") {
			t.Error("explicit = false after a set")
		}
		if _, ok := c.Repos["widgets"]; !ok {
			t.Errorf("entry must be keyed as project.repos writes it, got %v", c.Repos)
		}
	})
	t.Run("default clears the override and drops an empty entry", func(t *testing.T) {
		c := newCfg()
		if err := c.SetParentAutoClose("gadgets", ParentAutoCloseOff); err != nil {
			t.Fatal(err)
		}
		if got := c.ParentAutoCloseFor("gadgets"); got != ParentAutoCloseOff {
			t.Errorf("effective = %q, want false", got)
		}
		if err := c.SetParentAutoClose("gadgets", ParentAutoCloseOn); err != nil {
			t.Fatal(err)
		}
		if c.ParentAutoCloseExplicit("gadgets") || len(c.Repos) != 0 {
			t.Errorf("default must leave no entry, got %v", c.Repos)
		}
	})
	t.Run("keeps other knobs when clearing", func(t *testing.T) {
		c := newCfg()
		c.Repos = map[string]RepoSettings{"widgets": {AgentsMdMaxBytes: 4096, ParentAutoClose: ParentAutoCloseOff}}
		if err := c.SetParentAutoClose("widgets", ParentAutoCloseOn); err != nil {
			t.Fatal(err)
		}
		if rs := c.Repos["widgets"]; rs.AgentsMdMaxBytes != 4096 || rs.ParentAutoClose != "" {
			t.Errorf("entry = %+v", rs)
		}
	})
	t.Run("reuses an existing differently written key", func(t *testing.T) {
		c := newCfg()
		c.Repos = map[string]RepoSettings{"Acme/Widgets": {ParentAutoClose: ParentAutoCloseOff}}
		if err := c.SetParentAutoClose("widgets", ParentAutoCloseEpicsToo); err != nil {
			t.Fatal(err)
		}
		if len(c.Repos) != 1 || c.Repos["Acme/Widgets"].ParentAutoClose != ParentAutoCloseEpicsToo {
			t.Errorf("repos = %v", c.Repos)
		}
	})
	t.Run("rejects bad value and unmonitored repo", func(t *testing.T) {
		c := newCfg()
		if err := c.SetParentAutoClose("widgets", "maybe"); err == nil {
			t.Error("bad value accepted")
		}
		if err := c.SetParentAutoClose("widgets", ""); err == nil {
			t.Error("empty value accepted")
		}
		if err := c.SetParentAutoClose("other/thing", ParentAutoCloseOff); !errors.Is(err, ErrRepoNotMonitored) {
			t.Errorf("unmonitored repo err = %v", err)
		}
		if len(c.Repos) != 0 {
			t.Errorf("a refused set stored %v", c.Repos)
		}
	})
	t.Run("nil config", func(t *testing.T) {
		var c *Config
		if err := c.SetParentAutoClose("widgets", ParentAutoCloseOff); err == nil {
			t.Error("nil config accepted a set")
		}
	})
}

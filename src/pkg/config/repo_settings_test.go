package config

import (
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

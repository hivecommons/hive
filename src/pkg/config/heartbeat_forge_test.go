package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#20181: a Forgejo-only spoke reported only github_host, so
// the hub drew it as a github.com hive. HeartbeatForge is what the spoke now
// reports beside it.
func TestHeartbeatForge(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want HeartbeatForge
	}{
		{
			name: "github hive reports kind only",
			yaml: "project:\n  org: acme\n  repos: [app]\n  primary_repo: app\n  ai_author: bot\n",
			want: HeartbeatForge{Kind: ForgeGitHub},
		},
		{
			name: "forgejo hive reports its instance and login",
			yaml: "project:\n  org: marinatedconcrete\n  repos: [sunshine-buffalo]\n  primary_repo: sunshine-buffalo\n  forge: forgejo\nforgejo:\n  forgejo_url: https://code.tswn.us\n  login: hive-bot\n",
			want: HeartbeatForge{Kind: ForgeForgejo, BaseURL: "https://code.tswn.us", Login: "hive-bot"},
		},
		{
			name: "forgejo login falls back to ai_author",
			yaml: "project:\n  org: acme\n  repos: [app]\n  primary_repo: app\n  forge: forgejo\n  ai_author: someone\nforgejo:\n  forgejo_url: https://code.example.org\n",
			want: HeartbeatForge{Kind: ForgeForgejo, BaseURL: "https://code.example.org", Login: "someone"},
		},
		{
			name: "primary repo override wins over project forge",
			yaml: "project:\n  org: acme\n  repos: [app, web]\n  primary_repo: web\n  repo_policies:\n    - repo: web\n      forge: gitlab\n      forge_url: https://gitlab.example.org/\n",
			want: HeartbeatForge{Kind: ForgeGitLab, BaseURL: "https://gitlab.example.org"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := cfg.HeartbeatForge(); got != tc.want {
				t.Errorf("HeartbeatForge() = %+v, want %+v", got, tc.want)
			}
		})
	}
	var nilCfg *Config
	if got := nilCfg.HeartbeatForge(); got != (HeartbeatForge{Kind: ForgeGitHub}) {
		t.Errorf("nil config = %+v, want github", got)
	}
}

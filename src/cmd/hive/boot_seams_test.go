package main

import (
	"errors"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestResolveDefaultConfigPath(t *testing.T) {
	if got := resolveDefaultConfigPath(""); got != "/etc/hive/hive.yaml" {
		t.Fatalf("unset HIVE_CONFIG: got %q", got)
	}
	if got := resolveDefaultConfigPath("/data/hive.yaml"); got != "/data/hive.yaml" {
		t.Fatalf("set HIVE_CONFIG: got %q", got)
	}
}

func TestConfigPathDisagrees(t *testing.T) {
	cases := []struct {
		env, loaded string
		want        bool
	}{
		{"", "/etc/hive/hive.yaml", false},
		{"/data/hive.yaml", "/data/hive.yaml", false},
		{"/data/hive.yaml", "/etc/hive/hive.yaml", true},
	}
	for _, tc := range cases {
		if got := configPathDisagrees(tc.env, tc.loaded); got != tc.want {
			t.Errorf("configPathDisagrees(%q, %q) = %v, want %v", tc.env, tc.loaded, got, tc.want)
		}
	}
}

func TestCanonicalGitShort(t *testing.T) {
	cases := map[string]string{
		"":          "",
		"abc":       "abc",
		"abcdef0":   "abcdef0",
		"abcdef012": "abcdef0",
	}
	for in, want := range cases {
		if got := canonicalGitShort(in); got != want {
			t.Errorf("canonicalGitShort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveHubTarget(t *testing.T) {
	base := config.HubConfig{Enabled: false, URL: "https://cfg.example", ClusterID: "cfg-cluster"}

	t.Run("no env keeps config and stays disabled", func(t *testing.T) {
		got := resolveHubTarget(base, "", "")
		if got.url != "https://cfg.example" || got.enabled || got.clusterID != "cfg-cluster" {
			t.Fatalf("got %+v", got)
		}
		if got.heartbeatsToHub() {
			t.Fatal("disabled hub must not heartbeat")
		}
	})
	t.Run("HIVE_HUB_URL enables and replaces the url", func(t *testing.T) {
		got := resolveHubTarget(base, "https://env.example", "")
		if got.url != "https://env.example" || !got.enabled || got.clusterID != "cfg-cluster" {
			t.Fatalf("got %+v", got)
		}
		if !got.heartbeatsToHub() {
			t.Fatal("enabled hub with url must heartbeat")
		}
	})
	t.Run("HIVE_CLUSTER_ID overrides only the cluster id", func(t *testing.T) {
		got := resolveHubTarget(base, "", "env-cluster")
		if got.clusterID != "env-cluster" || got.enabled || got.url != "https://cfg.example" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("enabled without url never heartbeats", func(t *testing.T) {
		got := resolveHubTarget(config.HubConfig{Enabled: true}, "", "")
		if got.heartbeatsToHub() {
			t.Fatal("no url: must not heartbeat")
		}
	})
}

func intp(n int) *int { return &n }

var errTest = errors.New("test error")

func TestPlanACMMBoot_FirstStart(t *testing.T) {
	if got := planACMMBoot(true, intp(4), intp(4), ""); got != (acmmBootPlan{}) {
		t.Fatalf("first start ignores config/saved and applies nothing without HIVE_LEVEL: %+v", got)
	}
	if got := planACMMBoot(true, nil, nil, "3"); got.level != 3 || got.action != "auto-applying ACMM pack" || got.invalidEnv != "" {
		t.Fatalf("got %+v", got)
	}
	for _, bad := range []string{"0", "7", "x", "-1"} {
		got := planACMMBoot(true, nil, nil, bad)
		if got.level != 0 || got.invalidEnv != bad {
			t.Errorf("HIVE_LEVEL=%q: got %+v", bad, got)
		}
	}
}

func TestPlanACMMBoot_RestartPrecedence(t *testing.T) {
	cases := []struct {
		name       string
		cfg, saved *int
		env        string
		wantLevel  int
		wantAction string
		wantBadEnv string
	}{
		{"config wins over saved and env", intp(2), intp(5), "6", 2, "re-applying pack (level changed)", ""},
		{"config equal to saved is a merge", intp(5), intp(5), "6", 5, "merging pack updates", ""},
		{"saved wins over env", nil, intp(3), "6", 3, "merging pack updates", ""},
		{"env is the fallback", nil, nil, "4", 4, "re-applying pack (level changed)", ""},
		{"invalid config falls through to saved", intp(9), intp(3), "", 3, "merging pack updates", ""},
		{"invalid saved falls through to env", nil, intp(0), "2", 2, "re-applying pack (level changed)", ""},
		{"invalid env is reported not applied", nil, nil, "42", 0, "", "42"},
		{"invalid env ignored when config is valid", intp(1), nil, "42", 1, "re-applying pack (level changed)", ""},
		{"nothing to apply", nil, nil, "", 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planACMMBoot(false, tc.cfg, tc.saved, tc.env)
			if got.level != tc.wantLevel || got.action != tc.wantAction || got.invalidEnv != tc.wantBadEnv {
				t.Fatalf("got %+v, want level=%d action=%q invalidEnv=%q", got, tc.wantLevel, tc.wantAction, tc.wantBadEnv)
			}
		})
	}
}

func TestParseACMMLevel_Bounds(t *testing.T) {
	for _, ok := range []string{"1", "6"} {
		if _, valid := parseACMMLevel(ok); !valid {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"0", "7", "", "1.5", " 2"} {
		if _, valid := parseACMMLevel(bad); valid {
			t.Errorf("%q must be invalid", bad)
		}
	}
}

func TestJudgeUpgradeMarker(t *testing.T) {
	m := upgradeMarker{CurrentSHA: "aaaaaaa", TargetSHA: "bbbbbbb"}
	if got := judgeUpgradeMarker(m, "bbbbbbb"); got != upgradeLanded {
		t.Fatalf("booted on a different SHA: got %v, want landed", got)
	}
	if got := judgeUpgradeMarker(m, "aaaaaaa"); got != upgradeDidNotLand {
		t.Fatalf("booted on the same SHA: got %v, want did-not-land", got)
	}
	if got := judgeUpgradeMarker(upgradeMarker{}, "aaaaaaa"); got != upgradeLanded {
		t.Fatalf("unparseable marker (empty current) must read as landed so it is cleared: got %v", got)
	}
}

func TestResolveCoverageBadgeURL(t *testing.T) {
	if got := resolveCoverageBadgeURL(""); got != "https://gist.githubusercontent.com/clubanderson/b9a9ae8469f1897a22d5a40629bc1e82/raw/coverage-badge.json" {
		t.Fatalf("default: %q", got)
	}
	if got := resolveCoverageBadgeURL("https://x/badge.json"); got != "https://x/badge.json" {
		t.Fatalf("override: %q", got)
	}
}

func TestMetricsPrimaryRepo(t *testing.T) {
	if got := metricsPrimaryRepo(config.ProjectConfig{PrimaryRepo: "core", Repos: []string{"a", "b"}}); got != "core" {
		t.Fatalf("explicit primary: %q", got)
	}
	if got := metricsPrimaryRepo(config.ProjectConfig{Repos: []string{"a", "b"}}); got != "a" {
		t.Fatalf("first repo: %q", got)
	}
	if got := metricsPrimaryRepo(config.ProjectConfig{}); got != "" {
		t.Fatalf("nothing: %q", got)
	}
}

func TestResolveFleetStatsIdentity(t *testing.T) {
	lookups := 0
	lookup := func(want string, login string, err error) func(string) (string, error) {
		return func(token string) (string, error) {
			lookups++
			if token != want {
				t.Fatalf("lookup got token %q, want %q", token, want)
			}
			return login, err
		}
	}

	t.Run("configured author never looks up", func(t *testing.T) {
		lookups = 0
		id := resolveFleetStatsIdentity("bot[bot]", "tok", "envtok", lookup("", "", nil))
		if id.author != "bot[bot]" || id.fromToken || id.lookupErr != nil || lookups != 0 {
			t.Fatalf("id=%+v lookups=%d", id, lookups)
		}
		if !id.enabled("acme") || id.enabled("") {
			t.Fatal("enabled must require both author and org")
		}
	})
	t.Run("config token beats env token", func(t *testing.T) {
		lookups = 0
		id := resolveFleetStatsIdentity("", "cfgtok", "envtok", lookup("cfgtok", "hive-bot", nil))
		if id.author != "hive-bot" || !id.fromToken || lookups != 1 {
			t.Fatalf("id=%+v lookups=%d", id, lookups)
		}
	})
	t.Run("env token is the fallback", func(t *testing.T) {
		id := resolveFleetStatsIdentity("", "", "envtok", lookup("envtok", "hive-bot", nil))
		if id.author != "hive-bot" || !id.fromToken {
			t.Fatalf("id=%+v", id)
		}
	})
	t.Run("lookup failure leaves author empty and is reported", func(t *testing.T) {
		id := resolveFleetStatsIdentity("", "tok", "", lookup("tok", "ghost", errTest))
		if id.author != "" || id.fromToken || id.lookupErr != errTest || id.enabled("acme") {
			t.Fatalf("id=%+v", id)
		}
	})
	t.Run("empty login is not an author", func(t *testing.T) {
		id := resolveFleetStatsIdentity("", "tok", "", lookup("tok", "", nil))
		if id.author != "" || id.fromToken || id.lookupErr != nil {
			t.Fatalf("id=%+v", id)
		}
	})
	t.Run("no token no lookup", func(t *testing.T) {
		lookups = 0
		id := resolveFleetStatsIdentity("", "", "", lookup("", "x", nil))
		if id.author != "" || lookups != 0 {
			t.Fatalf("id=%+v lookups=%d", id, lookups)
		}
	})
}

func TestPolicyDirs(t *testing.T) {
	cases := []struct {
		name            string
		p               config.PoliciesConfig
		wantLocal, want string
	}{
		{"defaults", config.PoliciesConfig{}, "/data/policies", "/data/policies"},
		{"path only", config.PoliciesConfig{Path: "examples/agents"}, "/data/policies", "/data/policies/examples/agents"},
		{"local only", config.PoliciesConfig{LocalDir: "/srv/pol"}, "/srv/pol", "/srv/pol"},
		{"both", config.PoliciesConfig{LocalDir: "/srv/pol", Path: "team"}, "/srv/pol", "/srv/pol/team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := policiesLocalDir(tc.p); got != tc.wantLocal {
				t.Errorf("policiesLocalDir = %q, want %q", got, tc.wantLocal)
			}
			if got := policyDir(tc.p); got != tc.want {
				t.Errorf("policyDir = %q, want %q", got, tc.want)
			}
		})
	}
}

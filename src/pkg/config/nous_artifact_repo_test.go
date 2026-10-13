package config

import (
	"strings"
	"testing"
	"time"
)

func TestNousArtifactRepoResolveOffByDefault(t *testing.T) {
	var cfg NousArtifactRepoConfig
	got := cfg.Resolve(func(string) string { return "" })
	if got.Enabled || got.Repo != "" {
		t.Fatalf("zero config must be disabled, got %+v", got)
	}
	if got.Path != DefaultNousArtifactPath || got.Branch != DefaultNousArtifactBranch || got.SyncInterval != DefaultNousArtifactSyncInterval {
		t.Fatalf("defaults not applied: %+v", got)
	}
	if nilEnv := cfg.Resolve(nil); nilEnv.Enabled {
		t.Fatalf("nil getenv must still resolve: %+v", nilEnv)
	}
}

func TestNousArtifactRepoResolvePrecedence(t *testing.T) {
	cfg := NousArtifactRepoConfig{Repo: "yaml/repo", Branch: "yaml-branch", Path: "/yaml/path/", SyncIntervalSeconds: 120}

	yamlOnly := cfg.Resolve(func(string) string { return "" })
	if !yamlOnly.Enabled || yamlOnly.Repo != "yaml/repo" || yamlOnly.Branch != "yaml-branch" || yamlOnly.Path != "yaml/path" || yamlOnly.SyncInterval != 120*time.Second {
		t.Fatalf("yaml tier: %+v", yamlOnly)
	}

	env := map[string]string{
		NousArtifactRepoEnv:         " env/repo ",
		NousArtifactBranchEnv:       "env-branch",
		NousArtifactPathEnv:         "env\\path",
		NousArtifactSyncIntervalEnv: "90",
	}
	withEnv := cfg.Resolve(func(k string) string { return env[k] })
	if withEnv.Repo != "env/repo" || withEnv.Branch != "env-branch" || withEnv.Path != "env/path" || withEnv.SyncInterval != 90*time.Second {
		t.Fatalf("env tier must win: %+v", withEnv)
	}

	env[NousArtifactSyncIntervalEnv] = "2m"
	if got := cfg.Resolve(func(k string) string { return env[k] }).SyncInterval; got != 2*time.Minute {
		t.Fatalf("duration env: got %v", got)
	}
	env[NousArtifactSyncIntervalEnv] = "garbage"
	if got := cfg.Resolve(func(k string) string { return env[k] }).SyncInterval; got != 120*time.Second {
		t.Fatalf("unparseable env must fall through to yaml: got %v", got)
	}
	env[NousArtifactSyncIntervalEnv] = "1"
	if got := cfg.Resolve(func(k string) string { return env[k] }).SyncInterval; got != MinNousArtifactSyncInterval {
		t.Fatalf("interval must be floored: got %v", got)
	}

	blank := NousArtifactRepoConfig{Repo: "yaml/repo"}.Resolve(func(k string) string {
		if k == NousArtifactRepoEnv {
			return "   "
		}
		return ""
	})
	if blank.Repo != "yaml/repo" {
		t.Fatalf("blank env must not override yaml: %+v", blank)
	}
}

func TestNormalizeNousArtifactPath(t *testing.T) {
	cases := map[string]string{
		"":              DefaultNousArtifactPath,
		".":             DefaultNousArtifactPath,
		"/":             DefaultNousArtifactPath,
		"campaigns/":    "campaigns",
		"/a//b/":        "a/b",
		"a\\b":          "a/b",
		" results/x/. ": "results/x",
	}
	for in, want := range cases {
		if got := NormalizeNousArtifactPath(in); got != want {
			t.Errorf("NormalizeNousArtifactPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNousArtifactRepoValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     NousArtifactRepoConfig
		wantErr string
	}{
		{name: "empty is off and valid"},
		{name: "valid", cfg: NousArtifactRepoConfig{Repo: "org/results", Branch: "main", Path: "campaigns/2026"}},
		{name: "bad branch/path ignored when repo empty", cfg: NousArtifactRepoConfig{Branch: "a b", Path: "../x"}},
		{name: "repo without owner", cfg: NousArtifactRepoConfig{Repo: "results"}, wantErr: "owner/name"},
		{name: "repo with url", cfg: NousArtifactRepoConfig{Repo: "https://github.com/org/results"}, wantErr: "owner/name"},
		{name: "repo with .git", cfg: NousArtifactRepoConfig{Repo: "org/results.git"}, wantErr: "owner/name"},
		{name: "repo three parts", cfg: NousArtifactRepoConfig{Repo: "a/b/c"}, wantErr: "owner/name"},
		{name: "branch with space", cfg: NousArtifactRepoConfig{Repo: "o/r", Branch: "a b"}, wantErr: "branch"},
		{name: "branch with dotdot", cfg: NousArtifactRepoConfig{Repo: "o/r", Branch: "a..b"}, wantErr: "branch"},
		{name: "branch refs prefix", cfg: NousArtifactRepoConfig{Repo: "o/r", Branch: "refs/heads/x"}, wantErr: "branch"},
		{name: "branch lock suffix", cfg: NousArtifactRepoConfig{Repo: "o/r", Branch: "x.lock"}, wantErr: "branch"},
		{name: "absolute path", cfg: NousArtifactRepoConfig{Repo: "o/r", Path: "/etc"}, wantErr: "relative"},
		{name: "dotdot path", cfg: NousArtifactRepoConfig{Repo: "o/r", Path: "a/../../b"}, wantErr: ".."},
		{name: "negative interval", cfg: NousArtifactRepoConfig{Repo: "o/r", SyncIntervalSeconds: -1}, wantErr: "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestNousValidateIncludesArtifactRepo(t *testing.T) {
	n := NousConfig{ArtifactRepo: NousArtifactRepoConfig{Repo: "bad"}}
	err := n.Validate()
	if err == nil || !strings.Contains(err.Error(), "nous: artifact_repo.repo") {
		t.Fatalf("NousConfig.Validate must surface artifact_repo errors, got %v", err)
	}
}

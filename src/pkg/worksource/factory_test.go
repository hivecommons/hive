package worksource

import (
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestFromConfig_SourceTypes verifies FromConfig returns the adapter whose
// SourceType matches each configured type, and that "" and "github" both map
// to the default GitHub Issues source (backward compatibility).
func TestFromConfig_SourceTypes(t *testing.T) {
	logger := slog.Default()
	cases := []struct {
		cfgType string
		want    string
	}{
		{"", "github"},
		{"github", "github"},
		{"github_projects", "github_projects"},
		{"linear", "linear"},
		{"jira", "jira"},
	}
	for _, tc := range cases {
		cfg := config.WorkSourceConfig{Type: tc.cfgType}
		if tc.cfgType == "linear" {
			cfg.Linear.APIKey = "key"
			cfg.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "my-org/repo"}}
		}
		ws, err := FromConfig(cfg, nil, "tok", "my-org", logger)
		if err != nil {
			t.Fatalf("FromConfig(%q): %v", tc.cfgType, err)
		}
		if got := ws.SourceType(); got != tc.want {
			t.Errorf("FromConfig(%q).SourceType() = %q, want %q", tc.cfgType, got, tc.want)
		}
	}
}

// TestFromConfig_UnknownType verifies an unrecognized type is a hard error,
// not a silent fallback.
func TestFromConfig_UnknownType(t *testing.T) {
	_, err := FromConfig(config.WorkSourceConfig{Type: "gitlab"}, nil, "", "", slog.Default())
	if err == nil {
		t.Fatal("FromConfig with unknown type should error")
	}
}

// TestFromConfig_GitHubProjectsOrgFallback verifies the org falls back to the
// hive's project org when the source config does not set one.
func TestFromConfig_GitHubProjectsOrgFallback(t *testing.T) {
	cfg := config.WorkSourceConfig{
		Type:           "github_projects",
		GitHubProjects: config.GitHubProjectsSourceConfig{ProjectNumber: 7},
	}
	ws, err := FromConfig(cfg, nil, "tok", "fallback-org", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	src, ok := ws.(*githubProjectsSource)
	if !ok {
		t.Fatalf("expected *githubProjectsSource, got %T", ws)
	}
	if src.cfg.Org != "fallback-org" {
		t.Errorf("Org = %q, want fallback-org", src.cfg.Org)
	}
	if src.cfg.Token != "tok" {
		t.Errorf("Token = %q, want tok", src.cfg.Token)
	}
	if src.cfg.ProjectNumber != 7 {
		t.Errorf("ProjectNumber = %d, want 7", src.cfg.ProjectNumber)
	}
}

// TestFromConfig_LinearTeamsMapped verifies team config translates to
// LinearTeamConfig entries.
func TestFromConfig_LinearTeamsMapped(t *testing.T) {
	cfg := config.WorkSourceConfig{
		Type: "linear",
		Linear: config.LinearSourceConfig{
			APIKey: "lin_key",
			Teams: []config.LinearTeamSourceConfig{
				{Key: "ENG", Repo: "my-org/my-repo", States: []string{"Todo"}},
			},
			HoldLabels: []string{"hold"},
		},
	}
	ws, err := FromConfig(cfg, nil, "", "", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	src, ok := ws.(*LinearSource)
	if !ok {
		t.Fatalf("expected *LinearSource, got %T", ws)
	}
	if len(src.cfg.Teams) != 1 || src.cfg.Teams[0].Key != "ENG" || src.cfg.Teams[0].Repo != "my-org/my-repo" {
		t.Errorf("teams not mapped: %+v", src.cfg.Teams)
	}
	if src.cfg.APIKey != "lin_key" {
		t.Errorf("APIKey = %q", src.cfg.APIKey)
	}
}

func TestCoalesce(t *testing.T) {
	if coalesce("a", "b") != "a" {
		t.Error("coalesce should prefer first non-empty")
	}
	if coalesce("", "b") != "b" {
		t.Error("coalesce should fall back to second")
	}
}

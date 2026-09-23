package worksource

import (
	"context"
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

func TestFromConfig_RunStagesWrapsPrimary(t *testing.T) {
	ws, err := FromConfig(config.WorkSourceConfig{Type: "jira", RunStages: true, Jira: config.JiraSourceConfig{
		BaseURL: "https://example.atlassian.net", Email: "bot@example.com", APIToken: "tok", Repo: "acme/repo",
	}}, nil, "", "", slog.Default())
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if _, ok := ws.(*Composite); !ok {
		t.Fatalf("RunStages=true should wrap primary in Composite, got %T", ws)
	}
	if got := ws.SourceType(); got != "jira" {
		t.Fatalf("SourceType = %q, want primary source type", got)
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

func TestAppendAdditive_FlagOffReturnsPrimaryUnchanged(t *testing.T) {
	primary := staticSource{sourceType: "github", issues: []Issue{{
		SourceType: "github", Repo: "hivecommons/hive", ExternalID: "42", Number: 42,
		Title: "regular issue", State: "open",
	}}}
	ws, err := AppendAdditive(primary, config.WorkSourceConfig{})
	if err != nil {
		t.Fatalf("AppendAdditive: %v", err)
	}
	if _, ok := ws.(staticSource); !ok {
		t.Fatalf("flag off must return the primary itself, got %T", ws)
	}
}

func TestAppendAdditive_WavefrontNotLinkedIsAnError(t *testing.T) {
	_, err := AppendAdditive(staticSource{sourceType: "github"}, config.WorkSourceConfig{
		Wavefront: config.WavefrontSourceConfig{Enabled: true, Path: "graph.json", Repo: "acme/repo"},
	})
	if err == nil {
		t.Fatal("enabling wavefront without a linked builder must fail closed, not silently list nothing")
	}
}

func TestAppendAdditive_RunStagesUsesConfiguredAccessor(t *testing.T) {
	SetRunStageAccessor(&stubRunStageLeases{stages: []RunStage{{
		RunKey: "run-8460", Stage: RunStageSpec, Repo: "hivecommons/hive", Title: "gap 2",
	}}})
	t.Cleanup(func() { SetRunStageAccessor(nil) })

	ws, err := AppendAdditive(staticSource{sourceType: "github"}, config.WorkSourceConfig{RunStages: true})
	if err != nil {
		t.Fatalf("AppendAdditive: %v", err)
	}
	got, err := ws.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 || got[0].SourceType != SourceTypeRun || got[0].ExternalID != "run-8460:spec" {
		t.Fatalf("composed run-stage issues = %+v", got)
	}
}

func TestRegisterAdditive_DuplicatePanics(t *testing.T) {
	const name = "test-additive-dup"
	RegisterAdditive(name, func(config.WorkSourceConfig, *slog.Logger) (WorkSource, error) { return nil, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("second RegisterAdditive with the same name should panic")
		}
	}()
	RegisterAdditive(name, func(config.WorkSourceConfig, *slog.Logger) (WorkSource, error) { return nil, nil })
}

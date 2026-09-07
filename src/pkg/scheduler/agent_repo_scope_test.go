package scheduler

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func scopeScheduler(t *testing.T, agents map[string]config.AgentConfig) *Scheduler {
	t.Helper()
	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org:         "my-org",
			Repos:       []string{"console", "dashboard", "docs"},
			PrimaryRepo: "console",
		},
		Agents: agents,
	}
	return New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
}

// An unscoped agent — every agent on a hive that does not use the feature —
// must get the section it always got.
func TestBuildReposSectionFor_UnscopedAgentUnchanged(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	section := s.buildReposSectionFor("scanner")

	if strings.Contains(section, "REPO-SCOPED") {
		t.Errorf("scope note emitted for an unscoped agent:\n%s", section)
	}
	for _, want := range []string{"my-org/console", "my-org/dashboard", "my-org/docs"} {
		if !strings.Contains(section, want) {
			t.Errorf("repo %q missing:\n%s", want, section)
		}
	}
	if !strings.Contains(section, "this project has 3 authorized repos") {
		t.Errorf("rotation note should count all three:\n%s", section)
	}
	// The no-agent form is the same thing, so pre-existing callers are safe.
	if s.buildReposSection() != section {
		t.Error("buildReposSection() drifted from buildReposSectionFor(\"\")")
	}
}

// A scoped agent sees its own repos, and is TOLD it is scoped: an agent that
// has filed issues in a repo for weeks and suddenly does not see it would
// otherwise read the shorter list as scope loss and file a finding about it.
func TestBuildReposSectionFor_ScopedAgent(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"dashboard"}},
	})
	section := s.buildReposSectionFor("schema")

	if !strings.Contains(section, "my-org/dashboard") {
		t.Errorf("agent's own repo missing:\n%s", section)
	}
	for _, unwanted := range []string{"my-org/console", "my-org/docs"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("out-of-scope repo %q offered to a scoped agent:\n%s", unwanted, section)
		}
	}
	if !strings.Contains(section, "REPO-SCOPED") {
		t.Errorf("scoped agent not told it is scoped:\n%s", section)
	}
	if !strings.Contains(section, "NOT scope loss") {
		t.Errorf("scope note does not tell the agent this is deliberate:\n%s", section)
	}
	// One repo left: nothing to rotate between.
	if strings.Contains(section, "MULTI-REPO COVERAGE") {
		t.Errorf("rotation instruction emitted for a single-repo agent:\n%s", section)
	}
}

// The rotation instruction counts the AGENT's repos and names the AGENT's
// primary: pointing a specialist at a repo it may not write to is a
// contradiction it will try to resolve by writing there.
func TestBuildReposSectionFor_RotationUsesAgentReposAndPrimary(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema": {Backend: "claude", Repos: []string{"dashboard", "docs"}},
	})
	section := s.buildReposSectionFor("schema")

	if !strings.Contains(section, "this project has 2 authorized repos") {
		t.Errorf("rotation note should count the agent's repos:\n%s", section)
	}
	if strings.Contains(section, "not just the primary (my-org/console)") {
		t.Errorf("rotation note named the hive primary, which this agent cannot write to:\n%s", section)
	}
	if !strings.Contains(section, "not just the primary (my-org/dashboard)") {
		t.Errorf("rotation note should name the agent's own primary:\n%s", section)
	}
}

// A scope that matches nothing is a misconfiguration, and the kick must say so
// rather than present an empty list the agent has to interpret.
func TestBuildReposSectionFor_ScopeMatchesNothing(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"lost": {Backend: "claude", Repos: []string{"not-watched"}},
	})
	section := s.buildReposSectionFor("lost")
	if !strings.Contains(section, "scoped to repos this hive does not watch") {
		t.Errorf("unmatched scope not stated:\n%s", section)
	}
}

// Task filtering is the cost half of the feature: a scoped agent still wakes on
// its cadence, but it wakes to its own repos' work.
func TestSubstituteTemplate_FiltersIssuesToAgentRepos(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{
		"schema":  {Backend: "claude", Repos: []string{"dashboard"}},
		"scanner": {Backend: "claude"},
	})
	issues := []github.Issue{
		{Repo: "console", Number: 1, Title: "console thing", URL: "u1"},
		{Repo: "dashboard", Number: 2, Title: "dashboard thing", URL: "u2"},
	}
	actionable := &github.ActionableResult{Issues: github.IssueResultFromItems(issues)}

	scoped, _ := s.substituteTemplateWithPolicy("${ISSUE_LIST}|${QUEUE_ISSUES}|${PROJECT_REPOS_LIST}|${PROJECT_PRIMARY_REPO}", actionable, "schema", issues)
	if strings.Contains(scoped, "console thing") {
		t.Errorf("a scoped agent was shown an out-of-scope issue:\n%s", scoped)
	}
	if !strings.Contains(scoped, "dashboard thing") {
		t.Errorf("a scoped agent lost its own issue:\n%s", scoped)
	}
	if !strings.Contains(scoped, "|1|") {
		t.Errorf("QUEUE_ISSUES was not recounted for the scoped agent:\n%s", scoped)
	}
	if !strings.Contains(scoped, "|dashboard|") {
		t.Errorf("PROJECT_REPOS_LIST should be the agent's repos:\n%s", scoped)
	}
	if !strings.Contains(scoped, "my-org/dashboard") {
		t.Errorf("PROJECT_PRIMARY_REPO should be the agent's primary:\n%s", scoped)
	}

	// The unscoped agent still sees everything, from the SAME actionable value:
	// filtering must not mutate the fleet-wide input.
	unscoped, _ := s.substituteTemplateWithPolicy("${ISSUE_LIST}|${QUEUE_ISSUES}|${PROJECT_REPOS_LIST}", actionable, "scanner", issues)
	if !strings.Contains(unscoped, "console thing") || !strings.Contains(unscoped, "dashboard thing") {
		t.Errorf("unscoped agent lost issues, so the shared input was mutated:\n%s", unscoped)
	}
	if !strings.Contains(unscoped, "|2|") {
		t.Errorf("unscoped QUEUE_ISSUES = wrong:\n%s", unscoped)
	}
}

func TestFormatMergeEligibleDataFor_FiltersByRepo(t *testing.T) {
	data := []byte(`{"merge_eligible":[{"number":1,"repo":"console","title":"a"},{"number":2,"repo":"dashboard","title":"b"}]}`)

	all := formatMergeEligibleData(data)
	if !strings.Contains(all, "console") || !strings.Contains(all, "dashboard") {
		t.Errorf("unfiltered list lost an entry: %q", all)
	}
	only := formatMergeEligibleDataFor(data, func(repo string) bool { return repo == "dashboard" })
	if strings.Contains(only, "console") || !strings.Contains(only, "dashboard") {
		t.Errorf("filtered list = %q, want only dashboard", only)
	}
	// Everything filtered out reads as "(none)", not as an empty block.
	none := formatMergeEligibleDataFor(data, func(string) bool { return false })
	if strings.TrimSpace(none) != "(none)" {
		t.Errorf("fully filtered list = %q, want (none)", none)
	}
}

func TestFilterIssuesForRepos(t *testing.T) {
	issues := []github.Issue{{Repo: "a"}, {Repo: "b"}}
	if got := filterIssuesForRepos(issues, nil); len(got) != 2 {
		t.Errorf("nil predicate should pass everything, got %v", got)
	}
	got := filterIssuesForRepos(issues, func(r string) bool { return r == "b" })
	if len(got) != 1 || got[0].Repo != "b" {
		t.Errorf("filterIssuesForRepos = %v, want the b issue", got)
	}
}

package crux

import (
	"reflect"
	"testing"
)

func scopedProject(scopes map[string][]string) ProjectContext {
	p := ProjectContext{
		Org:             "testorg",
		Repos:           []string{"console", "dashboard", "docs"},
		PrimaryRepoName: "console",
		ACMMLevel:       4,
		PRsAllowed:      true,
	}
	if len(scopes) == 0 {
		return p
	}
	p.AgentRepos = func(agent string) []string {
		scope, ok := scopes[agent]
		if !ok {
			return nil
		}
		out := make([]string, 0, len(p.Repos))
		for _, r := range p.Repos {
			for _, s := range scope {
				if r == s {
					out = append(out, r)
				}
			}
		}
		return out
	}
	p.AgentPrimaryRepo = func(agent string) string {
		if repos := p.AgentRepos(agent); len(repos) > 0 {
			return repos[0]
		}
		return ""
	}
	return p
}

func TestProjectContext_PrimaryRepo(t *testing.T) {
	tests := []struct {
		name string
		p    ProjectContext
		want string
	}{
		{"named primary wins", ProjectContext{Org: "o", Repos: []string{"a", "b"}, PrimaryRepoName: "b"}, "b"},
		{"org prefix is stripped", ProjectContext{Org: "o", PrimaryRepoName: "o/b"}, "b"},
		{"blank primary falls back to first repo", ProjectContext{Org: "o", Repos: []string{"o/a", "b"}, PrimaryRepoName: "  "}, "a"},
		{"nothing configured", ProjectContext{Org: "o"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.PrimaryRepo(); got != tc.want {
				t.Fatalf("PrimaryRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProjectContext_ReposForFallsBackWhenUnwired(t *testing.T) {
	p := scopedProject(nil)
	if got := p.ReposFor("anything"); !reflect.DeepEqual(got, p.Repos) {
		t.Fatalf("ReposFor with no scope function = %v, want the hive list", got)
	}
	if got := p.PrimaryRepoFor("anything"); got != "console" {
		t.Errorf("PrimaryRepoFor with no scope function = %q, want console", got)
	}
	if got := p.ActiveRepos(); !reflect.DeepEqual(got, p.Repos) {
		t.Errorf("ActiveRepos with no pause predicate = %v, want the hive list", got)
	}
	if got := p.ActiveReposFor("anything"); !reflect.DeepEqual(got, p.Repos) {
		t.Errorf("ActiveReposFor with no pause predicate = %v, want the hive list", got)
	}
}

func TestProjectContext_ReposForScopedAgent(t *testing.T) {
	p := scopedProject(map[string][]string{"schema": {"dashboard"}})

	if got := p.ReposFor("schema"); !reflect.DeepEqual(got, []string{"dashboard"}) {
		t.Errorf("ReposFor(schema) = %v, want [dashboard]", got)
	}
	// A scope function answering nil means "unscoped": the hive list applies.
	if got := p.ReposFor("scanner"); !reflect.DeepEqual(got, p.Repos) {
		t.Errorf("ReposFor(unscoped) = %v, want all three", got)
	}
	if got := p.PrimaryRepoFor("schema"); got != "dashboard" {
		t.Errorf("PrimaryRepoFor(schema) = %q, want dashboard", got)
	}
	if got := p.PrimaryRepoFor("scanner"); got != "console" {
		t.Errorf("PrimaryRepoFor(unscoped) = %q, want console", got)
	}
	if len(p.Repos) != 3 {
		t.Errorf("Repos was mutated to %v", p.Repos)
	}
}

func TestProjectContext_PrimaryRepoForStripsOrgAndFallsBack(t *testing.T) {
	p := scopedProject(nil)
	p.AgentPrimaryRepo = func(agent string) string {
		switch agent {
		case "qualified":
			return " testorg/docs "
		default:
			return ""
		}
	}
	if got := p.PrimaryRepoFor("qualified"); got != "docs" {
		t.Errorf("PrimaryRepoFor(qualified) = %q, want docs", got)
	}
	// A blank per-agent answer falls through to the hive primary.
	if got := p.PrimaryRepoFor("blank"); got != "console" {
		t.Errorf("PrimaryRepoFor(blank) = %q, want console", got)
	}
}

func TestProjectContext_PausedReposDropOutOfActiveScope(t *testing.T) {
	p := scopedProject(map[string][]string{"schema": {"dashboard", "docs"}})
	p.RepoPaused = func(repo string) bool { return repo == "dashboard" }

	if got := p.ActiveRepos(); !reflect.DeepEqual(got, []string{"console", "docs"}) {
		t.Errorf("ActiveRepos = %v, want [console docs]", got)
	}
	if got := p.ActiveReposFor("schema"); !reflect.DeepEqual(got, []string{"docs"}) {
		t.Errorf("ActiveReposFor(schema) = %v, want [docs]", got)
	}
	// The pause narrows the work scope only: the scope and the primary are
	// untouched, so unpausing restores them without a re-scope.
	if got := p.ReposFor("schema"); !reflect.DeepEqual(got, []string{"dashboard", "docs"}) {
		t.Errorf("ReposFor(schema) = %v, want the unfiltered scope", got)
	}
	if got := p.PrimaryRepoFor("schema"); got != "dashboard" {
		t.Errorf("PrimaryRepoFor(schema) = %q, want dashboard even while paused", got)
	}
	if got := p.PrimaryRepo(); got != "console" {
		t.Errorf("PrimaryRepo = %q, want console", got)
	}
}

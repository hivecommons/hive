package agent

import "testing"

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
			return p.Repos
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

func TestProjectContext_ReposForFallsBackWhenUnwired(t *testing.T) {
	p := scopedProject(nil)
	if got := p.ReposFor("anything"); len(got) != 3 {
		t.Fatalf("ReposFor with no scope function = %v, want the hive list", got)
	}
	if got := p.PrimaryRepoFor("anything"); got != "console" {
		t.Errorf("PrimaryRepoFor with no scope function = %q, want console", got)
	}
}

func TestProjectContext_ReposForScopedAgent(t *testing.T) {
	p := scopedProject(map[string][]string{"schema": {"dashboard"}})

	if got := p.ReposFor("schema"); len(got) != 1 || got[0] != "dashboard" {
		t.Errorf("ReposFor(schema) = %v, want [dashboard]", got)
	}
	if got := p.ReposFor("scanner"); len(got) != 3 {
		t.Errorf("ReposFor(unscoped) = %v, want all three", got)
	}
	// $HIVE_REPO must point a specialist at a repo it may actually write to.
	if got := p.PrimaryRepoFor("schema"); got != "dashboard" {
		t.Errorf("PrimaryRepoFor(schema) = %q, want dashboard", got)
	}
	if got := p.PrimaryRepoFor("scanner"); got != "console" {
		t.Errorf("PrimaryRepoFor(unscoped) = %q, want console", got)
	}
	// The hive's own repo list is untouched: the scope narrows a view, not the
	// hive.
	if len(p.Repos) != 3 {
		t.Errorf("Repos was mutated to %v", p.Repos)
	}
}

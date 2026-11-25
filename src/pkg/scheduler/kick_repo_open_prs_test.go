package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestBuildReposSectionFor_RepoOpenPRsNotes(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	section := s.buildReposSectionFor("scanner")
	if strings.Contains(section, "PRs are off") || strings.Contains(section, "only workflow hand-off PRs") {
		t.Fatalf("no note expected while every repo is on:\n%s", section)
	}

	s.cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "docs", OpenPRs: string(config.RepoOpenPRsWorkflowOnly)},
		{Repo: "dashboard", OpenPRs: string(config.RepoOpenPRsOff)},
	}
	section = s.buildReposSectionFor("scanner")
	for _, want := range []string{
		"PRs are off for my-org/dashboard",
		"file findings as issues",
		"my-org/docs accepts only workflow hand-off PRs",
		"Workflow hand-off",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("kick lacks %q\n%s", want, section)
		}
	}
	if strings.Contains(section, "my-org/console accepts only") || strings.Contains(section, "PRs are off for my-org/console") {
		t.Errorf("an unaffected repo got a note:\n%s", section)
	}

	off := false
	s.cfg.Project.OpenPRs = &off
	section = s.buildReposSectionFor("scanner")
	for _, repo := range []string{"console", "dashboard", "docs"} {
		if !strings.Contains(section, "PRs are off for my-org/"+repo) {
			t.Errorf("project.open_prs=false: kick lacks the off note for %s\n%s", repo, section)
		}
	}
}

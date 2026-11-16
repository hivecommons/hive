package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#18516: a bot-identity reference repo gets its own PR
// instruction (no fork, push the topic branch to the repo itself), and in a
// mixed set the owner's fork paragraph no longer claims to cover it.
func TestBuildReposSectionFor_ReferenceRepoBotInstruction(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	s.cfg.Project.ReferenceRepos = []config.ReferenceRepo{
		{Repo: "hivecommons/docs", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier, CredentialLogin: "hivecommons-hive[bot]", CredentialIdentity: config.ReferenceIdentityBot},
		{Repo: "acme/site", Write: []string{"issues", "prs"}, DerivedFrom: "cncf/techdocs#395", CredentialState: config.ReferenceCredentialWriteTier, CredentialLogin: "nate-double-u", CredentialIdentity: config.ReferenceIdentityOwner},
	}
	section := s.buildReposSectionFor("scanner")
	for _, want := range []string{
		"Writes to hivecommons/docs appear under the hive's GitHub App, hivecommons-hive[bot]",
		"do NOT fork: push your topic branch to the reference repo itself (never its default branch)",
		"--head <branch>",
		"Writes to any other write-tier reference repo appear under the GitHub account nate-double-u (the hive owner)",
		"You CANNOT push to those repos themselves",
		"--head nate-double-u:<branch>",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("REFERENCE REPOS block lacks %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "https://github.com/hivecommons-hive[bot]/") {
		t.Error("the bot has no fork; the kick must never point at one")
	}

	// Bot only: no fork paragraph at all.
	s.cfg.Project.ReferenceRepos = s.cfg.Project.ReferenceRepos[:1]
	section = s.buildReposSectionFor("scanner")
	if strings.Contains(section, "gh repo fork") || !strings.Contains(section, "do NOT fork") {
		t.Errorf("bot-only set must not render the fork flow:\n%s", section)
	}
}

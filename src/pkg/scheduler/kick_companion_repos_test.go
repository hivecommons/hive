package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/decisionpolicy"
)

// hivecommons/hive#13059: resident agents get the same two rules contributor
// agents get — a target repo's own confirmation gate is satisfied by the
// dispatch, and a registered companion checkout is cloned, not reported —
// plus the clone plan for any companion repos the operator configured.
func TestBuildReposSectionFor_RepoLocalGateAndCompanions(t *testing.T) {
	s := scopeScheduler(t, map[string]config.AgentConfig{"scanner": {Backend: "claude"}})
	section := s.buildReposSectionFor("scanner")
	if !strings.Contains(section, decisionpolicy.DispatchSatisfiesRepoGates) {
		t.Error("kick must carry the dispatch-satisfies-gates rule")
	}
	if !strings.Contains(section, decisionpolicy.MissingCompanionCheckoutRule) {
		t.Error("kick must carry the missing-companion-checkout rule")
	}
	if strings.Contains(section, "companion repositor") {
		t.Error("no companion clone plan without configured companions")
	}

	s.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "docs", CompanionRepos: []string{"docs-website", "other-org/theme"}}}
	section = s.buildReposSectionFor("scanner")
	for _, want := range []string{
		"my-org/docs registers 2 companion repositories",
		"$HOME/workspace/my-org/docs-website",
		"$HOME/workspace/other-org/theme",
		"git clone --depth 1",
		"clone the primary to $HOME/workspace/my-org/docs",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("kick lacks %q\n%s", want, section)
		}
	}
}

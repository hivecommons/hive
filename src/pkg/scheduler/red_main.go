package scheduler

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// addRedMainNotice tells the agent which of its repos have a red default
// branch past the red-main threshold (hivecommons/hive#13630): new PRs there
// are refused by hive-open-pr until main is green, unless they fix main. It
// names the red check and the fix PR so the agent helps the fix land
// instead of opening PRs that would be red too. Nothing is added when every
// default branch is green.
func (s *Scheduler) addRedMainNotice(agentName string, actionable *github.ActionableResult, message string) string {
	if actionable == nil || len(actionable.RedMain) == 0 {
		return message
	}
	active, _ := s.activeReposForAgent(agentName)
	var lines []string
	for _, repo := range active {
		fact := github.RedMainFactFor(actionable.RedMain, config.QualifyRepo(s.cfg.Project.Org, repo))
		if fact == nil {
			continue
		}
		lines = append(lines, "  "+fact.Repo+": "+fact.CardLine()+"\n    "+fact.PauseReason())
	}
	if len(lines) == 0 {
		return message
	}
	section := "🚨 RED MAIN — NEW PRs PAUSED (existing PRs and issue filing are not affected):\n" + strings.Join(lines, "\n")
	return appendKickSection(message, section)
}

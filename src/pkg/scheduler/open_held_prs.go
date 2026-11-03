package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// addOpenHeldPRCapNotice tells a PR-opening agent which of its repos are at
// their max_open_held_prs cap (hivecommons/hive#13645): hive-open-pr refuses
// new PRs there until held hive PRs are reviewed, released or closed. Issue
// filing is unaffected. Nothing is added for repos without a cap or below it.
func (s *Scheduler) addOpenHeldPRCapNotice(agentName string, actionable *github.ActionableResult, message string) string {
	if message == "" || actionable == nil || len(actionable.OpenHeldPRs) == 0 || !s.isPRCapableAgent(agentName) {
		return message
	}
	active, _ := s.activeReposForAgent(agentName)
	var lines []string
	for _, repo := range active {
		full := config.QualifyRepo(s.cfg.Project.Org, repo)
		o, ok := github.OpenHeldPRsFor(actionable.OpenHeldPRs, full)
		if !ok || !o.AtCapacity() {
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: %d open held hive PRs (max_open_held_prs %d)\n    %s", full, o.Count, o.Max, github.RepoAtCapacityLine))
	}
	if len(lines) == 0 {
		return message
	}
	section := "🧱 PR CAPACITY — NEW PRs PAUSED (issue filing is not affected):\n" + strings.Join(lines, "\n")
	return appendKickSection(message, section)
}

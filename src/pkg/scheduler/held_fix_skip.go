package scheduler

import (
	"strconv"
	"strings"
)

// HeldFixSkipMessage is the one INFO line logged when a fix candidate (a
// red PR's CI repair row) is skipped because the PR is held. A held PR is
// hands-off for every agent but its own author's narrow FIX-BEFORE-NEW
// exception; since the human-merge-path rule no longer holds PRs, a held PR
// is one a person (or a hive gate label such as decision-needed) stopped.
// The line makes "never handed to the fixer" visible instead of silent.
const HeldFixSkipMessage = "fix candidate skipped: the PR is held"

// logHeldFixSkip logs HeldFixSkipMessage once per kind, PR and head: a
// later pass on the same head stays quiet, a new head logs again. A nil
// scheduler or logger logs nothing.
func (s *Scheduler) logHeldFixSkip(kind, repo string, number int, head string) {
	if s == nil || s.logger == nil || number <= 0 {
		return
	}
	head = strings.TrimSpace(head)
	key := kind + " " + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
	if prev, ok := s.heldFixSkipLogged.Load(key); ok && prev == head {
		return
	}
	s.heldFixSkipLogged.Store(key, head)
	s.logger.Info(HeldFixSkipMessage, "kind", kind, "repo", repo, "pr", number, "head", head)
}

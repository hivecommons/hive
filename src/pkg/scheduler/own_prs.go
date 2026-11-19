package scheduler

import (
	"strconv"
	"strings"
)

// An agent's own PRs (hivecommons/hive#19325).
//
// A PR the hive's App opens in a repo carries the App as its GitHub author
// whichever agent opened it, so the kick cannot tell an agent its own PRs from
// the author field. The audit trail records which agent opened each PR
// (agent_pr_created); cmd/hive installs a lookup over it, and the held-PR
// block tags each PR the agent opened. Without the tag an agent judged a PR it
// had opened as another lane's from its topic or labels, and never answered
// the review on it.

// PRAuthorLookup returns "owner/repo#number" (or "repo#number") -> the agent
// that opened the PR.
type PRAuthorLookup func() map[string]string

// ownPRTagText marks a PR the kicked agent opened.
const ownPRTagText = " [YOURS: you opened it]"

// SetPRAuthorLookup installs (or with nil, removes) the PR-author lookup.
func (s *Scheduler) SetPRAuthorLookup(fn PRAuthorLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prAuthors = fn
}

// ownedPR is one PR an agent opened.
type ownedPR struct {
	repo   string
	number int
}

// prsOpenedBy returns the PRs agentName's lane opened, per the installed
// lookup. Instances of a lane share its PRs: a replica answers a review on a
// PR its sibling opened.
func (s *Scheduler) prsOpenedBy(agentName string) []ownedPR {
	s.mu.RLock()
	fn := s.prAuthors
	s.mu.RUnlock()
	if fn == nil || strings.TrimSpace(agentName) == "" {
		return nil
	}
	base := agentName
	if s.cfg != nil {
		base = s.cfg.BaseAgentName(agentName)
	}
	var out []ownedPR
	for key, agent := range fn() {
		agent = strings.TrimSpace(agent)
		if agent == "" {
			continue
		}
		if s.cfg != nil {
			agent = s.cfg.BaseAgentName(agent)
		}
		if agent != base {
			continue
		}
		i := strings.LastIndex(key, "#")
		if i <= 0 {
			continue
		}
		n, err := strconv.Atoi(key[i+1:])
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, ownedPR{repo: key[:i], number: n})
	}
	return out
}

// ownPRTag returns ownPRTagText when repo#number is in opened, else "".
func ownPRTag(opened []ownedPR, repo string, number int) string {
	for _, p := range opened {
		if p.number == number && sameRepoSpelling(p.repo, repo) {
			return ownPRTagText
		}
	}
	return ""
}

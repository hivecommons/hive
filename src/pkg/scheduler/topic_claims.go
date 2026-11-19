package scheduler

import (
	"fmt"
	"strings"
)

// Topic claims in the kick (hivecommons/hive#18493).
//
// A long-lived agent records "topic key -> one PR" with `bd topic claim`
// (pkg/beads/topic.go). The kick shows the agent its open claims so it finds
// its PR by number on every later kick, even after a branch rename, and the
// FIX-BEFORE-NEW and held-PR blocks name the topic next to the PR.

// TopicClaim is one open topic claim, as the kick needs it. Agent is the bead
// store (lane) that holds it.
type TopicClaim struct {
	Agent  string
	Key    string
	Repo   string
	PR     int
	Branch string
}

// TopicClaimLookup returns every open topic claim across the hive's agents.
type TopicClaimLookup func() []TopicClaim

// SetTopicClaimLookup installs (or with nil, removes) the topic-claim lookup.
func (s *Scheduler) SetTopicClaimLookup(fn TopicClaimLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.topicClaims = fn
}

// allTopicClaims returns the installed lookup's claims, or nil.
func (s *Scheduler) allTopicClaims() []TopicClaim {
	s.mu.RLock()
	fn := s.topicClaims
	s.mu.RUnlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// topicClaimsFor returns the open claims held by agentName's lane.
func (s *Scheduler) topicClaimsFor(agentName string) []TopicClaim {
	base := s.cfg.BaseAgentName(agentName)
	var out []TopicClaim
	for _, c := range s.allTopicClaims() {
		if c.Agent == base || c.Agent == agentName {
			out = append(out, c)
		}
	}
	return out
}

// topicKeyFor returns the topic key held through repo#number, or "".
func topicKeyFor(claims []TopicClaim, repo string, number int) string {
	for _, c := range claims {
		if c.PR == number && sameRepoSpelling(c.Repo, repo) {
			return c.Key
		}
	}
	return ""
}

// topicTag is the " [topic: key]" suffix a PR row carries, or "".
func topicTag(claims []TopicClaim, repo string, number int) string {
	if key := topicKeyFor(claims, repo, number); key != "" {
		return " [topic: " + key + "]"
	}
	return ""
}

// sameRepoSpelling matches "owner/name" whole when both sides carry an owner,
// otherwise by name only (kick surfaces carry either spelling).
func sameRepoSpelling(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return a == b
	}
	return a[strings.LastIndex(a, "/")+1:] == b[strings.LastIndex(b, "/")+1:]
}

// topicClaimsHeader opens the kick block; the seam skips a kick that already
// carries it (the template placed ${TOPIC_CLAIMS}).
const topicClaimsHeader = "## 📌 Topic claims (one PR per topic)"

// topicClaimsUsage is how an agent claims, reads and releases a topic.
const topicClaimsUsage = "Claim a new topic right after opening its PR: `bd topic claim <key> --repo <owner/name> --pr <n> --branch <branch>`.\n" +
	"Read one back with `bd topic get <key>`; give one up with `bd topic release <key>`. A second claim on an\n" +
	"open topic for a different PR is refused. Hive releases a claim when its PR merges or closes.\n" +
	"In a sandbox, use `hive-topic` with the same arguments; `hive-topic claim <key>` without --pr claims the PR hive opens from this kick.\n"

// topicClaimsSection renders the ${TOPIC_CLAIMS} block. It always carries the
// usage lines, so a template that places the variable teaches the command
// even before the agent holds a claim.
func topicClaimsSection(claims []TopicClaim) string {
	var b strings.Builder
	b.WriteString(topicClaimsHeader + "\n\n")
	if len(claims) == 0 {
		b.WriteString("You hold no topic claims.\n")
	} else {
		b.WriteString("You own these topics. Find each PR by NUMBER, never by branch name, and keep working\n")
		b.WriteString("on that PR (`gh pr checkout <pr> --repo <repo>`). Do NOT open a second PR for a claimed topic:\n")
		for _, c := range claims {
			line := fmt.Sprintf("  %s → %s#%d", c.Key, c.Repo, c.PR)
			if c.Branch != "" {
				line += " (branch " + c.Branch + ")"
			}
			if c.PR == 0 {
				// A pending claim from a sandboxed kick (#18572): hive binds
				// it once the PR opened from the branch appears.
				line = fmt.Sprintf("  %s → %s, PR not opened yet (pending on branch %s)", c.Key, c.Repo, c.Branch)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(topicClaimsUsage)
	return b.String()
}

// addTopicClaims appends the agent's topic claims to a resolved kick when it
// holds any and the template did not place ${TOPIC_CLAIMS} itself.
func (s *Scheduler) addTopicClaims(agentName, message string) string {
	if message == "" || strings.Contains(message, topicClaimsHeader) {
		return message
	}
	claims := s.topicClaimsFor(agentName)
	if len(claims) == 0 {
		return message
	}
	return appendKickSection(message, topicClaimsSection(claims))
}

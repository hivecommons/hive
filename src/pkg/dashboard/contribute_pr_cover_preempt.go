package dashboard

import (
	"fmt"
	"strings"
	"time"
)

// PreemptIssueCoveredByPR releases every in-flight contributor task on
// repo#number because an open PR by someone else now declares it fixes the
// issue, and hands each released session different work (the same yank
// PreemptContributorIssue performs for a claim takeover).
//
// Before this, the ledger stopped OFFERING the issue the moment the PR's
// webhook arrived, but a contributor already dispatched on it was never
// told: it kept working for minutes and opened a second PR for the same
// issue, which the merge queue then carried alongside the first
// (hivecommons/hive#15469 → #15470 by the owner, #15473 by the hive's
// own fixer eight minutes later). The contributor's OWN PR arrives
// through the same webhook and must not revoke the task it completes, so
// a session whose GitHub username is the PR author is left alone.
// Returns the number of sessions released.
func (h *ContributeWSHub) PreemptIssueCoveredByPR(repo string, number, pr int, prAuthor string) int {
	if h == nil || number <= 0 || pr <= 0 {
		return 0
	}
	prAuthor = strings.TrimSpace(prAuthor)
	var targets []releaseTarget
	h.mu.RLock()
	for _, c := range h.connections {
		c.mu.Lock()
		match := c.currentTask != nil && c.currentTask.Number == number &&
			sameRepoSpelling(c.currentTask.Repo, repo) && !connectionOwnsPRAuthor(c, prAuthor)
		if match {
			released := *c.currentTask
			c.currentTask = nil
			c.currentPrompt = ""
			c.currentLabels = nil
			c.tokenMintedAt = time.Time{}
			c.pendingToken = ""
			c.credentialDelivered = false
			c.currentTaskGen = h.nextTaskGen()
			c.lastLeaseRenew = time.Time{}
			targets = append(targets, releaseTarget{conn: c, task: released})
		}
		c.mu.Unlock()
	}
	h.mu.RUnlock()
	if len(targets) == 0 {
		return 0
	}
	reason := fmt.Sprintf("preempted: open PR #%d already covers this issue", pr)
	if prAuthor != "" {
		reason = fmt.Sprintf("preempted: open PR #%d by @%s already covers this issue", pr, prAuthor)
	}
	released, _ := h.yankAndReassign(targets, reason, "preempted by an open PR covering the issue", "reassigned after PR cover")
	return released
}

// connectionOwnsPRAuthor reports whether the session's GitHub account is the
// PR author (case-insensitively), so a contributor's own PR never preempts
// the task that produced it. Callers hold c.mu.
func connectionOwnsPRAuthor(c *ContributorConnection, prAuthor string) bool {
	if c == nil || c.profile == nil || prAuthor == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c.profile.GitHubUsername), prAuthor)
}

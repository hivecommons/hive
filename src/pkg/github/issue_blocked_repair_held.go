package github

import (
	"strings"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The blocked sweep asks the write-back gate about every marker-less
// hive-applied block on every pass, and a refusal that lasts would be logged
// each time. The gate remembers the reason it last refused each issue for
// (writeback.Gate.NoteHeld); these helpers keep that memory in step with the
// sweep, so a hold is reported once per issue and reason and again after the
// issue stopped being one the repair converts.

// blockedRepairRepo is repo as owner/name, the form the gate is asked with.
func (c *Client) blockedRepairRepo(repo string) string {
	owner, name := c.splitRepo(repo)
	return owner + "/" + name
}

// forgetBlockedRepairHeld drops the hold remembered for t: the repair went
// through, or t is no longer a block the repair converts (it names what it
// waits on, a person applied it, it is a review-backlog item). A later hold
// is then logged again.
func forgetBlockedRepairHeld(client *Client, t blockedIssueTarget) {
	client.WritebackGate().ForgetHeld(client.blockedRepairRepo(t.repo), t.number, writeback.KindBlockedRepair)
}

// retainBlockedRepairHeld forgets the holds of issues in repos that targets,
// the complete listing of open `blocked` issues in those repos, no longer
// contains: an issue that was closed, or whose label was removed, never
// reaches the repair again. A repo outside this listing keeps its holds, so
// a sweep of some repos does not forget the others.
func (c *Client) retainBlockedRepairHeld(repos []string, targets []blockedIssueTarget) {
	gate := c.WritebackGate()
	if gate == nil {
		return
	}
	listed := make(map[string]bool, len(repos))
	for _, repo := range repos {
		listed[strings.ToLower(c.blockedRepairRepo(repo))] = true
	}
	open := make(map[string]bool, len(targets))
	for _, t := range targets {
		open[blockedOnSeenKey(c.blockedRepairRepo(t.repo), t.number)] = true
	}
	gate.RetainHeld(writeback.KindBlockedRepair, func(repo string, number int) bool {
		return !listed[repo] || open[blockedOnSeenKey(repo, number)]
	})
}

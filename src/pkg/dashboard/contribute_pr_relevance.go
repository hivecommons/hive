package dashboard

import (
	"regexp"
	"strconv"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// reportedPRAssignmentSkew is how far before the task's assignment a reported
// PR's GitHub creation time may fall and still count as opened for the task.
// It absorbs clock skew between the hub and GitHub; it is not a grace period
// for older work.
const reportedPRAssignmentSkew = 2 * time.Minute

// reportedPRUnrelatedReason is the PRVerification.Reason a completion's PR
// carries after unrelatedReportedPR downgraded it.
const reportedPRUnrelatedReason = "PR predates the assignment and does not reference the dispatched issue"

// unrelatedReportedPR reports whether a verified completion PR is not the work
// of the dispatched issue task (hivecommons/hive#19985).
//
// The relay scrapes the PR URL out of the agent's terminal output, and
// VerifyReportedPR only checks the base repo and the author. A contributor who
// reported "blocked: waits on my PR #19984" therefore had #19984, their own
// open PR for a different issue, verified as the task's PR. A verified PR
// overrides the blocked verdict, so the hub appended `Fixes #19985` to #19984,
// recorded it as #19985's covering PR and moved #19985 to covered-by-pr. When
// #19984 closed, the claim was released and the issue fell back to actionable
// (the tracker's `regressed`), with no `blocked` label.
//
// A PR is treated as unrelated only when both hold: GitHub created it before
// the task was assigned (minus reportedPRAssignmentSkew), and neither its
// title, body nor head branch names the issue. A PR opened during the task, or
// an older PR that already says it is for this issue (a re-dispatch that
// pushes to the contributor's existing PR), keeps its verification. With no
// assignment time (an adopted task) or no creation time there is nothing to
// compare, so the PR is left alone.
func unrelatedReportedPR(pr ghpkg.PRVerification, task *WSTaskAssign, assignedAt time.Time) bool {
	if !pr.Verified || task == nil || task.Kind != "issue" || task.Number <= 0 {
		return false
	}
	if task.SourceType != "" && task.SourceType != "github" {
		return false
	}
	if assignedAt.IsZero() || pr.CreatedAt.IsZero() {
		return false
	}
	if !pr.CreatedAt.Before(assignedAt.Add(-reportedPRAssignmentSkew)) {
		return false
	}
	return !prTextReferencesIssue(pr, task.Number)
}

// prTextReferencesIssue reports whether the PR's title or body names issue n
// as `#n` or an `/issues/n` URL, or its head branch carries n as a whole
// number (`scanner/fix-19985`).
func prTextReferencesIssue(pr ghpkg.PRVerification, n int) bool {
	num := strconv.Itoa(n)
	textRE := regexp.MustCompile(`(?:#|/issues/)` + num + `(?:[^0-9]|$)`)
	if textRE.MatchString(pr.Title) || textRE.MatchString(pr.Body) {
		return true
	}
	branchRE := regexp.MustCompile(`(?:^|[^0-9])` + num + `(?:[^0-9]|$)`)
	return branchRE.MatchString(pr.HeadRef)
}

// dropUnrelatedReportedPR downgrades a verified completion PR that
// unrelatedReportedPR rejects to unverified, so it earns no shipped verdict,
// no claim, no `Fixes #N` edit and no trust credit, and the relay's own
// verdict (blocked, needs-decision, no-work) stands.
func (h *ContributeWSHub) dropUnrelatedReportedPR(task *WSTaskAssign, pr ghpkg.PRVerification, prURL, username string, assignedAt time.Time) ghpkg.PRVerification {
	if !unrelatedReportedPR(pr, task, assignedAt) {
		return pr
	}
	if h != nil && h.logger != nil {
		h.logger.Warn("[contribute-ws] reported PR NOT counted for the task: it predates the assignment and does not reference the issue",
			"repo", task.Repo, "issue", task.Number, "pr_url", prURL, "username", username,
			"pr_created_at", pr.CreatedAt, "assigned_at", assignedAt)
	}
	pr.Verified = false
	pr.Reason = reportedPRUnrelatedReason
	return pr
}

package dashboard

import (
	"regexp"
	"strconv"
	"strings"
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
	return !prTextReferencesIssue(pr, task.Repo, task.Number)
}

// prIssueRefRE matches `#N`, `owner/repo#N` and `.../owner/repo/issues/N`
// references, capturing the optional owner/repo and the number.
var prIssueRefRE = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#|(?:^|[^A-Za-z0-9_./-])#|([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/issues/)([0-9]+)`)

// prBranchNumberRE matches whole numbers in a head branch name.
var prBranchNumberRE = regexp.MustCompile(`[0-9]+`)

// prTextReferencesIssue reports whether the PR's title or body names issue n
// of repo as `#n`, `repo#n` or a `repo/issues/n` URL, or its head branch
// carries n as a whole number (`scanner/fix-19985`). A reference that names
// another repository does not count.
func prTextReferencesIssue(pr ghpkg.PRVerification, repo string, n int) bool {
	num := strconv.Itoa(n)
	for _, text := range []string{pr.Title, pr.Body} {
		for _, m := range prIssueRefRE.FindAllStringSubmatch(text, -1) {
			if m[3] != num {
				continue
			}
			named := m[1]
			if named == "" {
				named = m[2]
			}
			if named == "" || repo == "" || strings.EqualFold(named, repo) {
				return true
			}
		}
	}
	for _, m := range prBranchNumberRE.FindAllString(pr.HeadRef, -1) {
		if m == num {
			return true
		}
	}
	return false
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

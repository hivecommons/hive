package automerge

// Prow presubmits on non-member fork PRs (hivecommons/hive#17474, #17451).
//
// On a Prow-managed repository the trigger plugin runs presubmits only for a
// PR whose author is one of its trusted users (an org member or collaborator)
// or that carries `ok-to-test`. A contributor-lane fork PR by a non-member
// therefore never gets them, neither when it is opened nor after the hive's
// own base refresh, and Tide reports "Jobs … have not succeeded" on a PR that
// is green on GitHub Actions (#17359 sat about four hours until a person
// commented `/ok-to-test`).
//
// For a fork PR the contributor lane admitted (the hive vouches for the author
// and refreshes or merges the PR), the lane posts `/ok-to-test` once per head
// through the mutation boundary and per-head marker the held-runs path uses
// (postWorkflowApprovalCommand). If Prow answers that the App is not a trusted
// user, the lane records that once (hgithub.PRApprovalCommandsSkippedProw),
// leaves one needs-human note and never posts the command on the PR again.
// Whether the App is a Prow trusted user is the repository's Prow config, not
// the hive's to decide.

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	// prowOkToTestLabel is the trigger plugin's label for a PR whose
	// presubmits a trusted user released; it persists across pushes.
	prowOkToTestLabel = "ok-to-test"
	// prowTideContext is the commit status Tide itself reports; its creator
	// is the Prow account that also reports the presubmit jobs.
	prowTideContext = "tide"
)

// prowTrustedAssociations are the author_association values Prow's trigger
// plugin trusts by default: presubmits already run for these authors.
var prowTrustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
}

// requestProwPresubmitsForFork posts `/ok-to-test` once for headSHA when pr is
// a fork PR by a non-member on a Prow-managed repository (Prow's labels or its
// approve plugin's comment), carries no `ok-to-test` label, and Prow has
// reported no presubmit status on the head. A Prow rejection of the App's
// command is recorded once per PR and stops every later post. The caller
// must already have admitted the PR to the contributor lane.
func (c *Engine) requestProwPresubmitsForFork(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA string) {
	if c == nil || c.gh == nil || pr == nil || strings.TrimSpace(headSHA) == "" || !c.isForkPR(pr) {
		return
	}
	if prowTrustedAssociations[strings.ToUpper(strings.TrimSpace(pr.GetAuthorAssociation()))] {
		return
	}
	labels := labelNames(pr.Labels)
	if hasLabel(labels, prowOkToTestLabel) || c.workflowApprovalUntrustedKnown(owner, repo, number) {
		return
	}
	_, first := c.markWorkflowApprovalCommandSeen(owner, repo, number, headSHA)
	state, scanned := c.workflowApprovalCommentState(ctx, displayRepo, owner, repo, number, headSHA, first)
	if !scanned {
		return
	}
	if state.rejected {
		c.markWorkflowApprovalUntrusted(owner, repo, number)
		args := []any{"repo", displayRepo, "pr", number, "sha", headSHA, "skipped", hgithub.PRApprovalCommandsSkippedProw}
		const msg = "contributor-lane ok-to-test not posted again: Prow rejected the App's command on this fork PR; a trusted user must post it"
		if state.rejectionNoted {
			c.debug(msg, args...)
			return
		}
		c.postWorkflowApprovalUntrustedNote(ctx, displayRepo, owner, repo, number, headSHA, prowPresubmitsRejectedNote(strings.TrimSpace(c.transport.AppBotLogin())))
		c.warn(msg, args...)
		return
	}
	if state.postedForHead || !(hgithub.ProwManagedLabels(labels) || state.prowManaged) {
		return
	}
	if c.prowPresubmitStatusPresent(ctx, displayRepo, owner, repo, number, headSHA) {
		return
	}
	c.postWorkflowApprovalCommand(ctx, displayRepo, owner, repo, number, headSHA)
}

// prowPresubmitStatusPresent reports whether Prow already reported a job on
// headSHA: a commit status other than Tide's own, created by the account that
// created Tide's status. Without a Tide status nothing on the head can be
// attributed to Prow, so it reports false. A lookup failure reports true so
// nothing is posted on evidence that could not be read; the next comment scan
// tries again.
func (c *Engine) prowPresubmitStatusPresent(ctx context.Context, displayRepo, owner, repo string, number int, headSHA string) bool {
	var statuses []*gh.RepoStatus
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.Repositories.ListStatuses(hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep"), owner, repo, headSHA, opts)
		if err != nil {
			c.warn("contributor-lane could not list commit statuses for Prow presubmits", "repo", displayRepo, "pr", number, "sha", headSHA, "error", err)
			return true
		}
		statuses = append(statuses, page...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	prow := ""
	for _, st := range statuses {
		if st != nil && strings.EqualFold(strings.TrimSpace(st.GetContext()), prowTideContext) {
			prow = hgithub.SafeGetLogin(st.GetCreator())
			break
		}
	}
	if prow == "" {
		return false
	}
	for _, st := range statuses {
		if st == nil || strings.EqualFold(strings.TrimSpace(st.GetContext()), prowTideContext) {
			continue
		}
		if strings.EqualFold(hgithub.SafeGetLogin(st.GetCreator()), prow) {
			return true
		}
	}
	return false
}

// prowPresubmitsRejectedNote is the one needs-human comment posted when Prow
// refuses the App's `/ok-to-test` on a non-member fork PR.
func prowPresubmitsRejectedNote(appLogin string) string {
	if appLogin == "" {
		appLogin = "the Hive App"
	}
	var b strings.Builder
	b.WriteString(workflowApprovalRejectedMarker)
	b.WriteString("\nneeds-human: Prow has not run this fork PR's presubmits. Its author is not one of Prow's trusted users, so the trigger plugin waits for `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("`; Hive posted it and Prow rejected `")
	b.WriteString(appLogin)
	b.WriteString("` (\"")
	b.WriteString(prowOkToTestRejection)
	b.WriteString("\"). Hive will not post the command again on this PR.\n\nA maintainer can comment `")
	b.WriteString(workflowApprovalCommand)
	b.WriteString("` (plus `/test all` for the current head), or make `")
	b.WriteString(appLogin)
	b.WriteString("` a Prow trusted user so Hive's command counts.")
	return b.String()
}

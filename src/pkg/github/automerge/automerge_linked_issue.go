package automerge

// Linked-issue gate. A maintainer parks work on the ISSUE, not on each PR
// that closes it, yet every lane decided from the PR's own labels: a PR
// whose body closes a held issue merged anyway (projectbluefin/utah#874,
// "Fixes #551", while #551 waited on a maintainer's test). Each lane now
// reads every open same-repo issue the PR closes, once the head is
// otherwise ready to merge, and refuses while one is held or parked on a
// person. No state is kept: removing the label lets the next sweep merge.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	linkedIssueReasonCheck               = prstage.LinkedIssueReasonCheck
	linkedIssueReasonHoldPrefix          = prstage.LinkedIssueReasonHoldPrefix
	linkedIssueReasonNeedsDecisionPrefix = prstage.LinkedIssueReasonNeedsDecisionPrefix
)

// linkedIssueParkLabels park an issue on a person's decision: the issue
// parking stage, its retired spellings (still read as the decision bucket
// until migrated) and needs-human.
var linkedIssueParkLabels = append([]string{
	config.DefaultNeedsDecisionLabel,
	config.PRNeedsDecisionStageLabel,
	config.DefaultNeedsHumanLabel,
}, config.LegacyNeedsDecisionLabels...)

// linkedIssueBlock returns the reason kind prefix and the label that blocks
// a PR closing an issue with these labels, or "" when none does. The hold
// family is the engine's own predicate (configured hold substrings and the
// hive's `hive-pause/<id>`) plus prow's do-not-merge(/*), which the issue
// stage engine also reads as a hold; it outranks a park, as on the issue.
func (c *Engine) linkedIssueBlock(labels []string) (string, string) {
	for _, label := range labels {
		lower := strings.ToLower(strings.TrimSpace(label))
		if lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/") || c.isHeld([]string{label}) {
			return linkedIssueReasonHoldPrefix, strings.TrimSpace(label)
		}
	}
	for _, label := range labels {
		if hasLabelFold(linkedIssueParkLabels, label) {
			return linkedIssueReasonNeedsDecisionPrefix, strings.TrimSpace(label)
		}
	}
	return "", ""
}

// linkedIssueGate refuses a merge while an open issue in the PR's own repo
// that the PR body closes is held or parked; the reason
// (`linked-issue:hold:#N:<label>` / `linked-issue:needs-decision:#N:<label>`)
// routes the PR to 085 naming the issue and label. It then runs
// closingKeywordPRGate: a closing keyword that names an open pull request
// refuses with `closing-keyword-targets-pr:#N`. Closed issues, closed pull
// requests and other repositories' issues never hold the PR, nor does a
// number GitHub reports missing (404/410). Any other read error fails closed with
// linkedIssueReasonCheck: no merge this tick. Lanes call it only for a
// head that would otherwise merge, so it costs one issue GET per linked
// issue per merge-ready PR.
func (c *Engine) linkedIssueGate(ctx context.Context, owner, repo string, pr *gh.PullRequest) (string, error) {
	fullRepo := owner + "/" + repo
	caller := hgithub.WithRESTCaller(ctx, "hive:automerge_linked_issue")
	readThisTick := map[int]bool{}
	for _, n := range contributorLaneLinkedIssues(pr.GetBody(), fullRepo) {
		issue, _, err := c.gh.Issues.Get(caller, owner, repo, n)
		if err != nil {
			if isGitHubStatus(err, http.StatusNotFound) || isGitHubStatus(err, http.StatusGone) {
				c.noteCreditIssue(owner, repo, n, nil)
				c.noteClosingTarget(owner, repo, n, nil)
				readThisTick[n] = false
				continue
			}
			return linkedIssueReasonCheck, fmt.Errorf("reading linked issue %s#%d: %w", fullRepo, n, err)
		}
		c.noteCreditIssue(owner, repo, n, issue)
		c.noteClosingTarget(owner, repo, n, issue)
		readThisTick[n] = isOpenPullRequest(issue)
		if issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "open") {
			continue
		}
		if kind, label := c.linkedIssueBlock(labelNames(issue.Labels)); kind != "" {
			reason := prstage.LinkedIssueReason(kind, n, label)
			c.info("automerge: linked issue blocks merge", "repo", fullRepo, "pr", pr.GetNumber(), "issue", n, "label", label, "reason", reason)
			return reason, nil
		}
	}
	// A closing keyword that names an open pull request (in the body, or in
	// a commit message on the head) would close that PR on merge
	// (hivecommons/hive#15025); the issues read above are not read again.
	return c.closingKeywordPRGate(ctx, owner, repo, pr, readThisTick)
}

// noteCreditIssue hands an issue this gate read (nil for one GitHub reported
// missing) to the transport's merge creditor, so the Co-authored-by trailer
// (hivecommons/hive#14581) costs no second GET for the same issue.
func (c *Engine) noteCreditIssue(owner, repo string, n int, issue *gh.Issue) {
	if mt, ok := c.transport.(mergeCreditTransport); ok {
		mt.MergeCreditor().NoteIssue(owner, repo, n, issue)
	}
}

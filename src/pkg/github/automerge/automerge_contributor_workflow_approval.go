package automerge

// Owner approval releases the workflow-path fence (hivecommons/hive#14664).
//
// The contributor lane's diff fence refuses any PR that edits
// `.github/workflows/**` or `.github/actions/**` (contributorLaneDiffReason).
// Until #14664 that refusal was absolute: an owner's approval only moved the
// PR to stage/090 with a "click Merge" caption and the hive never merged it
// (hive#14568, hive#14498, rationguard#401). The owner's decision is that an
// explicitly approved workflow-path PR is the hive's to merge. "Explicit
// approval" is one of:
//
//   - a GitHub pull-request review in state APPROVED whose commit_id is the
//     current head, by a trusted merger (isTrustedMerger — the same bar
//     `/hive decision` and the requires_human release use);
//   - a dashboard approval (`/hive decision approve`, which posts `/lgtm`
//     and `/approve` attributed to the maintainer — ParsePRApprovalCommandComment)
//     or a person's own `/hive decision approve` comment, by a trusted
//     merger, bound to the current head: the comment's head marker names
//     it, or the comment was posted after the head was pushed.
//
// Approval on an older head never counts, so a new push re-arms the fence.
// A released fence is logged once per head and leaves one audit marker per
// head on the PR; the PR then goes through every other gate (CI, soak,
// holds, sentinel, verdict) exactly like any other contributor-lane PR.
//
// Merging a workflow-path PR needs the App's `workflows: write`. When the
// merge is refused for want of it, the lane falls back to the pre-#14664
// behaviour for that head: hive:unauthorized (step merge-workflow-path) and
// the workflow-path reason, which the stage engine already routes to 090
// "click Merge" for an approved head. The in-memory unauthorized mark and
// the durable marker comment keep the lane from retrying every tick.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
)

const (
	// workflowApprovalMarkerPrefix opens the one-per-head audit comment the
	// lane leaves when owner approval releases the fence.
	workflowApprovalMarkerPrefix = "<!-- hive:workflow-approval"
	// unauthorizedStepMergeWorkflowPath names the blocked merge step.
	unauthorizedStepMergeWorkflowPath = prstage.UnauthorizedStepMergeWorkflowPath
	// unauthorizedPermissionWorkflowsWrite is the App permission the merge
	// of a workflow-path PR needs.
	unauthorizedPermissionWorkflowsWrite = "workflows: write"
	// workflowApprovalDecisionCommand is the person-typed form of the
	// dashboard approval, read off a comment's first line.
	workflowApprovalDecisionCommand = "/hive decision approve"
	// workflowApprovalRESTCaller tags the audit comment write.
	workflowApprovalRESTCaller = "hive:workflow_fence_release"
)

// workflowFenceReleased decides whether the diff fence that tripped with
// reason may be released for this head. Only the workflow-path fence is
// releasable, only when the policy toggle is on, and only while the lane has
// not already learnt that the App cannot merge this head (an unauthorized
// mark for merge-workflow-path, in memory or as a marker comment). It
// returns the approver and true when released; the caller then skips the
// fence. Read failures release nothing: the fence stays as it was.
func (c *Engine) workflowFenceReleased(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int, headSHA, reason string, policy ContributorLanePolicy) (string, bool) {
	if c == nil || c.gh == nil || reason != contributorLaneReasonWorkflowPath || !policy.OwnerApprovalReleasesWorkflowFence || strings.TrimSpace(headSHA) == "" {
		return "", false
	}
	if c.unauthorizedStep(owner, repo, number, headSHA) == unauthorizedStepMergeWorkflowPath {
		return "", false
	}
	comments, err := c.listIssueComments(hgithub.WithRESTCaller(ctx, workflowApprovalRESTCaller), owner, repo, number)
	if err != nil {
		c.warn("workflow-path fence: could not read comments", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
		return "", false
	}
	approver, ok := c.workflowFenceApproval(ctx, owner, repo, pr, number, headSHA, comments)
	if !ok {
		return "", false
	}
	if hasCommentWithPrefix(comments, unauthorizedMarker(headSHA, unauthorizedStepMergeWorkflowPath)) {
		// A previous process already learnt the App cannot merge this head;
		// re-arm the in-memory mark so the reason flips without another
		// merge attempt.
		c.markUnauthorized(ctx, displayRepo, owner, repo, pr, number, headSHA, unauthorizedStepMergeWorkflowPath, unauthorizedPermissionWorkflowsWrite, mergeWorkflowPathHumanAction())
		return "", false
	}
	if !hasCommentWithPrefix(comments, workflowApprovalMarker(headSHA)) {
		c.info("workflow-path fence released by owner approval", "repo", displayRepo, "pr", number, "login", approver, "head", headSHA)
		if err := c.commentWorkflowApproval(ctx, owner, repo, number, headSHA, approver); err != nil {
			c.warn("workflow-path fence: could not post audit comment", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
		}
	}
	return approver, true
}

// workflowFenceApproval finds the trusted merger whose explicit approval is
// bound to headSHA: an APPROVED review on the head first, then an approval
// comment (dashboard-posted or typed) among comments bound to it.
func (c *Engine) workflowFenceApproval(ctx context.Context, owner, repo string, pr *gh.PullRequest, number int, headSHA string, comments []*gh.IssueComment) (string, bool) {
	if login, ok := c.latestTrustedHumanApproval(ctx, owner, repo, number, headSHA); ok {
		return login, true
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	for i := len(comments) - 1; i >= 0; i-- {
		login, ok := workflowApprovalCommentActor(comments[i], headSHA, headPushedAt, appLogin)
		if !ok {
			continue
		}
		if trusted, configured := c.isTrustedMerger(login); configured && trusted {
			c.noteStageTrustedHumanApproval(owner, repo, number, headSHA, login)
			return login, true
		}
	}
	return "", false
}

// workflowApprovalCommentActor reads one comment as an explicit approval
// bound to headSHA and returns the login it is attributed to. A dashboard
// `/approve` posted by the App counts for the maintainer it names; a
// person's `/hive decision approve` counts for its author. Either binds to
// the head through its marker, or — for comments without one — by having
// been posted after the head (headPushedAt, when known).
func workflowApprovalCommentActor(comment *gh.IssueComment, headSHA string, headPushedAt time.Time, appLogin string) (string, bool) {
	if comment == nil {
		return "", false
	}
	body := comment.GetBody()
	author := strings.TrimSpace(hgithub.SafeGetLogin(comment.GetUser()))
	boundToHead := func(markerHead string) bool {
		if markerHead != "" {
			return strings.EqualFold(markerHead, headSHA)
		}
		created := comment.GetCreatedAt().Time
		return !headPushedAt.IsZero() && !created.IsZero() && !created.Before(headPushedAt)
	}
	if command, actor, head, ok := hgithub.ParsePRApprovalCommandComment(body); ok {
		if command != "/approve" || appLogin == "" || !strings.EqualFold(author, appLogin) {
			return "", false
		}
		if !boundToHead(head) {
			return "", false
		}
		return actor, true
	}
	if !hgithub.IsPersonLogin(comment.GetUser(), appLogin) {
		return "", false
	}
	line := strings.ToLower(strings.Join(strings.Fields(firstCommentLine(body)), " "))
	if line != workflowApprovalDecisionCommand || !boundToHead("") {
		return "", false
	}
	return author, true
}

// firstCommentLine returns the first non-empty line of body, trimmed; a
// quoted or fenced first line yields "" so a quoted command never counts.
func firstCommentLine(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ">") || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			return ""
		}
		return line
	}
	return ""
}

// hasCommentWithPrefix reports whether any comment body starts with prefix.
func hasCommentWithPrefix(comments []*gh.IssueComment, prefix string) bool {
	for _, comment := range comments {
		if comment != nil && strings.HasPrefix(comment.GetBody(), prefix) {
			return true
		}
	}
	return false
}

// workflowApprovalMarker is the first line of the audit comment for a head.
func workflowApprovalMarker(headSHA string) string {
	return workflowApprovalMarkerPrefix + " head=" + strings.ToLower(headSHA)
}

// workflowApprovalComment is the audit comment body.
func workflowApprovalComment(headSHA, approver string) string {
	return workflowApprovalMarker(headSHA) + " by=" + strings.ToLower(approver) + " -->\n" +
		"@" + approver + " approved head `" + shortSHA(headSHA) + "`, which edits CI workflow / action files. That explicit owner approval releases the contributor lane's workflow-path fence: the hive merges this PR itself once the usual gates (CI, soak, holds, review verdict) are clear. A new push re-arms the fence until the new head is approved again."
}

// commentWorkflowApproval posts the audit comment once per head: an existing
// marker on the PR, or a prior effects claim, suppresses it.
func (c *Engine) commentWorkflowApproval(ctx context.Context, owner, repo string, number int, headSHA, approver string) error {
	marker := workflowApprovalMarker(headSHA)
	body := workflowApprovalComment(headSHA, approver)
	_, err := effects.Execute(hgithub.WithRESTCaller(ctx, workflowApprovalRESTCaller), c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(marker)},
	}, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		c.noteOwnStageComment(owner, repo, number)
		return effects.Result{Provenance: comment.GetHTMLURL()}, nil
	})
	return err
}

// workflowFenceMergeUnauthorized handles a merge failure on a PR whose
// workflow-path fence owner approval released. When GitHub refused the merge
// for want of the App's `workflows` permission, it marks the PR unauthorized
// for merge-workflow-path and returns the workflow-path reason — the
// pre-#14664 outcome, which the stage engine routes to 090 "click Merge" for
// an approved head. Any other failure, or a PR whose fence did not trip,
// returns "" so the caller's usual handling runs.
func (c *Engine) workflowFenceMergeUnauthorized(ctx context.Context, displayRepo, owner, repo string, eval contributorLaneEval, err error) string {
	if eval.workflowApprover == "" || !isWorkflowsPermissionMergeError(err) {
		return ""
	}
	c.warn("workflow-path merge refused: App lacks workflows permission", "repo", displayRepo, "pr", eval.pr.GetNumber(), "head", eval.headSHA, "approver", eval.workflowApprover, "error", err)
	c.markUnauthorized(ctx, displayRepo, owner, repo, eval.pr, eval.pr.GetNumber(), eval.headSHA, unauthorizedStepMergeWorkflowPath, unauthorizedPermissionWorkflowsWrite, mergeWorkflowPathHumanAction())
	return contributorLaneReasonWorkflowPath
}

// mergeWorkflowPathHumanAction is the human step named in the
// hive:unauthorized comment for a refused workflow-path merge.
func mergeWorkflowPathHumanAction() string {
	return "A maintainer must click **Merge** on GitHub (the approval already stands), or grant the App `" + unauthorizedPermissionWorkflowsWrite + "` so the hive can merge approved workflow-path PRs itself."
}

// isWorkflowsPermissionMergeError reports whether GitHub refused a merge
// because the App installation lacks the `workflows` permission: the
// generic 403 "Resource not accessible by integration", or a 403 / 422
// whose message names the workflow permission (`refusing to allow a GitHub
// App to create or update workflow … without workflows permission`).
func isWorkflowsPermissionMergeError(err error) bool {
	if err == nil {
		return false
	}
	if isResourceNotAccessible(err) {
		return true
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	if code := ghErr.Response.StatusCode; code != http.StatusForbidden && code != http.StatusUnprocessableEntity {
		return false
	}
	msg := strings.ToLower(ghErr.Message)
	if msg == "" {
		msg = strings.ToLower(ghErr.Error())
	}
	return strings.Contains(msg, "workflow") && strings.Contains(msg, "permission")
}

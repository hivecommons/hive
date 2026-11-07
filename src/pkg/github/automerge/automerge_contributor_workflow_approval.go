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
//     it, or the comment was posted after the head was first observed
//     (headObservedAt — the pinned server-side updated_at, never the
//     commit's own committer date, which the pusher chooses).
//
// Approval on an older head never counts, so a new push re-arms the fence —
// except a head the hive's own base refresh produced: an App-authored merge
// of the base branch that leaves the PR's three-dot diff file-for-file what
// the person approved carries that approval forward
// (workflowFenceCarriedApproval), so a strict up-to-date rule does not cost
// a re-approval every time the base moves.
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
	"sort"
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
	grant, ok := c.workflowFenceApproval(ctx, owner, repo, pr, number, headSHA, comments)
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
		if grant.carriedFrom != "" {
			c.info("workflow-path fence approval carried over hive base refresh", "repo", displayRepo, "pr", number, "login", grant.login, "approved_head", grant.carriedFrom, "head", headSHA)
		} else {
			c.info("workflow-path fence released by owner approval", "repo", displayRepo, "pr", number, "login", grant.login, "head", headSHA)
		}
		if err := c.commentWorkflowApproval(ctx, owner, repo, number, headSHA, grant); err != nil {
			c.warn("workflow-path fence: could not post audit comment", "repo", displayRepo, "pr", number, "head", headSHA, "error", err)
		}
	}
	return grant.login, true
}

// workflowFenceGrant is the trusted merger whose explicit approval releases
// the workflow-path fence for a head.
type workflowFenceGrant struct {
	login string
	// carriedFrom is the earlier head the approval was given on when the
	// hive's own base refresh is all that moved the PR to the current head;
	// "" when the approval is bound to the current head itself.
	carriedFrom string
}

// workflowFenceApproval finds the trusted merger whose explicit approval is
// bound to headSHA: an APPROVED review on the head first, then an approval
// comment (dashboard-posted or typed) among comments bound to it, and last
// an approval on an earlier head carried over the hive's own base refresh.
func (c *Engine) workflowFenceApproval(ctx context.Context, owner, repo string, pr *gh.PullRequest, number int, headSHA string, comments []*gh.IssueComment) (workflowFenceGrant, bool) {
	reviews := c.standingTrustedHumanReviews(ctx, owner, repo, number)
	if rv := newestReviewOnHead(reviews, "APPROVED", headSHA); rv != nil {
		login := strings.TrimSpace(hgithub.SafeGetLogin(rv.GetUser()))
		c.noteStageTrustedHumanApproval(owner, repo, number, headSHA, login)
		return workflowFenceGrant{login: login}, true
	}
	headPushedAt := c.headObservedAt(owner, repo, number, headSHA, pr)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	for i := len(comments) - 1; i >= 0; i-- {
		login, ok := workflowApprovalCommentActor(comments[i], headSHA, headPushedAt, appLogin)
		if !ok {
			continue
		}
		if trusted, configured := c.isTrustedMerger(login); configured && trusted {
			c.noteStageTrustedHumanApproval(owner, repo, number, headSHA, login)
			return workflowFenceGrant{login: login}, true
		}
	}
	return c.workflowFenceCarriedApproval(ctx, owner, repo, pr, number, headSHA, appLogin, reviews, comments)
}

// workflowFenceCarriedApproval carries an approval given on an earlier head
// to headSHA when the hive's own base refresh is all that moved the PR there
// (BaseRefreshChecker, hivecommons/hive#15649: GitHub-verified App merges of
// the base branch whose three-dot diff against the base is file-for-file the
// approved head's, at most a few hops back). The workflow change is then
// exactly what the person approved, so under a strict up-to-date branch rule
// the update-branch the lane itself performs no longer re-arms the fence and
// waits for a fresh approval every time the base moves (projectbluefin/utah
// #1111). The approver must still be a trusted merger. Any other head
// change — a contributor commit, even one leaving the workflow files alone;
// a rebase; a base merge by anyone but the App; a base merge that changed a
// file the PR also changes — carries nothing and the fence stays armed.
func (c *Engine) workflowFenceCarriedApproval(ctx context.Context, owner, repo string, pr *gh.PullRequest, number int, headSHA, appLogin string, reviews []*gh.PullRequestReview, comments []*gh.IssueComment) (workflowFenceGrant, bool) {
	baseRef := strings.TrimSpace(pr.GetBase().GetRef())
	if c.gh == nil || appLogin == "" || baseRef == "" {
		return workflowFenceGrant{}, false
	}
	for _, prior := range workflowFencePriorApprovals(headSHA, appLogin, reviews, comments) {
		if trusted, configured := c.isTrustedMerger(prior.login); !configured || !trusted {
			continue
		}
		if !c.baseRefresh.HeadIsBaseRefreshOf(ctx, c.gh.Repositories, owner, repo, number, baseRef, prior.carriedFrom, headSHA, appLogin) {
			continue
		}
		c.noteStageTrustedHumanApproval(owner, repo, number, headSHA, prior.login)
		return prior, true
	}
	return workflowFenceGrant{}, false
}

// workflowFencePriorApprovals lists the explicit approvals bound to heads
// other than headSHA, one per head, newest first: the hive's own audit
// marker for a head it released (which also records a typed or unmarked
// approval the comment itself cannot be bound to later), a dashboard
// `/approve` marked for a head, then each trusted merger's standing APPROVED
// review. Only App-authored, unedited comments are read, so nobody else can
// forge an earlier approval.
func workflowFencePriorApprovals(headSHA, appLogin string, reviews []*gh.PullRequestReview, comments []*gh.IssueComment) []workflowFenceGrant {
	var out []workflowFenceGrant
	seen := map[string]bool{strings.ToLower(strings.TrimSpace(headSHA)): true}
	add := func(head, login string) {
		head, login = strings.ToLower(strings.TrimSpace(head)), strings.TrimSpace(login)
		if head == "" || login == "" || seen[head] {
			return
		}
		seen[head] = true
		out = append(out, workflowFenceGrant{login: login, carriedFrom: head})
	}
	for i := len(comments) - 1; i >= 0; i-- {
		comment := comments[i]
		if comment == nil || appLogin == "" || !strings.EqualFold(strings.TrimSpace(hgithub.SafeGetLogin(comment.GetUser())), appLogin) {
			continue
		}
		if comment.GetUpdatedAt().Time.After(comment.GetCreatedAt().Time) {
			continue
		}
		body := comment.GetBody()
		if head, login, ok := parseWorkflowApprovalMarker(body); ok {
			add(head, login)
			continue
		}
		if command, actor, head, ok := hgithub.ParsePRApprovalCommandComment(body); ok && command == "/approve" {
			add(head, actor)
		}
	}
	approvals := make([]*gh.PullRequestReview, 0, len(reviews))
	for _, rv := range reviews {
		if rv != nil && strings.EqualFold(rv.GetState(), "APPROVED") {
			approvals = append(approvals, rv)
		}
	}
	sort.SliceStable(approvals, func(i, j int) bool {
		return approvals[i].GetSubmittedAt().After(approvals[j].GetSubmittedAt().Time)
	})
	for _, rv := range approvals {
		add(rv.GetCommitID(), hgithub.SafeGetLogin(rv.GetUser()))
	}
	return out
}

// parseWorkflowApprovalMarker reads the head and approver off the first line
// of an audit comment (workflowApprovalComment).
func parseWorkflowApprovalMarker(body string) (head, approver string, ok bool) {
	line, _, _ := strings.Cut(body, "\n")
	rest, found := strings.CutPrefix(strings.TrimSpace(line), workflowApprovalMarkerPrefix+" ")
	if !found {
		return "", "", false
	}
	rest, _, _ = strings.Cut(rest, "-->")
	for _, field := range strings.Fields(rest) {
		if v, isHead := strings.CutPrefix(field, "head="); isHead {
			head = v
		} else if v, isBy := strings.CutPrefix(field, "by="); isBy {
			approver = v
		}
	}
	if head == "" || approver == "" {
		return "", "", false
	}
	return head, approver, true
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
	// An edit to an existing comment is not a new instruction.
	if comment.GetUpdatedAt().Time.After(comment.GetCreatedAt().Time) {
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

// workflowApprovalComment is the audit comment body. A carried approval
// names the head it was given on (from=) and says why it still stands.
func workflowApprovalComment(headSHA string, grant workflowFenceGrant) string {
	marker := workflowApprovalMarker(headSHA) + " by=" + strings.ToLower(grant.login)
	if grant.carriedFrom != "" {
		return marker + " from=" + strings.ToLower(grant.carriedFrom) + " -->\n" +
			"@" + grant.login + " approved head `" + shortSHA(grant.carriedFrom) + "`, which edits CI workflow / action files. Head `" + shortSHA(headSHA) + "` is only the hive's own merge of the base branch into it — the PR's diff is unchanged — so that approval still releases the contributor lane's workflow-path fence. Any other push re-arms the fence until the new head is approved again."
	}
	return marker + " -->\n" +
		"@" + grant.login + " approved head `" + shortSHA(headSHA) + "`, which edits CI workflow / action files. That explicit owner approval releases the contributor lane's workflow-path fence: the hive merges this PR itself once the usual gates (CI, soak, holds, review verdict) are clear. A new push re-arms the fence until the new head is approved again; the hive's own merge of the base branch, which leaves the diff unchanged, does not."
}

// commentWorkflowApproval posts the audit comment once per head: an existing
// marker on the PR, or a prior effects claim, suppresses it.
func (c *Engine) commentWorkflowApproval(ctx context.Context, owner, repo string, number int, headSHA string, grant workflowFenceGrant) error {
	marker := workflowApprovalMarker(headSHA)
	body := workflowApprovalComment(headSHA, grant)
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

package automerge

// Owner approval releases the human-merge-path hold (opt-in, per repository).
//
// auto_merge.human_merge_paths makes every App merge lane refuse a pull
// request that touches a listed path: the lane adds `hold`, posts one marker
// comment and leaves the merge to a person. On a repository listed in
// auto_merge.human_merge_owner_approval a trusted merger's explicit approval
// of the PR's current head lifts that refusal instead, and the lane merges
// the PR once CI, soak, review and every other gate pass
// (Danathar/goodreads-mcp#463: the owner approved the head, GitHub required
// no review and reported the PR clean, and the hive still answered that a
// person must merge it).
//
// "Explicit approval" is the workflow-path fence's (workflowFenceApproval: a
// trusted merger's GitHub Approve review on the head or carried over the
// hive's own base refresh, a dashboard approval, or a typed
// `/hive decision approve`), plus a trusted merger's `/hive approve` reply
// posted after the head was first seen, bound to the head the same way a
// typed decision is (humanMergeApproveCommandActor). A new push needs a new
// approval. The PR author's own approval never counts
// (humanMergeSelfApproval). A repository not listed keeps the rule absolute.
//
// The release reuses the human-merge-path hold's provenance check
// (hgithub.ReleaseApprovedHumanMergePathHold): only a `hold` the App put on
// for this refusal is ever removed, never one a person added or re-added,
// and never while another hold source explains it. The gate itself
// (humanMergePathGate) then lets the approved head through instead of
// holding it again. One audit comment per head names the approver.

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// humanMergeOwnerApprovalConfigKey is the config key named in log lines and
// replies.
const humanMergeOwnerApprovalConfigKey = "auto_merge.human_merge_owner_approval"

// humanMergeApprovalMarkerPrefix opens the one-per-head audit comment left
// when an owner's approval releases the human-merge-path refusal.
const humanMergeApprovalMarkerPrefix = "<!-- hive:human-merge-approval "

func humanMergeApprovalMarker(head string) string {
	return humanMergeApprovalMarkerPrefix + strings.ToLower(strings.TrimSpace(head)) + " -->"
}

// humanMergeOwnerApprovalEnabled reports whether repo (owner/repo) is listed
// in auto_merge.human_merge_owner_approval.
func (c *Engine) humanMergeOwnerApprovalEnabled(repo string) bool {
	if c == nil || c.humanMergeOwnerApproval == nil {
		return false
	}
	return c.humanMergeOwnerApproval(repo)
}

// humanMergeOwnerApprovalFor returns the trusted merger's explicit approval
// of pr's current head on a repository that lets one release the
// human-merge-path refusal, or false. comments are the PR's comments, oldest
// first. Read failures and an unlisted repository answer false: the refusal
// stays.
func (c *Engine) humanMergeOwnerApprovalFor(ctx context.Context, owner, repo string, pr *gh.PullRequest, comments []*gh.IssueComment) (workflowApproval, bool) {
	if c == nil || c.gh == nil || pr == nil || !c.humanMergeOwnerApprovalEnabled(owner+"/"+repo) {
		return workflowApproval{}, false
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return workflowApproval{}, false
	}
	number := pr.GetNumber()
	author := strings.TrimSpace(hgithub.SafeGetLogin(pr.GetUser()))
	if approval, ok := c.workflowFenceApproval(ctx, owner, repo, pr, number, head, comments); ok && !humanMergeSelfApproval(approval.Login, author) {
		return approval, true
	}
	headPushedAt := c.headObservedAt(owner, repo, number, head, pr)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	for i := len(comments) - 1; i >= 0; i-- {
		login, ok := humanMergeApproveCommandActor(comments[i], headPushedAt, appLogin)
		if !ok || humanMergeSelfApproval(login, author) {
			continue
		}
		if trusted, configured := c.isTrustedMerger(login); !configured || !trusted {
			continue
		}
		c.noteStageTrustedHumanApproval(owner, repo, number, head, login)
		return workflowApproval{Login: login}, true
	}
	return workflowApproval{}, false
}

// humanMergeSelfApproval reports whether approver is the PR's own author. The
// author's approval never releases the human-merge-path refusal: the hold
// exists so a second person looks at the change, and a trusted merger who
// opened the PR cannot be that person (GitHub already refuses an author's
// Approve review; a typed `/hive approve` or `/hive decision approve` would
// otherwise let one account stand in for two).
func humanMergeSelfApproval(approver, author string) bool {
	approver, author = strings.TrimSpace(approver), strings.TrimSpace(author)
	return approver != "" && author != "" && strings.EqualFold(approver, author)
}

// humanMergeApproveCommandActor reads one comment as a person's
// `/hive approve` of the head first seen at headPushedAt and returns its
// author: the command on the first line, the comment unedited (collaborators
// can edit others' comments) and posted after the head. With no known head
// time nothing binds, so nothing counts.
func humanMergeApproveCommandActor(comment *gh.IssueComment, headPushedAt time.Time, appLogin string) (string, bool) {
	if comment == nil || headPushedAt.IsZero() {
		return "", false
	}
	created, updated := comment.GetCreatedAt().Time, comment.GetUpdatedAt().Time
	if created.IsZero() || updated.After(created) || created.Before(headPushedAt) {
		return "", false
	}
	if !hgithub.IsPersonLogin(comment.GetUser(), appLogin) {
		return "", false
	}
	line := strings.ToLower(strings.Join(strings.Fields(firstCommentLine(comment.GetBody())), " "))
	if line != hgithub.ReviewHoldReleaseCommand {
		return "", false
	}
	return strings.TrimSpace(hgithub.SafeGetLogin(comment.GetUser())), true
}

// humanMergeOwnerApprovalGate is humanMergePathGate's owner-approval
// exception: true when the repository lets an owner's approval release the
// refusal and pr's current head carries one. It records the release once
// per head (log line and audit comment). A comment read failure answers
// false, so the gate holds as before.
func (c *Engine) humanMergeOwnerApprovalGate(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, matches []string) bool {
	if !c.humanMergeOwnerApprovalEnabled(owner + "/" + repo) {
		return false
	}
	number := pr.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		c.warn("human-merge-path owner approval: could not read comments; hold kept", "repo", displayRepo, "pr", number, "error", err)
		return false
	}
	approval, ok := c.humanMergeOwnerApprovalFor(ctx, owner, repo, pr, comments)
	if !ok {
		return false
	}
	head := pr.GetHead().GetSHA()
	if !hasCommentWithPrefix(comments, humanMergeApprovalMarker(head)) {
		c.info("automerge sweep: owner approval releases human-merge paths", "repo", displayRepo, "pr", number, "paths", matches, "login", approval.Login, "head", head, "config", humanMergeOwnerApprovalConfigKey)
		appBot := strings.TrimSpace(c.transport.AppBotLogin())
		author := hgithub.SafeGetLogin(pr.GetUser())
		item := writeback.Item{Number: number, IsPR: true, Author: author, Labels: labelNames(pr.Labels), CreatedAt: pr.GetCreatedAt().Time,
			HiveAuthored: appBot != "" && strings.EqualFold(normalizeBotLogin(author), normalizeBotLogin(appBot))}
		c.commentHumanMergeApproval(ctx, owner+"/"+repo, owner, repo, number, head, approval, item)
	}
	return true
}

// releaseApprovedHumanMergePathHold is the stage pass's half: on a repository
// listed in auto_merge.human_merge_owner_approval it removes the `hold` the
// hive put on for the human-merge-path refusal once a trusted merger
// explicitly approved the PR's current head, so the lanes reach the gate,
// which lets the approved head through. It never removes a hold another
// source explains (a person's, a pause, the reporter-trust gate), and the
// label removal itself re-checks the hold's provenance
// (hgithub.ReleaseApprovedHumanMergePathHold). Anything it cannot establish
// keeps the hold.
func (c *Engine) releaseApprovedHumanMergePathHold(ctx context.Context, obs stageObservation, prov *stagelabels.HoldProvenance) (bool, error) {
	if c == nil || c.gh == nil || c.transport == nil || !prov.SelfImposed() || !prov.HumanMergePath() {
		return false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	head := strings.TrimSpace(obs.HeadSHA)
	if head == "" || !c.humanMergeOwnerApprovalEnabled(repo) {
		return false, nil
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, fmt.Errorf("reading PR: %w", err)
	}
	if pr.GetHead().GetSHA() != head {
		// The pass observed an older head; the next pass judges this one.
		return false, nil
	}
	comments, err := c.listAllIssueComments(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return false, fmt.Errorf("listing comments: %w", err)
	}
	approval, ok := c.humanMergeOwnerApprovalFor(ctx, obs.Owner, obs.Repo, pr, comments)
	if !ok {
		return false, nil
	}
	others, ok := c.transport.(otherHoldSourcesTransport)
	if !ok {
		return false, nil
	}
	if sources := others.OtherHoldSourcesForPR(repo, obs.Number, obs.Labels, comments); len(sources) > 0 {
		c.info("stage labels: human-merge-path hold approved but another hold source keeps it", "repo", obs.Display, "pr", obs.Number, "head", head, "login", approval.Login, "sources", strings.Join(sources, ","))
		return false, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	allowed := func(item writeback.Item) bool {
		d := c.writebackAllowed(repo, item, writeback.KindHumanMergePathRelease, "")
		if !d.Allowed {
			c.info("stage labels: human-merge-path hold approved; release refused by the write-back gate", "repo", obs.Display, "pr", obs.Number, "head", head, "reason", d.Reason)
		}
		return d.Allowed
	}
	released, err := hgithub.ReleaseApprovedHumanMergePathHold(ctx, c.gh, allowed, obs.Owner, obs.Repo, obs.Number, appBot, head)
	if err != nil || !released {
		return false, err
	}
	c.info("stage labels: owner approval released human-merge-path hold", "repo", obs.Display, "pr", obs.Number, "head", head, "login", approval.Login, "config", humanMergeOwnerApprovalConfigKey)
	if !hasCommentWithPrefix(comments, humanMergeApprovalMarker(head)) {
		item := writebackItemForObservation(obs)
		item.HiveAuthored = strings.EqualFold(normalizeBotLogin(obs.Author), normalizeBotLogin(appBot))
		c.commentHumanMergeApproval(ctx, repo, obs.Owner, obs.Repo, obs.Number, head, approval, item)
	}
	return true, nil
}

// humanMergeApprovalComment is the audit comment's text: who released the
// refusal for which head, and that every other gate still applies.
func humanMergeApprovalComment(head string, approval workflowApproval) string {
	short := "`" + stagelabels.ShortSHA(head) + "`"
	var b strings.Builder
	b.WriteString(humanMergeApprovalMarker(head) + "\n")
	fmt.Fprintf(&b, "Hold released for %s: `%s` approved this head, and `%s` lets a trusted merger's approval release this repository's human-merge paths (`%s`). ", short, approval.Login, humanMergeOwnerApprovalConfigKey, hgithub.HumanMergePathsConfigKey)
	b.WriteString("The hive merges this PR once CI, review, soak and every other merge gate are clear; the approval is not a merge override. ")
	b.WriteString("A new push puts the hold back until that head is approved, except the hive's own update from the base branch, which leaves the change as approved. A `hold` a person adds is never lifted this way.\n")
	return b.String()
}

// commentHumanMergeApproval posts the audit comment through the write-back
// gate; a refusal or a failed write is logged and nothing else changes.
func (c *Engine) commentHumanMergeApproval(ctx context.Context, repoKey, owner, repo string, number int, head string, approval workflowApproval, item writeback.Item) {
	body := humanMergeApprovalComment(head, approval)
	if d := c.writebackAllowed(repoKey, item, writeback.KindHumanMergePathRelease, body); !d.Allowed {
		c.debug("human-merge-path owner approval: audit comment withheld by the write-back gate", "repo", repoKey, "pr", number, "head", head, "reason", d.Reason)
		return
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("human-merge-path owner approval: could not post audit comment", "repo", repoKey, "pr", number, "head", head, "error", err)
		return
	}
	c.noteOwnStageComment(owner, repo, number)
}

// headHumanMergeOwnerApproval returns the login whose explicit approval of
// obs's head releases the human-merge-path refusal, for the stage route. Read
// failures answer false: the route stays a person's merge.
func (c *Engine) headHumanMergeOwnerApproval(ctx context.Context, obs stageObservation) (string, bool) {
	if c == nil || c.gh == nil || strings.TrimSpace(obs.HeadSHA) == "" {
		return "", false
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil || pr.GetHead().GetSHA() != obs.HeadSHA {
		return "", false
	}
	comments, err := c.listAllIssueComments(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return "", false
	}
	approval, ok := c.humanMergeOwnerApprovalFor(ctx, obs.Owner, obs.Repo, pr, comments)
	return approval.Login, ok
}

// humanMergeOwnerApprovalReply is the `/hive approve` reply on a PR that
// touches a human-merge path in a repository listed in
// auto_merge.human_merge_owner_approval: it says what happens to the hold,
// never "cannot override". It is read-only like the rest of PRApproveReply;
// the stage pass releases the hold and the lane merges.
func (c *Engine) humanMergeOwnerApprovalReply(ctx context.Context, owner, repo string, pr *gh.PullRequest, head string) (string, error) {
	short := "`" + stagelabels.ShortSHA(head) + "`"
	comments, err := c.listIssueComments(ctx, owner, repo, pr.GetNumber())
	if err != nil {
		return "", err
	}
	if approval, ok := c.humanMergeOwnerApprovalFor(ctx, owner, repo, pr, comments); ok {
		return "`/hive approve` acknowledged: released for " + short + ". This PR changes a path a person must merge (`" + hgithub.HumanMergePathsConfigKey + "`), and on this repository (`" + humanMergeOwnerApprovalConfigKey + "`) `" + approval.Login + "`'s approval of this head releases the hive's hold: the hive takes it off on its next pass and merges the PR when CI, review, soak and the other merge gates clear. A new push needs a new approval. This reply does not mean the PR has merged.", nil
	}
	return "`/hive approve` acknowledged, but no approval of the current head (" + short + ") is recorded yet. This PR changes a path a person must merge (`" + hgithub.HumanMergePathsConfigKey + "`); on this repository (`" + humanMergeOwnerApprovalConfigKey + "`) a trusted merger's GitHub **Approve** review on that head, a `/hive decision approve` reply, or a `/hive approve` reply posted after the head releases the hive's hold, and the hive then merges the PR once the other gates clear.", nil
}

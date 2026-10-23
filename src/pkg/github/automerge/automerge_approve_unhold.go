package automerge

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// One human step per agent PR at ACMM L5 (RFC #13617 Proposal 2,
// hivecommons/hive#13629). With auto_merge.queue_on_approve_and_unhold on for
// a repo, a trusted merger who both submits an APPROVED review on an agent
// PR's current head and removes its level `hold` has queued the PR: the
// sweep calls the transport's QueuePRAutoMerge, the path the dashboard's
// queue-auto-merge button uses, with that merger as the queuer. Nothing else
// counts as consent. The queued lane then merges with every existing gate:
// trusted-merger re-check, head-SHA re-check, hold labels, Sentinel and CI.

// Skip reasons for the approve-and-unhold trigger.
const (
	approveUnholdReasonNotAppAuthored = "not-app-authored"
	approveUnholdReasonDraft          = "draft"
	approveUnholdReasonMissingHead    = "missing-head-sha"
	approveUnholdReasonQueued         = "already-queued"
	approveUnholdReasonHeld           = "held"
	approveUnholdReasonSentinel       = "sentinel"
	approveUnholdReasonQueuedOnHead   = "queue-approval-on-head"
	approveUnholdReasonNoUnhold       = "hold-not-removed-by-person"
	approveUnholdReasonNoLevelHold    = "no-level-hold"
	approveUnholdReasonUntrusted      = "unholder-not-trusted-merger"
	approveUnholdReasonNoApproval     = "no-approval-on-current-head"
	approveUnholdReasonNoNotice       = "no-level-hold-notice"
)

// autoMergeQueuer is the human merge-queue entry point the transport client
// provides (hgithub.Client.QueuePRAutoMerge). A transport without it never
// queues on approve-and-unhold.
type autoMergeQueuer interface {
	QueuePRAutoMerge(ctx context.Context, repo string, number int, queuedBy string) error
}

func (c *Engine) queueOnApproveAndUnholdActive(repo string) bool {
	return c != nil && c.queueOnApproveAndUnhold != nil && c.queueOnApproveAndUnhold(repo)
}

// queueApprovedUnheldLevelHolds queues every open agent PR in owner/repo on
// which one trusted merger approved the current head and removed the level
// hold. It returns how many PRs it queued. Errors are logged per PR and never
// stop the queued sweep.
func (c *Engine) queueApprovedUnheldLevelHolds(ctx context.Context, displayRepo, owner, repo, label string) int {
	if !c.ready() {
		return 0
	}
	queuer, ok := c.transport.(autoMergeQueuer)
	if !ok || strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return 0
	}
	if _, configured := c.isTrustedMerger(""); !configured {
		return 0
	}
	ctx = hgithub.WithRESTCaller(ctx, "hive:automerge_approve_unhold")
	prs, err := c.listOpenPullRequests(ctx, owner, repo)
	if err != nil {
		c.warn("approve-and-unhold queue could not list open PRs", "repo", displayRepo, "error", err)
		return 0
	}
	queued := 0
	for _, pr := range prs {
		if pr == nil {
			continue
		}
		number := pr.GetNumber()
		by, reason, err := c.approveAndUnholdConsent(ctx, owner, repo, pr, label)
		if err != nil {
			c.warn("approve-and-unhold queue could not evaluate PR", "repo", displayRepo, "pr", number, "reason", reason, "error", err)
			continue
		}
		if by == "" {
			c.debug("approve-and-unhold queue skipped PR", "repo", displayRepo, "pr", number, "reason", reason)
			continue
		}
		if err := queuer.QueuePRAutoMerge(ctx, owner+"/"+repo, number, by); err != nil {
			c.warn("approve-and-unhold queue failed", "repo", displayRepo, "pr", number, "queued_by", by, "error", err)
			continue
		}
		c.info("approve-and-unhold queued PR for auto-merge", "repo", displayRepo, "pr", number, "queued_by", by, "head_sha", pr.GetHead().GetSHA())
		queued++
	}
	return queued
}

// approveAndUnholdConsent returns the trusted merger whose APPROVED review on
// pr's current head and removal of its level hold together queue the PR, or
// "" and the reason nothing does. Cheap label checks run first; then reviews,
// hold-label events and the level-hold notice, each read only when the
// previous step passed.
func (c *Engine) approveAndUnholdConsent(ctx context.Context, owner, repo string, pr *gh.PullRequest, label string) (string, string, error) {
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	if !strings.EqualFold(hgithub.SafeGetLogin(pr.GetUser()), appBot) {
		return "", approveUnholdReasonNotAppAuthored, nil
	}
	if pr.GetDraft() {
		return "", approveUnholdReasonDraft, nil
	}
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if head == "" {
		return "", approveUnholdReasonMissingHead, nil
	}
	labels := labelNames(pr.Labels)
	if hasLabelFold(labels, label) {
		return "", approveUnholdReasonQueued, nil
	}
	if c.sentinelLabelPresent(labels) {
		return "", approveUnholdReasonSentinel, nil
	}
	if c.isHeld(labels) {
		return "", approveUnholdReasonHeld, nil
	}
	number := pr.GetNumber()

	approvedHead, queuedOnHead, err := c.currentHeadApprovals(ctx, owner, repo, number, head)
	if err != nil {
		return "", "list-reviews", err
	}
	// A queue approval the App already posted on this head means this head
	// was queued once; if a person since removed the queue label, that
	// decision stands.
	if queuedOnHead {
		return "", approveUnholdReasonQueuedOnHead, nil
	}
	if len(approvedHead) == 0 {
		return "", approveUnholdReasonNoApproval, nil
	}

	unholder, appHeld, err := c.levelHoldRemovedBy(ctx, owner, repo, number)
	if err != nil {
		return "", "list-events", err
	}
	if unholder == "" {
		return "", approveUnholdReasonNoUnhold, nil
	}
	if !appHeld {
		return "", approveUnholdReasonNoLevelHold, nil
	}
	if trusted, configured := c.isTrustedMerger(unholder); !configured || !trusted {
		return "", approveUnholdReasonUntrusted, nil
	}
	if !approvedHead[strings.ToLower(unholder)] {
		return "", approveUnholdReasonNoApproval, nil
	}

	notice, err := c.hasLevelHoldNotice(ctx, owner, repo, number)
	if err != nil {
		return "", "list-comments", err
	}
	if !notice {
		return "", approveUnholdReasonNoNotice, nil
	}
	return unholder, "", nil
}

// currentHeadApprovals returns the lower-cased logins of people whose latest
// decisive review (APPROVED, CHANGES_REQUESTED or DISMISSED) is an APPROVED
// review of head, and whether the App already posted a Hive queue approval
// on head. Comment-only reviews never count.
func (c *Engine) currentHeadApprovals(ctx context.Context, owner, repo string, number int, head string) (map[string]bool, bool, error) {
	latest := make(map[string]*gh.PullRequestReview)
	queuedOnHead := false
	opts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, false, err
		}
		for _, review := range reviews {
			if review == nil {
				continue
			}
			state := strings.ToUpper(review.GetState())
			if c.isHiveAppReviewAuthor(review) {
				if state == "APPROVED" && review.GetCommitID() == head && parseHiveQueueReview(review.GetBody()) != "" {
					queuedOnHead = true
				}
				continue
			}
			if !c.isPersonLogin(review.GetUser()) {
				continue
			}
			if state != "APPROVED" && state != "CHANGES_REQUESTED" && state != "DISMISSED" {
				continue
			}
			login := strings.ToLower(hgithub.SafeGetLogin(review.GetUser()))
			if prev, ok := latest[login]; !ok || !review.GetSubmittedAt().Before(prev.GetSubmittedAt().Time) {
				latest[login] = review
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	approved := make(map[string]bool)
	for login, review := range latest {
		if strings.EqualFold(review.GetState(), "APPROVED") && review.GetCommitID() == head {
			approved[login] = true
		}
	}
	return approved, queuedOnHead, nil
}

// levelHoldRemovedBy returns the person whose `unlabeled` event is the newest
// `hold` label event on the PR, and whether the App applied a `hold` before
// that removal. It returns "" when the newest hold event is not a person's
// removal.
func (c *Engine) levelHoldRemovedBy(ctx context.Context, owner, repo string, number int) (string, bool, error) {
	var events []*gh.IssueEvent
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return "", false, err
		}
		for _, event := range page {
			if event == nil || !strings.EqualFold(event.GetLabel().GetName(), "hold") {
				continue
			}
			if event.GetEvent() != "labeled" && event.GetEvent() != "unlabeled" {
				continue
			}
			events = append(events, event)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	var latest *gh.IssueEvent
	for _, event := range events {
		if latest == nil || !event.GetCreatedAt().Before(latest.GetCreatedAt().Time) {
			latest = event
		}
	}
	if latest == nil || latest.GetEvent() != "unlabeled" || !c.isPersonLogin(latest.GetActor()) {
		return "", false, nil
	}
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	appHeld := false
	for _, event := range events {
		if event.GetEvent() == "labeled" && strings.EqualFold(hgithub.SafeGetLogin(event.GetActor()), appBot) &&
			event.GetCreatedAt().Before(latest.GetCreatedAt().Time) {
			appHeld = true
			break
		}
	}
	return hgithub.SafeGetLogin(latest.GetActor()), appHeld, nil
}

// hasLevelHoldNotice reports whether the App posted the ACMM level-gate hold
// notice on the PR, which marks the hold a person removed as the level hold.
func (c *Engine) hasLevelHoldNotice(ctx context.Context, owner, repo string, number int) (bool, error) {
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			if comment == nil || !strings.EqualFold(hgithub.SafeGetLogin(comment.GetUser()), appBot) {
				continue
			}
			if hgithub.IsLevelHoldNotice(comment.GetBody()) {
				return true, nil
			}
		}
		if resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
}

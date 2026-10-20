package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ReviewHoldReleaseCommand is the reply a person posts on a pull request to
// accept a reviewer's requires_human verdict. On this release line the merge
// lanes do not consume it (the release is a GitHub Approve review on the
// current head); the command sweep still answers it so the person learns
// what the lane will and will not do (hivecommons/hive#13087).
const ReviewHoldReleaseCommand = "/hive approve"

// IsReviewHoldReleaseCommand reports whether a comment body carries
// `/hive approve` as its first-line command, the same shape the issue
// un-park sweep reads.
func IsReviewHoldReleaseCommand(body string) bool {
	cmd, ok := parseUnparkCommand(body)
	return ok && cmd.Kind == unparkCommandApprove
}

// IsPersonLogin reports whether u is a person: not appBotLogin, nor any
// other bot.
func IsPersonLogin(u *gh.User, appBotLogin string) bool {
	login := strings.TrimSpace(SafeGetLogin(u))
	if login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if app := strings.TrimSpace(appBotLogin); app != "" && strings.EqualFold(login, app) {
		return false
	}
	return true
}

// replyPRApproveCommand answers at most one unanswered `/hive approve` per PR
// per pass. PRs never enter the issue unpark path: an approval cannot clear
// their hold labels. The merge engine explains eligibility without
// authorizing a merge. Replies share the issue command's durable,
// author-checked ID marker. Only comments updated since the cutoff are read:
// a command older than that is ignored anyway, and Hive's reply is always
// newer than the command it answers, so the dedup markers stay visible.
func (c *Client) replyPRApproveCommand(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, reply func(context.Context, string, int, *gh.IssueComment) (string, error)) (bool, error) {
	comments, err := c.listIssueCommentsSince(ctx, owner, repo, issue.GetNumber(), cutoff)
	if err != nil {
		return false, err
	}
	answered := c.answeredUnparkCommentIDs(comments)
	for _, cm := range comments {
		if cm == nil || cm.GetID() <= 0 || answered[cm.GetID()] || strings.EqualFold(cm.GetUser().GetLogin(), c.AppBotLogin()) || cm.GetCreatedAt().Time.Before(cutoff) || !IsReviewHoldReleaseCommand(cm.GetBody()) {
			continue
		}
		body, err := reply(ctx, displayRepo, issue.GetNumber(), cm)
		if err != nil {
			return false, err
		}
		if body == "" {
			continue
		}
		if err := c.postUnparkReply(ctx, owner, repo, issue.GetNumber(), cm.GetID(), body); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

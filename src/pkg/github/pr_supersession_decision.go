package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// supersessionCloseDecision accepts only an explicit close instruction. A bare
// reject is safe only after detecting a live supersession below; other decision
// text continues to the existing changes-requested path.
func supersessionCloseDecision(command unparkCommand) (closer int, ok bool) {
	if command.Kind != unparkCommandDecision {
		return 0, false
	}
	text := strings.ToLower(strings.TrimSpace(command.DecisionText()))
	if text == "reject" {
		return 0, true
	}
	const prefix = "close superseded by #"
	if !strings.HasPrefix(text, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(text, prefix))
	return n, err == nil && n > 0
}

// Called only after eligibleUnparkCommand and TrustedMerger authorize the actor.
// No contributor branch is pushed or rewritten.
func (c *Client) closeSupersededPRDecision(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, actor string, requestedCloser int) (string, error) {
	if labels := labelNames(pr.Labels); c.isHeld(labels) || c.isExempt(labels) {
		return "The PR is held or exempt from hive writes; clear that gate before closing it as superseded.", nil
	}
	identity := c.getHiveIdentity()
	candidate, closer, detected, err := c.detectSupersession(ctx, displayRepo, pr, identity, time.Now())
	if err != nil {
		return "", err
	}
	if !detected || !candidate.Closing {
		return "No merged PR currently supersedes this PR's closing claims; the PR stays open.", nil
	}
	if requestedCloser != 0 && (requestedCloser != closer.Number || !strings.EqualFold(closer.Repo, owner+"/"+repo)) {
		return fmt.Sprintf("The detected closer is %s#%d, which does not match the requested PR; the PR stays open.", closer.Repo, closer.Number), nil
	}
	note := fmt.Sprintf("Closing as superseded by %s#%d, following @%s's `/hive decision`. No branch changes are required.", closer.Repo, closer.Number, actor)
	if err := c.CreateIssueComment(ctx, displayRepo, pr.GetNumber(), note); err != nil {
		return "", err
	}
	if err := c.CloseSupersededPR(ctx, displayRepo, pr.GetNumber()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Closed as superseded by %s#%d, following @%s's decision.", closer.Repo, closer.Number, actor), nil
}

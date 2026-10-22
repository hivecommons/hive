package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/review"
)

// Operator decisions on a pull request parked at stage/085-needs-decision,
// made from the dashboard's activity monitor (hivecommons/hive#13479
// follow-up). Each mirrors what the hive honours when a maintainer does it
// on GitHub; none of them merges:
//
//   - approval posts the `/lgtm` and `/approve` commands as two comments —
//     the merge gate (Prow / tide, then the hive's own queue) decides from
//     there, exactly as when the maintainer types them;
//   - a direction is the `/hive decision <text>` lane of
//     tryUnparkPullRequest: recorded against the head so the review
//     planner kicks the author agent, then acknowledged in a reply.

// ErrPullRequestNotFound is returned when the pull request does not exist.
var ErrPullRequestNotFound = errors.New("pull request not found")

// ErrPullRequestClosed is returned when a decision is pointed at a pull
// request that is no longer open.
var ErrPullRequestClosed = errors.New("pull request is closed")

// ErrPullRequestNotHiveAuthored is returned when a direction is pointed at
// a pull request the hive did not open: the hive does not push to it, so
// there is no author agent to carry the instruction.
var ErrPullRequestNotHiveAuthored = errors.New("pull request was not opened by the hive")

// ErrPullRequestHeadUnknown is returned when the PR's head SHA cannot be
// read, so a direction has nothing to be recorded against.
var ErrPullRequestHeadUnknown = errors.New("pull request head is unknown")

// PRApprovalCommands are the comment bodies an approval posts, in order.
var PRApprovalCommands = []string{"/lgtm", "/approve"}

// PullRequestApprovalEvent records one dashboard approval for the audit log.
type PullRequestApprovalEvent struct {
	Repo    string
	Number  int
	HeadSHA string
	Actor   string
}

func renderPRApprovalCommandComment(command, actor string) string {
	return fmt.Sprintf("%s\n\nPosted by @%s from the Hive dashboard; the merge gate decides from here.", command, strings.TrimSpace(actor))
}

// openPullRequestHead returns the PR's head SHA and whether the hive opened
// it, or ErrPullRequestClosed / a not-found error.
func (c *Client) openPullRequestHead(ctx context.Context, owner, name string, number int) (head string, selfAuthored bool, err error) {
	pr, resp, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, fmt.Errorf("pull request %s/%s#%d: %w", owner, name, number, ErrPullRequestNotFound)
		}
		return "", false, fmt.Errorf("fetching pull request %s/%s#%d: %w", owner, name, number, err)
	}
	if strings.EqualFold(pr.GetState(), "closed") {
		return "", false, ErrPullRequestClosed
	}
	if pr.GetHead() != nil {
		head = strings.TrimSpace(pr.GetHead().GetSHA())
	}
	selfAuthored = c.isHiveAccount(pr.GetUser()) || labelPresent(issueLabelNames(pr.Labels), authorkind.Hive.Label())
	return head, selfAuthored, nil
}

func (c *Client) operatorPRTarget(repo string, number int, actor, what string) (owner, name string, err error) {
	if c == nil || c.client == nil {
		return "", "", ErrNoGitHubClient
	}
	if strings.TrimSpace(actor) == "" {
		return "", "", fmt.Errorf("%s: actor is required", what)
	}
	owner, name = c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return "", "", fmt.Errorf("%s: %w", what, err)
	}
	if number <= 0 {
		return "", "", fmt.Errorf("%s: pull request number is required", what)
	}
	return owner, name, nil
}

// RecordOperatorPullRequestApproval posts the `/lgtm` and `/approve`
// commands on an open pull request on behalf of actor, each attributed to
// them. It does not review, label or merge: whatever gates merging reads
// the commands exactly as it would had the maintainer typed them. The
// caller is responsible for having verified actor's write access.
func (c *Client) RecordOperatorPullRequestApproval(ctx context.Context, repo string, number int, actor string) (PullRequestApprovalEvent, error) {
	owner, name, err := c.operatorPRTarget(repo, number, actor, "RecordOperatorPullRequestApproval")
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	actor = strings.TrimSpace(actor)
	head, _, err := c.openPullRequestHead(ctx, owner, name, number)
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	for _, command := range PRApprovalCommands {
		if err := c.CreateIssueComment(ctx, owner+"/"+name, number, renderPRApprovalCommandComment(command, actor)); err != nil {
			return PullRequestApprovalEvent{}, fmt.Errorf("posting %s on %s/%s#%d: %w", command, owner, name, number, err)
		}
	}
	event := PullRequestApprovalEvent{Repo: owner + "/" + name, Number: number, HeadSHA: head, Actor: actor}
	if c.logger != nil {
		c.logger.Info("pull request approval commands posted by dashboard decision", "repo", event.Repo, "pr", number, "actor", actor, "head", head)
	}
	return event, nil
}

// RecordOperatorPullRequestDecision records a maintainer's direction on a
// hive-authored open pull request made through the dashboard — the same
// effect as that maintainer commenting `/hive decision <text>` and the
// un-park sweep answering it: the text is stored against the head as a
// human change request (the review planner kicks the author agent with it
// verbatim) and one reply naming the actor is posted. The caller is
// responsible for having verified actor's write access.
func (c *Client) RecordOperatorPullRequestDecision(ctx context.Context, repo string, number int, actor, decision string, store review.HumanChangeRequestStore) (PullRequestDecisionEvent, error) {
	owner, name, err := c.operatorPRTarget(repo, number, actor, "RecordOperatorPullRequestDecision")
	if err != nil {
		return PullRequestDecisionEvent{}, err
	}
	actor = strings.TrimSpace(actor)
	decision = strings.TrimSpace(decision)
	if decision == "" {
		return PullRequestDecisionEvent{}, fmt.Errorf("RecordOperatorPullRequestDecision: decision text is required")
	}
	if store == nil {
		return PullRequestDecisionEvent{}, fmt.Errorf("RecordOperatorPullRequestDecision: no human change request store")
	}
	head, selfAuthored, err := c.openPullRequestHead(ctx, owner, name, number)
	if err != nil {
		return PullRequestDecisionEvent{}, err
	}
	if !selfAuthored {
		return PullRequestDecisionEvent{}, ErrPullRequestNotHiveAuthored
	}
	if head == "" {
		return PullRequestDecisionEvent{}, ErrPullRequestHeadUnknown
	}
	req := review.HumanChangeRequest{
		Repo:     owner + "/" + name,
		Number:   number,
		HeadSHA:  head,
		Source:   review.HumanChangeRequestSourceDecision,
		SourceID: operatorDecisionMarkerID,
		Actor:    actor,
		Body:     decision,
	}
	// Record before replying: a reply without a record would promise a fix
	// nothing is queued to make.
	if err := store.Record(req); err != nil {
		return PullRequestDecisionEvent{}, fmt.Errorf("recording dashboard decision on %s/%s#%d: %w", owner, name, number, err)
	}
	body := renderPRDecisionRecordedReply(actor, head) + "\n\n_Recorded from the hive dashboard._"
	if err := c.postUnparkReply(ctx, owner, name, number, operatorDecisionMarkerID, body); err != nil {
		return PullRequestDecisionEvent{}, err
	}
	event := PullRequestDecisionEvent{Repo: owner + "/" + name, Number: number, HeadSHA: head, Actor: actor, Decision: decision}
	if c.logger != nil {
		c.logger.Info("pull request direction recorded by dashboard decision", "repo", event.Repo, "pr", number, "actor", actor, "head", head)
	}
	return event, nil
}

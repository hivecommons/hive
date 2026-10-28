package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
//     there, exactly as when the maintainer types them. A person's approval
//     (PullRequestApprovalOptions) is also recorded against the head as the
//     answer to a requires_human verdict, lifts the `needs-human` park and
//     the sentinel alert, and is submitted to the stage engine — the
//     comments alone were read by no lane (#14821, #14822, crux#30 stayed
//     at stage/085 after the owner approved);
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
	// Recorded is true when the approval was written to the human change
	// request store as the head-scoped answer the merge lanes and the stage
	// engine honour (PullRequestApprovalOptions.HumanChangeRequests).
	Recorded bool
	// NeedsHumanLifted is true when the PR's `needs-human` label was taken
	// off by this approval.
	NeedsHumanLifted bool
	// SentinelLabelCleared is true when the sentinel alert label was taken
	// off by this approval; SentinelAcknowledged when the review evidence
	// bundle for the head now records the person's acknowledgment of its
	// sentinel findings.
	SentinelLabelCleared bool
	SentinelAcknowledged bool
	// Stage is the stage label the engine derived when the approval was
	// submitted to it, or "" when the stage write was deferred to the sweep.
	Stage string
}

// PullRequestApprovalOptions is what a person's Approve decision releases
// beyond posting the commands (hivecommons/hive: #14821, #14822, crux#30
// sat at stage/085 after the owner approved from the dashboard — the
// comments were read by nobody but the workflow-path fence). The zero value
// posts the commands only, which is what the delegated "Let hive decide"
// path keeps: a recommendation must not clear a person's gate.
type PullRequestApprovalOptions struct {
	// HumanChangeRequests, when set, records the approval against the head
	// as a bare `approve` decision — the fact withRecordedDecision and the
	// stage engine read as "the requires_human verdict on this head has
	// been answered", for every lane, whoever authored the PR.
	HumanChangeRequests review.HumanChangeRequestStore
	// LiftNeedsHuman takes `needs-human` off the PR. The hive's own park
	// (a supersession or adoption ask) is what the person just answered; a
	// person's label is lifted on the same reasoning as an issue decision
	// (stageengine.DecisionLiftsNeedsHuman).
	LiftNeedsHuman bool
	// SentinelLabel, when non-empty, is the alert label to take off: the
	// Decide dialog showed the sentinel reason and styled Approve as
	// dangerous, so the approval is the human review the alert asked for.
	// The acknowledgment is also recorded in the head's evidence bundle so
	// a bundle-only alert (no label) clears too.
	SentinelLabel string
	// Now stamps the acknowledgment; time.Now when nil.
	Now func() time.Time
}

// prApprovalCommentMarkerPrefix opens the hidden marker a dashboard approval
// comment carries so a later reader can bind the decision to the head it was
// made on (hivecommons/hive#14664).
const prApprovalCommentMarkerPrefix = "<!-- hive:pr-approval"

// prApprovalCommentSuffix is the attribution line of a dashboard approval.
const prApprovalCommentSuffix = " from the Hive dashboard; the merge gate decides from here."

func renderPRApprovalCommandComment(command, actor, head string) string {
	body := fmt.Sprintf("%s\n\nPosted by @%s%s", command, strings.TrimSpace(actor), prApprovalCommentSuffix)
	if head = strings.ToLower(strings.TrimSpace(head)); head != "" {
		body += "\n" + prApprovalCommentMarkerPrefix + " head=" + head + " by=" + strings.ToLower(strings.TrimSpace(actor)) + " -->"
	}
	return body
}

// ParsePRApprovalCommandComment reads a comment the dashboard posted on a
// maintainer's behalf (renderPRApprovalCommandComment). It returns the
// command (`/lgtm` or `/approve`), the actor the comment is attributed to,
// and the head SHA the marker bound it to ("" for comments posted before
// the marker existed). ok is false for any other comment.
func ParsePRApprovalCommandComment(body string) (command, actor, head string, ok bool) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var rest []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if command == "" {
			command = line
			continue
		}
		rest = append(rest, line)
	}
	if len(rest) == 0 {
		return "", "", "", false
	}
	known := false
	for _, want := range PRApprovalCommands {
		if command == want {
			known = true
		}
	}
	if !known {
		return "", "", "", false
	}
	attribution := rest[0]
	if !strings.HasPrefix(attribution, "Posted by @") || !strings.HasSuffix(attribution, prApprovalCommentSuffix) {
		return "", "", "", false
	}
	actor = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(attribution, "Posted by @"), prApprovalCommentSuffix))
	if actor == "" || strings.ContainsAny(actor, " \t") {
		return "", "", "", false
	}
	for _, line := range rest[1:] {
		if !strings.HasPrefix(line, prApprovalCommentMarkerPrefix) {
			continue
		}
		for _, field := range strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, prApprovalCommentMarkerPrefix), "-->")) {
			if v, found := strings.CutPrefix(field, "head="); found {
				head = strings.ToLower(strings.TrimSpace(v))
			}
		}
	}
	return command, actor, head, true
}

// openPullRequestHead returns the PR's head SHA and whether the hive opened
// it, or ErrPullRequestClosed / a not-found error.
func (c *Client) openPullRequestHead(ctx context.Context, owner, name string, number int) (head string, selfAuthored bool, err error) {
	head, selfAuthored, _, err = c.openPullRequestHeadLabels(ctx, owner, name, number)
	return head, selfAuthored, err
}

// openPullRequestHeadLabels is openPullRequestHead plus the PR's labels as
// read in the same fetch.
func (c *Client) openPullRequestHeadLabels(ctx context.Context, owner, name string, number int) (head string, selfAuthored bool, labels []string, err error) {
	pr, resp, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil, fmt.Errorf("pull request %s/%s#%d: %w", owner, name, number, ErrPullRequestNotFound)
		}
		return "", false, nil, fmt.Errorf("fetching pull request %s/%s#%d: %w", owner, name, number, err)
	}
	if strings.EqualFold(pr.GetState(), "closed") {
		return "", false, nil, ErrPullRequestClosed
	}
	if pr.GetHead() != nil {
		head = strings.TrimSpace(pr.GetHead().GetSHA())
	}
	labels = issueLabelNames(pr.Labels)
	selfAuthored = c.isHiveAccount(pr.GetUser()) || labelPresent(labels, authorkind.Hive.Label())
	return head, selfAuthored, labels, nil
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
	return c.RecordOperatorPullRequestApprovalWith(ctx, repo, number, actor, PullRequestApprovalOptions{})
}

// RecordOperatorPullRequestApprovalWith is RecordOperatorPullRequestApproval
// plus what opts asks the person's approval to release, in this order: the
// commands are posted first (the durable, visible record), then the
// approval is recorded against the head, the park labels the person just
// answered come off, the sentinel acknowledgment is written, and the stage
// engine is given the decision so the row moves now rather than on the
// next sweep. A failed label or stage write after the record is logged and
// reported on the event, not returned: the record stands, and the lanes
// derive the same answer from it on their next pass.
func (c *Client) RecordOperatorPullRequestApprovalWith(ctx context.Context, repo string, number int, actor string, opts PullRequestApprovalOptions) (PullRequestApprovalEvent, error) {
	owner, name, err := c.operatorPRTarget(repo, number, actor, "RecordOperatorPullRequestApproval")
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	actor = strings.TrimSpace(actor)
	head, selfAuthored, labels, err := c.openPullRequestHeadLabels(ctx, owner, name, number)
	if err != nil {
		return PullRequestApprovalEvent{}, err
	}
	full := owner + "/" + name
	for _, command := range PRApprovalCommands {
		if err := c.CreateIssueComment(ctx, full, number, renderPRApprovalCommandComment(command, actor, head)); err != nil {
			return PullRequestApprovalEvent{}, fmt.Errorf("posting %s on %s/%s#%d: %w", command, owner, name, number, err)
		}
	}
	event := PullRequestApprovalEvent{Repo: full, Number: number, HeadSHA: head, Actor: actor}
	if c.logger != nil {
		c.logger.Info("pull request approval commands posted by dashboard decision", "repo", event.Repo, "pr", number, "actor", actor, "head", head)
	}
	if opts.HumanChangeRequests != nil && head != "" {
		req := review.HumanChangeRequest{
			Repo:     full,
			Number:   number,
			HeadSHA:  head,
			Source:   review.HumanChangeRequestSourceDecision,
			SourceID: operatorDecisionMarkerID,
			Actor:    actor,
			Body:     prDecisionApproveBody,
			Kind:     review.HumanChangeRequestKindApprove,
		}
		if err := opts.HumanChangeRequests.Record(req); err != nil {
			return event, fmt.Errorf("recording dashboard approval on %s#%d: %w", full, number, err)
		}
		event.Recorded = true
	}
	if opts.LiftNeedsHuman && labelPresent(labels, issueNeedsHumanLabel) {
		if err := c.RemoveLabel(ctx, full, number, issueNeedsHumanLabel); err != nil {
			c.warnOperatorApproval("needs-human not lifted", full, number, head, err)
		} else {
			event.NeedsHumanLifted = true
		}
	}
	if label := strings.TrimSpace(opts.SentinelLabel); label != "" {
		if labelPresent(labels, label) {
			if err := c.RemoveLabel(ctx, full, number, label); err != nil {
				c.warnOperatorApproval("sentinel label not cleared", full, number, head, err)
			} else {
				event.SentinelLabelCleared = true
			}
		}
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		event.SentinelAcknowledged = c.RecordReviewEvidenceSentinelAck(full, number, head, actor, now())
	}
	if event.Recorded {
		event.Stage = c.applyPRDecisionStage(ctx, owner, name, number, head, actor, prDecisionApprove, selfAuthored)
		c.logPRDecision(full, number, head, actor, prDecisionApprove, event.Stage, false)
	}
	return event, nil
}

// prDecisionApproveBody is the decision text a dashboard approval records,
// the bare form `/hive decision approve` carries.
const prDecisionApproveBody = "approve"

func (c *Client) warnOperatorApproval(what, repo string, number int, head string, err error) {
	if c.logger != nil {
		c.logger.Warn("dashboard approval: "+what, "repo", repo, "pr", number, "head", head, "error", err)
	}
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
	mode := prDecisionModeFor(unparkCommand{Kind: unparkCommandDecision, Decision: decision, Text: decision})
	req := review.HumanChangeRequest{
		Repo:     owner + "/" + name,
		Number:   number,
		HeadSHA:  head,
		Source:   review.HumanChangeRequestSourceDecision,
		SourceID: operatorDecisionMarkerID,
		Actor:    actor,
		Body:     decision,
		Kind:     mode.kind(),
	}
	// Record before replying: a reply without a record would promise a fix
	// nothing is queued to make.
	if err := store.Record(req); err != nil {
		return PullRequestDecisionEvent{}, fmt.Errorf("recording dashboard decision on %s/%s#%d: %w", owner, name, number, err)
	}
	stage := c.applyPRDecisionStage(ctx, owner, name, number, head, actor, mode, selfAuthored)
	body := renderPRDecisionReply(actor, head, mode) + "\n\n_Recorded from the hive dashboard._"
	if err := c.postUnparkReply(ctx, owner, name, number, operatorDecisionMarkerID, body); err != nil {
		return PullRequestDecisionEvent{}, err
	}
	event := PullRequestDecisionEvent{Repo: owner + "/" + name, Number: number, HeadSHA: head, Actor: actor, Decision: decision, Mode: string(mode)}
	c.logPRDecision(event.Repo, number, head, actor, mode, stage, false)
	return event, nil
}

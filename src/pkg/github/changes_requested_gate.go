package github

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/reviewgate"
)

// Open change-request gate (hivecommons/hive#20067). Every hive path that
// approves a pull request, writes `lgtm` / `approved`, queues one or merges
// one asks GateChangesRequested first; the rules live in pkg/reviewgate.

// What a gated attempt was about to do, recorded as action= on the audit.
const (
	ChangesRequestedGateActionMerge   = "merge"
	ChangesRequestedGateActionApprove = "approve"
	ChangesRequestedGateActionLabel   = "label"
	ChangesRequestedGateActionEnqueue = "enqueue"
)

// AuditActionChangesRequestedGate records every gated attempt: outcome=
// blocked (an open change request stopped it), overridden (an operator's
// audited dashboard override let it through) or unreadable (the reviews
// could not be read, so the gate failed closed). Detail carries action=,
// path=, reviewers= and review_ids=, plus actor= and reason= on an override.
const AuditActionChangesRequestedGate = "changes-requested-gate"

// prowApprovedLabel is Prow's approve-plugin label.
const prowApprovedLabel = "approved"

// OpenChangeRequests returns repo#number's open change requests from
// reviewers with write access or above.
func (c *Client) OpenChangeRequests(ctx context.Context, repo string, number int) ([]reviewgate.Block, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	return openChangeRequestsCheck(WithRESTCaller(ctx, "hive:changes_requested_gate"), c.client, owner, name, number)
}

// openChangeRequestsCheck reads the open change requests. It is a variable
// only so this package's older tests, whose GitHub fakes predate the gate,
// can run without serving the review listing; the gate's own tests restore
// reviewgate.CheckGitHub.
var openChangeRequestsCheck = reviewgate.CheckGitHub

// GateChangesRequested returns nil when repo#number has no open change
// request from a writer, or when an operator's recorded override covers every
// one. Otherwise it returns a *reviewgate.BlockedError (errors.Is
// reviewgate.ErrChangesRequested). A failed review read also refuses: the
// gate fails closed. Every refusal and every override is logged and audited
// with the reviewer and the review ID.
func (c *Client) GateChangesRequested(ctx context.Context, repo string, number int, action, path string) error {
	if c == nil || c.client == nil || number <= 0 {
		return nil
	}
	owner, name := c.splitRepo(repo)
	full := owner + "/" + name
	blocks, err := c.OpenChangeRequests(ctx, full, number)
	if err != nil {
		c.RecordChangesRequestedGate(full, number, action, path, reviewgate.Decision{}, err)
		return &reviewgate.BlockedError{Repo: full, Number: number, Cause: err}
	}
	d := reviewgate.Decide(full, number, blocks)
	if len(d.Blocks) == 0 {
		return nil
	}
	c.RecordChangesRequestedGate(full, number, action, path, d, nil)
	if d.Allowed() {
		return nil
	}
	return &reviewgate.BlockedError{Repo: full, Number: number, Blocks: d.Blocks}
}

// RecordChangesRequestedGate logs and audits one gated attempt. The
// automerge engine, which reads reviews through its own client, records
// through it too so every path lands on the same audit action.
func (c *Client) RecordChangesRequestedGate(repo string, number int, action, path string, d reviewgate.Decision, cause error) {
	if c == nil {
		return
	}
	outcome := reviewgate.OutcomeBlocked
	switch {
	case cause != nil:
		outcome = reviewgate.OutcomeUnreadable
	case d.Overridden:
		outcome = reviewgate.OutcomeOverridden
	}
	extra := []string{
		"outcome", outcome,
		"action", action,
		"path", path,
		"reviewers", reviewgate.Reviewers(d.Blocks),
		"review_ids", reviewgate.ReviewIDs(d.Blocks),
	}
	if d.Overridden {
		extra = append(extra, "actor", d.Override.Actor, "reason", d.Override.Reason)
	}
	if cause != nil {
		extra = append(extra, "error", cause.Error())
	}
	c.recordWriteAudit(AuditActionChangesRequestedGate, InvocationMeta{Agent: AttributionAgentGovernor},
		WriteTarget{Repo: repo, Number: number}, extra...)
	if c.logger == nil {
		return
	}
	args := []any{"repo", repo, "pr", number, "action", action, "path", path, "outcome", outcome,
		"reviewers", reviewgate.Reviewers(d.Blocks), "review_ids", reviewgate.ReviewIDs(d.Blocks)}
	switch outcome {
	case reviewgate.OutcomeOverridden:
		c.logger.Warn("open change request overridden by operator decision", append(args, "actor", d.Override.Actor, "reason", d.Override.Reason)...)
	case reviewgate.OutcomeUnreadable:
		c.logger.Warn("could not read reviews; refusing to approve, label or merge", append(args, "error", cause)...)
	default:
		c.logger.Warn("open change request blocks hive approval, label or merge", args...)
	}
}

// approvalLabels reports whether labels include one that approves a PR for
// merging: the configured auto-merge label, Prow's `lgtm` or `approved`.
func (c *Client) approvalLabels(labels []string) bool {
	queue := c.AutoMergeLabel()
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if strings.EqualFold(l, queue) || strings.EqualFold(l, prowLgtmLabel) || strings.EqualFold(l, prowApprovedLabel) {
			return true
		}
	}
	return false
}

// GateApprovalLabels runs the gate when labels include an approval label
// (see approvalLabels) and is a no-op otherwise. The GitHub forge adapter's
// label writer calls it so the agents' label relay is gated as well.
func (c *Client) GateApprovalLabels(ctx context.Context, repo string, number int, labels []string) error {
	if c == nil || !c.approvalLabels(labels) {
		return nil
	}
	return c.GateChangesRequested(ctx, repo, number, ChangesRequestedGateActionLabel, PRAuditPathRelay)
}

// The dashboard reaches the gate through pkg/github only (its import
// ratchet), so the pieces its operator-override path needs are re-exported.

// ChangeRequestBlock is one open change request (reviewgate.Block).
type ChangeRequestBlock = reviewgate.Block

// DescribeChangeRequests renders blocks as "alice (review 12), ...".
func DescribeChangeRequests(blocks []ChangeRequestBlock) string { return reviewgate.Describe(blocks) }

// ChangeRequestReviewers lists the blocks' reviewers for audit detail.
func ChangeRequestReviewers(blocks []ChangeRequestBlock) string { return reviewgate.Reviewers(blocks) }

// ChangeRequestReviewIDs lists the blocks' review IDs for audit detail.
func ChangeRequestReviewIDs(blocks []ChangeRequestBlock) string { return reviewgate.ReviewIDs(blocks) }

// RecordChangesRequestedOverride records an operator's audited override of
// exactly these open change requests (reviewgate.RecordOverride).
func RecordChangesRequestedOverride(repo string, number int, actor, reason string, blocks []ChangeRequestBlock) error {
	_, err := reviewgate.RecordOverride(repo, number, actor, reason, blocks)
	return err
}

// ClearChangesRequestedOverride drops any override for repo#number.
func ClearChangesRequestedOverride(repo string, number int) { reviewgate.ClearOverride(repo, number) }

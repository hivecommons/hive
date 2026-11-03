package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrIssueNotParked is returned when an operator decision is recorded on an
// issue that carries no parking label: there is nothing to decide, and
// writing `approved-direction` onto an un-parked issue would be a silent
// endorsement nobody asked for.
var ErrIssueNotParked = errors.New("issue is not parked on a decision")

// ErrDecisionTargetIsPR is returned when the operator decision path is
// pointed at a pull request. A PR parked at stage/085 is answered by the
// review planner's fix lane, not by the issue un-park.
var ErrDecisionTargetIsPR = errors.New("decision target is a pull request")

// operatorDecisionMarkerID is the comment id the dashboard's decision reply
// carries in the unpark-reply marker. The sweep's answered-id scan ignores a
// non-positive id (so this reply never shadows a real command), while the
// re-park refusal still finds the "Decision:" line through the shared prefix.
const operatorDecisionMarkerID = 0

// RecordOperatorIssueDecision records a maintainer decision made through the
// dashboard on a parked issue — the same effect as that maintainer leaving
// `/hive approve` (decision == "") or `/hive decision <text>` on the issue and
// the un-park sweep answering it: the park labels the hive added are cleared,
// `approved-direction` is applied, the stage engine is told the park lifted,
// and one reply naming the actor and the decision is posted. The caller is
// responsible for having verified that actor holds write access on repo, the
// bar the sweep itself applies to a commenter. `needs-human` is lifted or
// kept exactly as the sweep decides for a maintainer's command (#13568);
// `hold` is never touched.
func (c *Client) RecordOperatorIssueDecision(ctx context.Context, repo string, number int, actor, decision string) (IssueUnparkEvent, error) {
	if c == nil || c.client == nil {
		return IssueUnparkEvent{}, ErrNoGitHubClient
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueDecision: actor is required")
	}
	owner, name := c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueDecision: %w", err)
	}
	if number <= 0 {
		return IssueUnparkEvent{}, fmt.Errorf("RecordOperatorIssueDecision: issue number is required")
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return IssueUnparkEvent{}, fmt.Errorf("reading %s#%d before decision: %w", repo, number, err)
	}
	if issue.IsPullRequest() {
		return IssueUnparkEvent{}, ErrDecisionTargetIsPR
	}
	labels := issueLabelNames(issue.Labels)
	if !hasIssueParkingLabel(labels) {
		return IssueUnparkEvent{}, ErrIssueNotParked
	}
	needsHuman := unparkNeedsHumanOutcome{}
	if hasIssueNeedsHumanLabel(labels) {
		needsHuman = c.needsHumanOutcome(ctx, owner, name, number, nil)
	}
	removed, err := c.clearIssueParkingLabels(ctx, owner, name, number, labels, actor, needsHuman.lift)
	if err != nil {
		return IssueUnparkEvent{}, err
	}
	decision = strings.TrimSpace(decision)
	if decision == "" {
		decision = "go with the recommendation"
	}
	body := renderUnparkAcceptedReplyFiltered(actor, decision, removed, hasIssueNeedsHumanLabel(labels), needsHuman, c.unparkIssueFilterNote(labels, removed)) +
		"\n\n_Recorded from the hive dashboard._"
	if err := c.postUnparkReply(ctx, owner, name, number, operatorDecisionMarkerID, body); err != nil {
		return IssueUnparkEvent{}, err
	}
	event := IssueUnparkEvent{Repo: owner + "/" + name, Number: number, Actor: actor, Decision: decision}
	if c.logger != nil {
		c.logger.Info("issue un-parked by dashboard decision", "repo", event.Repo, "issue", number, "actor", actor, "decision", decision)
	}
	return event, nil
}

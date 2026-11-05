package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// An agent that parked an issue on a decision can take the ask back
// (`hive-open-issue comment --withdraw-decision`) once the question is gone:
// Danathar/openaudible-autoupdate#177's architect found the real cause,
// wrote "No decision is needed here any more ... Please do not apply option
// A", and the issue still sat at 045 in the owner's queue under a "What to
// reply" notice recommending option A until a maintainer answered.

// parkWithdrawnMarkerPrefix tags the withdrawal comment; the agent's name
// follows. unparkMenuFor reads it as the end of the park it withdrew.
const parkWithdrawnMarkerPrefix = "<!-- hive:park-withdrawn by="

func parkWithdrawnMarker(agent string) string {
	return parkWithdrawnMarkerPrefix + strings.TrimSpace(agent) + " -->"
}

// parkTextAgent names the agent whose text parks the issue now: the
// attribution trailer of the newest hive-authored owner-step text, a
// comment or else the body of a hive-authored issue. A maintainer's
// accepted un-park ends that park, so text before it does not count. When
// the newest such text is a withdrawal, withdrawn is set and agent is the
// withdrawing agent: the comment is up and only the lift is left (a retry).
// "" when no such text names an agent.
func (c *Client) parkTextAgent(issue *gh.Issue, comments []*gh.IssueComment) (agent string, withdrawn bool) {
	if c.isHiveAccount(issue.GetUser()) && hasOwnerStepMarker(issue.GetBody()) {
		agent = trailerAgent(issue.GetBody())
	}
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		body := cm.GetBody()
		switch {
		case strings.Contains(body, unparkAcceptedMarker):
			agent, withdrawn = "", false
		case strings.Contains(body, parkWithdrawnMarkerPrefix):
			agent, withdrawn = parkWithdrawnBy(body), true
		case strings.Contains(body, unparkNoticeMarker), strings.Contains(body, unparkReplyMarkerPrefix):
		case hasOwnerStepMarker(body):
			agent, withdrawn = trailerAgent(body), false
		}
	}
	return agent, withdrawn
}

// parkWithdrawnBy reads the agent from a withdrawal marker, or "".
func parkWithdrawnBy(body string) string {
	_, rest, ok := strings.Cut(body, parkWithdrawnMarkerPrefix)
	if !ok {
		return ""
	}
	agent, _, ok := strings.Cut(rest, " -->")
	if !ok {
		return ""
	}
	return strings.TrimSpace(agent)
}

// trailerAgent reads the agent from the text's attribution trailer. Hidden
// markers after it (the owner-step marker a park ends with) are skipped:
// ParseAttributionTrailer reads only the final visible line.
func trailerAgent(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && !(strings.HasPrefix(last, "<!--") && strings.HasSuffix(last, "-->")) {
			break
		}
		lines = lines[:len(lines)-1]
	}
	meta, ok := ParseAttributionTrailer(strings.Join(lines, "\n"))
	if !ok {
		return ""
	}
	return strings.TrimSpace(meta.Agent)
}

// parkWithdrawal is a withdrawal the watcher may carry out: the issue and
// its comments as read, and whether the withdrawal comment is already up
// (a retry after a failed lift), so it is not posted twice.
type parkWithdrawal struct {
	issue    *gh.Issue
	comments []*gh.IssueComment
	posted   bool
}

// decisionParkWithdrawal checks a `withdraw_decision` comment request
// against the issue. It returns the refusal to give the agent ("" when the
// withdrawal may go ahead), what to withdraw, and an error for a failed
// read (the request retries).
func (c *Client) decisionParkWithdrawal(ctx context.Context, req IssueRequest) (string, *parkWithdrawal, error) {
	owner, name := c.splitRepo(req.Repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, req.Number)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s#%d to withdraw its park: %w", req.Repo, req.Number, err)
	}
	if issue.IsPullRequest() {
		return fmt.Sprintf("withdraw_decision applies to issues only: %s#%d is a pull request", req.Repo, req.Number), nil, nil
	}
	labels := issueLabelNames(issue.Labels)
	if !labelPresent(labels, c.needsDecisionLabel()) && !labelPresent(labels, NeedsDecisionStageLabel) {
		return fmt.Sprintf("nothing to withdraw: %s#%d is not parked on a decision; post a plain comment instead", req.Repo, req.Number), nil, nil
	}
	if c.hasNeedsHumanLabel(labels) {
		return fmt.Sprintf("not withdrawn: %s#%d also carries `needs-human`, which only a person lifts; say in a plain comment that the decision is no longer needed", req.Repo, req.Number), nil, nil
	}
	comments, err := c.listIssueComments(ctx, owner, name, req.Number)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s#%d comments to withdraw its park: %w", req.Repo, req.Number, err)
	}
	parker, posted := c.parkTextAgent(issue, comments)
	if parker == "" {
		return fmt.Sprintf("not withdrawn: no hive park text on %s#%d names the agent that parked it, so only a maintainer can lift it; say in a plain comment that the decision is no longer needed", req.Repo, req.Number), nil, nil
	}
	if !strings.EqualFold(parker, strings.TrimSpace(req.Agent)) {
		return fmt.Sprintf("not withdrawn: %s parked %s#%d, and only the agent that parked an issue may withdraw the ask; say in a plain comment that the decision is no longer needed", parker, req.Repo, req.Number), nil, nil
	}
	return "", &parkWithdrawal{issue: issue, comments: comments, posted: posted}, nil
}

// renamedParkLabel is the operator-renamed decision label the issue wears,
// which the watcher removes itself (the engine writes only stage labels),
// or "".
func (c *Client) renamedParkLabel(w *parkWithdrawal) string {
	label := c.needsDecisionLabel()
	if strings.EqualFold(label, NeedsDecisionStageLabel) || !labelPresent(issueLabelNames(w.issue.Labels), label) {
		return ""
	}
	return label
}

// withdrawDecisionPark lifts the park once the withdrawal comment is up and
// any renamed decision label is off: the stage engine takes 045 off on the
// Unparked fact (the same fact the label relay's downgrade submits). The
// "What to reply" notice goes too, through the write-back gate the notice
// move uses: it would otherwise keep offering the options the agent just
// withdrew.
func (c *Client) withdrawDecisionPark(ctx context.Context, req IssueRequest, w *parkWithdrawal) error {
	issue, comments := w.issue, w.comments
	if err := c.StageFacts().Unparked(ctx, c.IssueStageRef(req.Repo, req.Number), req.Agent); err != nil {
		return err
	}
	owner, name := c.splitRepo(req.Repo)
	item := c.WritebackItemForIssue(Issue{Number: req.Number, Author: safeGetLogin(issue.GetUser()), Labels: issueLabelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time})
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) || !strings.Contains(cm.GetBody(), unparkNoticeMarker) {
			continue
		}
		if d := c.WritebackAllowed(req.Repo, item, writeback.KindNeedsDecisionComment, cm.GetBody()); !d.Allowed {
			return nil
		}
		if _, err := c.client.Issues.DeleteComment(ctx, owner, name, cm.GetID()); err != nil && !githubStatusError(err, http.StatusNotFound) { // writeback:gated
			return fmt.Errorf("deleting the unpark notice on %s#%d: %w", req.Repo, req.Number, err)
		}
	}
	return nil
}

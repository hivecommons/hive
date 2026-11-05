package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A decision park that defers to another one ("Answer on #511; the same
// choice applies here") is linked "blocked by" that issue, so the owner is
// asked once (hivecommons/hive#16694). When the owner answers on #N, the
// deferring park takes the same answer: without this it kept its decision
// label and came back as a fresh ask, with the same "Answer on #N" body,
// once #N was unparked or closed (hivecommons/hive#16699).

// decisionInheritedMarker tags the note left on a park that took the
// decision recorded on its blocker.
const decisionInheritedMarker = "<!-- hive:decision-inherited -->"

// isNeedsDecisionLabel reports whether label is a needs-decision park label
// (narrower than isDecisionParkLabel, which also counts needs-human),
// under any spelling the hive writes or the operator configured.
func (c *Client) isNeedsDecisionLabel(label string) bool {
	for _, want := range []string{c.needsDecisionLabel(), NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel} {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return c.configuredParkingBucket(label) == issueNeedsDecisionLabel
}

// onlyDecisionParked reports whether every label parking the issue is a
// decision label: a park that also waits on something else (needs-human,
// blocked) is not answered by another issue's decision.
func (c *Client) onlyDecisionParked(labels []string) bool {
	parking := c.parkingLabelsPresent(labels)
	for _, label := range parking {
		if !c.isNeedsDecisionLabel(label) {
			return false
		}
	}
	return len(parking) > 0
}

// hasNeedsDecisionLabel reports whether any label is a needs-decision park
// label.
func (c *Client) hasNeedsDecisionLabel(labels []string) bool {
	for _, label := range labels {
		if c.isNeedsDecisionLabel(label) {
			return true
		}
	}
	return false
}

// decisionToInherit returns the decision a deferring park may take from the
// un-park event on its blocker, or false. Only a decision counts: a
// `/hive done` ("the owner-only step is done") or a release approval
// answers the blocker's own owner step, not the question the deferring
// park asked. The blocker must have been a decision park when this pass
// read it (its labels before the un-park), so a `/hive approve` clearing a
// `blocked` or `needs-human` park never stands in for a decision.
func (c *Client) decisionToInherit(from IssueUnparkEvent, blocker *gh.Issue) (IssueUnparkEvent, bool) {
	if blocker == nil || blocker.IsPullRequest() || from.ReleaseTag != "" || from.ReleaseURL != "" {
		return IssueUnparkEvent{}, false
	}
	if strings.TrimSpace(from.Decision) == "" || from.Decision == unparkDoneDecision {
		return IssueUnparkEvent{}, false
	}
	if !c.hasNeedsDecisionLabel(issueLabelNames(blocker.Labels)) {
		return IssueUnparkEvent{}, false
	}
	return from, true
}

// inheritDeferredDecisions un-parks the open decision parks in one repo
// whose body defers to their only open "blocked by" issue (askDefersTo:
// "Answer on #N; the same choice applies here") when that issue was a
// decision park this pass un-parked on a maintainer's decision (decided,
// by number). Each takes the same actor and decision and gets one note
// saying where it was answered. A park whose only blocker is a genuine
// dependency, or whose blocker was answered with `/hive done` or a release
// approval, stays its own ask. A failed read or write skips that issue: it
// stays an ask of its own, as before. The pass's maxActions budget counts
// each inherited un-park like any other write.
func (c *Client) inheritDeferredDecisions(ctx context.Context, displayRepo, owner, repo string, issues []*gh.Issue, decided map[int]IssueUnparkEvent, maxActions int, result *IssueUnparkSweepResult, audit func(IssueUnparkEvent)) {
	byNumber := make(map[int]*gh.Issue, len(issues))
	for _, issue := range issues {
		if issue != nil {
			byNumber[issue.GetNumber()] = issue
		}
	}
	for _, issue := range issues {
		if maxActions > 0 && result.actions() >= maxActions {
			return
		}
		if issue == nil || issue.IsPullRequest() {
			continue
		}
		number := issue.GetNumber()
		labels := issueLabelNames(issue.Labels)
		if _, done := decided[number]; done || !c.onlyDecisionParked(labels) {
			continue
		}
		blockers, err := c.fetchBlockedBy(ctx, owner, repo, number)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("unpark sweep: could not read a decision park's \"blocked by\" dependencies; it stays its own ask", "repo", displayRepo, "issue", number, "error", err)
			}
			continue
		}
		var open []blockerRef
		for _, b := range blockers {
			if b.State != "closed" {
				open = append(open, b)
			}
		}
		if len(open) != 1 || !strings.EqualFold(open[0].Owner, owner) || !strings.EqualFold(open[0].Repo, repo) {
			continue
		}
		from, ok := decided[open[0].Number]
		if !ok {
			continue
		}
		if from, ok = c.decisionToInherit(from, byNumber[open[0].Number]); !ok {
			continue
		}
		if !askDefersTo(issue.GetBody(), open[0].Number) {
			continue
		}
		removed, err := c.clearIssueParkingLabels(ctx, owner, repo, number, labels, from.Actor, false)
		if err != nil {
			if c.logger != nil {
				c.logger.Warn("unpark sweep: could not lift a deferring decision park", "repo", displayRepo, "issue", number, "blocker", from.Number, "error", err)
			}
			continue
		}
		note := renderDecisionInheritedNote(from, removed)
		// The note is unsolicited, so it consults the gate like the other
		// decision-park comments; a refusal keeps the un-park and skips it.
		item := c.WritebackItemForIssue(Issue{Number: number, Author: safeGetLogin(issue.GetUser()), Labels: labels, CreatedAt: issue.GetCreatedAt().Time})
		if d := c.WritebackAllowed(owner+"/"+repo, item, writeback.KindNeedsDecisionComment, note); !d.Allowed {
			if c.logger != nil {
				c.logger.Info("unpark sweep: write-back gate withheld the inherited-decision note", "repo", displayRepo, "issue", number, "reason", d.Reason)
			}
		} else if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(note)}); err != nil && c.logger != nil { // writeback:gated
			c.logger.Warn("unpark sweep: could not post the inherited-decision note", "repo", displayRepo, "issue", number, "error", err)
		}
		event := IssueUnparkEvent{Repo: displayRepo, Number: number, Actor: from.Actor, Decision: from.Decision, CommentURL: from.CommentURL}
		result.Unparked = append(result.Unparked, event)
		if audit != nil {
			audit(event)
		}
		if c.logger != nil {
			c.logger.Info("decision park inherited its blocker's decision", "repo", displayRepo, "issue", number, "blocker", from.Number, "actor", from.Actor)
		}
	}
}

func renderDecisionInheritedNote(from IssueUnparkEvent, removed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\nAnswered on #%d by @%s. Decision: %s\n\nThis issue deferred to that decision, so it takes the same answer", decisionInheritedMarker, from.Number, from.Actor, from.Decision)
	if len(removed) > 0 {
		fmt.Fprintf(&b, " and %s came off", quoteLabelsProse(removed))
	}
	fmt.Fprintf(&b, ". It still waits for #%d to close before it is picked up.", from.Number)
	return b.String()
}

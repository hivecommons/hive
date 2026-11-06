package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// A hive agent that answers an issue without a pull request ("no code
// change can fix this; owner's call") is told to park it through
// `hive-open-issue comment … --needs-decision`. When it posts a plain
// comment instead, nothing parks the issue: it stays at 010 "waiting for the
// scanner to pick it up" while the scanner, having answered, never comes
// back — off Your Turn and with no `/hive` options (hivecommons/hive#16947,
// Danathar/atomic-image-builder#834 sat there five days).
//
// The issue stage sweep is the safety net. An issue at 010 that has stalled
// there (no movement for the stage's dwell), has no claim, PR or children,
// is not held, and whose newest word is the hive's own agent speaking after
// the issue reached 010, is parked for the owner the way the relay parks a
// `--needs-decision` comment: a note with the options, the decision label
// and the Parked fact. The unpark sweep then posts the "What to reply" menu,
// and the issue is on Your Turn. Waiting for the stall keeps the net off an
// issue an agent is still working: a claim or a PR in that time stages it
// elsewhere.

// noPRParkMarker tags the hive's park note, so a second pass over the same
// stall does not post it again.
const noPRParkMarker = "<!-- hive:no-pr-park -->"

// noPRParkRecheck bounds how often one stalled 010 issue has its comments
// read: the stall persists for days, and every read is an API call.
const noPRParkRecheck = time.Hour

// noPRParkChecks remembers when each stalled issue was last looked at.
type noPRParkChecks struct {
	mu sync.Mutex
	at map[string]time.Time
}

// due reports whether key may be checked at now, and records the check.
func (n *noPRParkChecks) due(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.at[key]; ok && now.Sub(last) < noPRParkRecheck {
		return false
	}
	if n.at == nil {
		n.at = map[string]time.Time{}
	}
	n.at[key] = now
	return true
}

// noPRParkCandidate: an actionable issue nothing is working on.
func noPRParkCandidate(in IssueStageInput) bool {
	return in.Bucket == "" && in.ClaimKind == "" && in.ClaimedBy == "" && !in.PRStarted && in.Claim == nil &&
		!in.Held && !in.DelegatesWork && (in.Children == nil || in.Children.Total == 0)
}

// hiveMachineryComment reports whether body is one of the hive's own
// bookkeeping comments (status card, menus, notes): any `<!-- hive:… -->`
// marker other than the origin stamp an agent's relayed comment carries.
func hiveMachineryComment(body string) bool {
	rest := body
	for {
		i := strings.Index(rest, "<!-- hive:")
		if i < 0 {
			return false
		}
		if !strings.HasPrefix(rest[i:], "<!-- hive:origin ") {
			return true
		}
		rest = rest[i+len("<!-- hive:"):]
	}
}

// noPRParkNote is the park note: why the issue waits on the owner, and the
// options the "What to reply" menu offers.
func noPRParkNote(answerURL string) string {
	answer := "answered this issue"
	if u := strings.TrimSpace(answerURL); u != "" {
		answer = "[answered this issue](" + u + ")"
	}
	return noPRParkMarker + "\n" +
		"**Parked for your decision.** The hive " + answer + " without opening a pull request, and nothing has moved here since. " +
		"It will not pick the issue up again on its own, so the next step is yours.\n\n" +
		"Option A: close it; nothing in this repository needs to change.\n" +
		"Option B: keep it open and send the hive back to it with your instructions (`/hive decision <instructions>`).\n"
}

// parkUnansweredNoPRIssue parks in for the owner when its newest word is the
// hive agent's answer posted after the issue reached 010 (rec.EnteredAt).
// Reports whether it parked. The caller has checked the stage (010), the
// stall and noPRParkCandidate.
func (c *Client) parkUnansweredNoPRIssue(ctx context.Context, in IssueStageInput, rec stagelabels.Record, now time.Time) (bool, error) {
	owner, name := c.splitRepo(in.Repo)
	if !c.noPRParkChecks.due(stagelabels.Key(owner+"/"+name, in.Number), now) {
		return false, nil
	}
	comments, err := c.listIssueComments(ctx, owner, name, in.Number)
	if err != nil {
		return false, err
	}
	answerURL, noted := "", false
	var newest time.Time
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		body, at := cm.GetBody(), cm.GetCreatedAt().Time
		if c.isHiveAccount(cm.GetUser()) && strings.Contains(body, noPRParkMarker) && !at.Before(rec.EnteredAt) {
			noted = true
		}
		if c.isHiveAccount(cm.GetUser()) && hiveMachineryComment(body) {
			continue
		}
		if at.Before(newest) {
			continue
		}
		newest = at
		answerURL = ""
		if c.isHiveAccount(cm.GetUser()) && !at.Before(rec.EnteredAt) {
			answerURL = cm.GetHTMLURL()
			if answerURL == "" {
				answerURL = "#issuecomment-" + fmt.Sprint(cm.GetID())
			}
		}
	}
	if answerURL == "" {
		// Nobody spoke, a person spoke last, or the hive's word predates
		// this stay at 010: the scanner's move stands.
		return false, nil
	}
	note := noPRParkNote(answerURL)
	item := c.WritebackItemForIssue(Issue{Number: in.Number, Labels: in.Labels})
	if d := c.WritebackAllowed(in.Repo, item, writeback.KindNeedsDecisionComment, note); !d.Allowed {
		return false, nil
	}
	// The note first: it carries the options the menu offers, and a retry
	// after a failed park finds it and parks without posting it again.
	if !noted {
		if err := c.CreateIssueComment(ctx, in.Repo, in.Number, note); err != nil { // writeback:gated
			return false, err
		}
	}
	// The park itself, as the relay's --needs-decision comment does it.
	_, parkLabel, _ := c.partitionStageRequestLabels([]string{c.needsDecisionLabel()})
	if !stagelabels.IsStageLabel(strings.ToLower(parkLabel)) {
		if err := c.AddLabels(ctx, in.Repo, in.Number, []string{parkLabel}); err != nil { // writeback:gated
			return false, err
		}
	}
	if err := c.StageFacts().Parked(ctx, c.IssueStageRef(in.Repo, in.Number), parkLabel); err != nil {
		return false, err
	}
	if err := c.verifyIssueParkLanded(ctx, in.Repo, in.Number, parkLabel); err != nil {
		return false, err
	}
	return true, nil
}

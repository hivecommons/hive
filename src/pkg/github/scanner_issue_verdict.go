package github

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// A scanner "not agent work" verdict on an issue changes the issue's state
// (hivecommons/hive#18675). Before this the verdict was only a comment: the
// issue kept stage/010-actionable and `help wanted`, the dashboard showed it
// Actionable for hours, and the scanner picked it up again and posted the
// same triage every pass (#18636 was triaged three times in one morning with
// the same "not a code defect" verdict; #18421 and #18659 stayed Actionable
// after "not startable by an agent").
//
// The relay is the enforcement point, as for the scanner's PR notes
// (scanner_note.go): every agent comment goes through it. A scanner comment
// on an open issue that says the issue is not agent work
//
//   - parks the issue on a person with `needs-human` (a Parked fact for the
//     stage engine, so it leaves stage/010-actionable; the enumeration
//     hard-suppresses it, so the next scanner pass does not see it; the
//     claim gate refuses it) and takes `help wanted` off;
//   - posts the verdict once, carrying ScannerVerdictMarker. A later verdict
//     on the same issue with no human comment since the marked one is
//     dropped: nothing new was said to triage against.
//
// A person's reply (`/hive approve`, `/hive decision`) or removing
// `needs-human` puts the issue back in the queue through the existing
// un-park paths, and a human comment re-arms the verdict comment.

// ScannerVerdictMarker tags the scanner's "not agent work" verdict comment
// on an issue.
const ScannerVerdictMarker = "<!-- hive:scanner-verdict -->"

// scannerNotAgentWork matches a scanner comment that concludes the issue is
// not agent work: one that opens (after markdown and an optional
// "Scanner:", "Status:", "Update:" or "Triage (…):" label) with
// `needs-human` or "not a [code] defect/bug", or that says anywhere "not
// started / not startable by an agent", "not agent work",
// "not a code defect", "needs a human", "out of the [agent] work queue" or
// "no PR to open". A decision park (`needs-decision`) is the agent's own
// stage/045 label request and a blocked-on-#N deferral has its own marker,
// so neither is matched here.
var scannerNotAgentWork = regexp.MustCompile(`(?i)` +
	`^\W*(?:(?:scanner|status|update|triage(?:\s*\([^)]*\))?)\s*:\W*)?(?:` +
	`needs-human\b` +
	`|not\s+a\s+(?:\S+\s+){0,2}?(?:code\s+)?(?:defect|bug)\b` +
	`)` +
	`|\bnot\s+start(?:ed|able)\b[^.\n]{0,40}?\bby\s+(?:an?\s+)?(?:hive\s+)?agents?\b` +
	`|\bnot\s+agent\s+work\b` +
	`|\bnot\s+a\s+code\s+defect\b` +
	`|\bneeds\s+a\s+human\b` +
	`|\bout\s+of\s+the\s+(?:agent\s+)?work\s+queue\b` +
	`|\bno\s+PR\s+to\s+open\b`)

// scannerIssueVerdict reports whether body, a relayed comment, is a "not
// agent work" verdict. The identity footer and fenced code (quoted
// material) are ignored, and a statement that work starts now overrides.
func scannerIssueVerdict(body string) bool {
	note := strings.ReplaceAll(body, "\r\n", "\n")
	note = attributionIdentityFooter.ReplaceAllString(note, "")
	note = strings.TrimSpace(prDeferralFence.ReplaceAllString(note, " "))
	return note != "" && !commentSaysStartNow(note) && scannerNotAgentWork.MatchString(note)
}

// scannerVerdictStands reports whether the latest trusted verdict comment
// among comments has had no human comment after it.
func (c *Client) scannerVerdictStands(comments []*gh.IssueComment) bool {
	stands := false
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		if strings.Contains(cm.GetBody(), ScannerVerdictMarker) && c.markerAuthorTrusted(cm.GetUser()) {
			stands = true
			continue
		}
		if stands && c.isHumanAuthor(cm.GetUser()) {
			stands = false
		}
	}
	return stands
}

// relayScannerIssueVerdict delivers a scanner "not agent work" verdict on an
// open issue: park it on a person, then post the verdict unless the same
// triage already stands (see the file comment). The park is written before
// the comment so a retry after a failed write still parks; the comment is
// the last write. handled is false when the caller should post the comment
// the usual way: another lane, a blocked-on-PR park, a body with a dedicated
// marker, no verdict, a pull request, or a closed or missing issue.
func (c *Client) relayScannerIssueVerdict(ctx context.Context, req IssueRequest, body string, resp *IssueResponse) (handled bool, err error) {
	if c == nil || c.client == nil || req.Number <= 0 || !isScannerAgent(req.Agent) || req.BlockedOnPR > 0 ||
		!scannerNoteCandidate(body) || !scannerIssueVerdict(body) {
		return false, nil
	}
	owner, name := c.splitRepo(req.Repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, req.Number)
	if err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return false, nil
		}
		return true, err
	}
	if issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "open") {
		return false, nil
	}
	comments, err := c.listIssueComments(ctx, owner, name, req.Number)
	if err != nil {
		return true, err
	}
	if c.scannerVerdictStands(comments) {
		c.logger.Info("scanner verdict: same triage already stands with no human reply since, not re-posting",
			slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("agent", req.Agent))
		resp.AlreadyExisted = true
		c.noteCommentSuppressed()
		return true, nil
	}
	labels := extractLabels(issue.Labels)
	if !hasIssueNeedsHumanLabel(labels) {
		if err := c.addLabelsObserved(ctx, req.Repo, req.Number, []string{issueNeedsHumanLabel}, observedState(issue.GetUpdatedAt().Time)); err != nil { // writeback:solicited the scanner's verdict parks the issue (#18675)
			return true, err
		}
	}
	if sink := c.StageFacts(); sink != nil {
		if err := sink.Parked(ctx, c.IssueStageRef(req.Repo, req.Number), issueNeedsHumanLabel); err != nil {
			return true, err
		}
	}
	if labelPresent(labels, helpWantedLabel) {
		if err := stripHelpWanted(ctx, c, req.Repo, req.Number); err != nil {
			return true, err
		}
	}
	if err := c.CreateIssueCommentBudgeted(ctx, req.Repo, req.Number, ScannerVerdictMarker+"\n"+body); err != nil { // writeback:solicited the scanner asked for this comment (#18675)
		return true, err
	}
	return true, nil
}

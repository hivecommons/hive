package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/sentinel"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Head-scoped sentinel results (hivecommons/hive#19262).
//
// A sentinel finding belongs to the head it was raised on. Before this, a PR
// that already wore the alert label was never evaluated again, so a finding
// on an early head (a Makefile edit, say) kept the label, the "Needs human ·
// sentinel" dashboard state and the status section's "Head <old> matched"
// line on every later head, even a docs-only one. Now every new head is
// evaluated; when the current head matches no rule, the label is removed if
// and only if the hive itself put it there for an earlier head's finding, and
// the hive's sentinel comment and status section say the current head is
// clean. The earlier finding stays in the /api/sentinel ring, the review
// evidence bundle for that head and the audit log.
const (
	// sentinelCleanHeading opens the hive's sentinel comment once the current
	// head is clean. It deliberately differs from sentinelAlertHeading so
	// sentinelClearedByMaintainer never reads a clean record as a cleared
	// alert.
	sentinelCleanHeading = "## ✅ Sentinel: current head is clean"
	// sentinelEarlierAlertSummary titles the collapsed copy of the earlier
	// alert kept in the clean comment for audit.
	sentinelEarlierAlertSummary = "Earlier sentinel alert, kept for audit"
	// sentinelEarlierAlertTitle replaces the alert heading inside that copy,
	// for the same reason sentinelCleanHeading differs from it.
	sentinelEarlierAlertTitle = "**Sentinel alert for head `%s`**"
	// SentinelAlertClearedAuditAction is the audit-log action for a label the
	// sweep removed because the current head is clean.
	SentinelAlertClearedAuditAction = "sentinel-alert-cleared"
)

// sentinelMarkerHead returns the head SHA named by the first
// `<!-- hive-sentinel --> <sha>` marker in body, or "".
func sentinelMarkerHead(body string) string {
	_, rest, ok := strings.Cut(body, SentinelMarker+" ")
	if !ok {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// SentinelCleanComment is the body the hive's sentinel comment is rewritten
// to when head matches no rule after an earlier head was alerted on. It
// carries the marker for head (so a restart finds it and the head stays
// judged) and a collapsed copy of the earlier alert, with its marker and
// heading neutralised, for audit.
func SentinelCleanComment(head, label, priorHead, priorBody string) string {
	var b strings.Builder
	b.WriteString(SentinelMarker + " " + head + "\n")
	b.WriteString(sentinelCleanHeading + "\n\n")
	fmt.Fprintf(&b, "Head `%s` matches no sentinel rule, so Hive removed the `%s` label it added for the finding on an earlier head (`%s`). Hive evaluates every new push again; the earlier finding stays in the dashboard Security tab and `/api/sentinel`.\n", shortSHA(head), label, shortSHA(priorHead))
	earlier := sentinelEarlierAlertBody(priorBody, priorHead)
	if earlier != "" {
		fmt.Fprintf(&b, "\n<details><summary>%s</summary>\n\n%s\n\n</details>\n", sentinelEarlierAlertSummary, earlier)
	}
	return b.String()
}

// sentinelEarlierAlertBody is priorBody without its marker lines and with
// the alert heading replaced, so the copy can never be read back as the
// hive's alert for any head.
func sentinelEarlierAlertBody(priorBody, priorHead string) string {
	lines := strings.Split(priorBody, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, SentinelMarker) {
			continue
		}
		if strings.Contains(line, sentinelAlertHeading) {
			line = fmt.Sprintf(sentinelEarlierAlertTitle, shortSHA(priorHead))
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// SentinelCleanStatusSection is the quiet-mode `sentinel` status section for
// a head that matches no rule after an earlier head's notice. It carries the
// notice marker for head so the section always names the head it describes.
func SentinelCleanStatusSection(head string) string {
	return fmt.Sprintf("%s %s -->\n✅ **Sentinel:** head `%s` matches no sentinel rule. An earlier head's finding stays in the dashboard Security tab and `/api/sentinel`.", SentinelNoticeCardMarker, head, shortSHA(head))
}

// markSentinelNoticeSectionClean rewrites an existing quiet-mode sentinel
// section that describes an earlier head so it says head is clean. It never
// creates a status card or a section: a PR the sentinel never noticed stays
// untouched. Best effort, like clearSentinelNoticeSection.
func (c *Client) markSentinelNoticeSectionClean(ctx context.Context, repo string, number int, head string) {
	if !c.notificationsQuiet() || strings.TrimSpace(head) == "" {
		return
	}
	render := func(previous string) string {
		if strings.TrimSpace(previous) == "" || strings.Contains(previous, SentinelNoticeCardMarker+" "+head+" ") {
			return previous
		}
		return SentinelCleanStatusSection(head)
	}
	if _, err := c.UpsertPRStatusSectionFunc(ctx, repo, number, SentinelStatusKey, render); err != nil {
		c.logger.Warn("sentinel: marking status-card section clean failed", "repo", repo, "number", number, "error", err)
	}
}

// sentinelLabelAddedByHive reports whether the newest label event for label
// on the PR is a `labeled` by one of the hive's own accounts at or before
// notAfter (the hive's alert comment's last write: the sweep adds the label
// and then writes the comment). A label a person added, directly or through
// the dashboard after the alert, fails one of those tests and is left alone.
// A hive that cannot name its accounts, or a zero notAfter, never matches:
// not knowing who added the label is never a reason to remove it.
func (c *Client) sentinelLabelAddedByHive(ctx context.Context, owner, repo string, number int, label string, notAfter time.Time) (bool, error) {
	if !c.knowsOwnAccounts() || notAfter.IsZero() {
		return false, nil
	}
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	var (
		lastKind  string
		lastActor *gh.User
		lastAt    time.Time
	)
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("reading timeline of %s/%s#%d for sentinel label provenance: %w", owner, repo, number, err)
		}
		for _, ev := range page {
			if ev == nil {
				continue
			}
			kind := strings.ToLower(ev.GetEvent())
			if kind != "labeled" && kind != "unlabeled" {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(ev.GetLabel().GetName()), label) {
				continue
			}
			at := ev.GetCreatedAt().Time
			if at.IsZero() || at.Before(lastAt) {
				continue
			}
			lastKind, lastActor, lastAt = kind, ev.GetActor(), at
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if lastKind != "labeled" || lastActor == nil || !c.isHiveAccount(lastActor) {
		return false, nil
	}
	return !lastAt.After(notAfter), nil
}

// sentinelCommentWrittenAt is the comment's last write time: updated_at,
// falling back to created_at.
func sentinelCommentWrittenAt(comment *gh.IssueComment) time.Time {
	if comment == nil {
		return time.Time{}
	}
	if at := comment.GetUpdatedAt().Time; !at.IsZero() {
		return at
	}
	return comment.GetCreatedAt().Time
}

// clearStaleSentinelAlert handles a labelled PR whose current head matches
// no rule. It removes the label only when all of these hold: the hive can
// name its own accounts, the hive's own sentinel comment is a blocking alert
// for an earlier head, and the newest label event is the hive's own
// `labeled`, made no later than that comment. It then rewrites the comment
// (SentinelCleanComment) and the quiet-mode status section. cleared is false,
// with nothing written, in every other case, including a write-back gate
// refusal.
func (c *Client) clearStaleSentinelAlert(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, label string) (SentinelSweepEvent, bool, error) {
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	if head == "" || !c.knowsOwnAccounts() {
		return SentinelSweepEvent{}, false, nil
	}
	prior, found, err := c.findOwnMarkerComment(ctx, owner, repo, number, SentinelMarker)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if !found || prior == nil || !strings.Contains(prior.GetBody(), sentinelAlertHeading) {
		return SentinelSweepEvent{}, false, nil
	}
	priorHead := sentinelMarkerHead(prior.GetBody())
	if priorHead == "" || strings.EqualFold(priorHead, head) {
		return SentinelSweepEvent{}, false, nil
	}
	hiveAdded, err := c.sentinelLabelAddedByHive(ctx, owner, repo, number, label, sentinelCommentWrittenAt(prior))
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if !hiveAdded {
		return SentinelSweepEvent{}, false, nil
	}
	body := SentinelCleanComment(head, label, priorHead, prior.GetBody())
	if d := c.WritebackAllowed(displayRepo, c.sentinelWritebackItem(pr), writeback.KindSentinel, body); !d.Allowed {
		return SentinelSweepEvent{}, false, nil
	}
	resp, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label) // writeback:gated
	c.noteWritebackOutcome(displayRepo, resp, err)
	if err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("removing %s label from a clean head: %w", label, err)
	}
	if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, prior.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel comment for a clean head: %w", err)
	}
	c.noteCommentEdited()
	c.markSentinelNoticeSectionClean(ctx, displayRepo, number, head)
	return SentinelSweepEvent{
		Number:      number,
		Author:      pr.GetUser().GetLogin(),
		Title:       pr.GetTitle(),
		HeadSHA:     head,
		Cleared:     true,
		ClearedHead: priorHead,
	}, true, nil
}

// trySentinelLabelledHead evaluates a new head of a PR that already wears the
// alert label on the blocking path. A clean head goes to
// clearStaleSentinelAlert. A head with findings keeps the label (it is
// already on, so nothing is added) and the hive's sentinel comment is
// rewritten as the alert for this head, so the explanation always names the
// current head. A head the hive already wrote a sentinel comment for, or a
// labelled PR with no sentinel comment of the hive's (the label came from
// somewhere else), is left as it is.
func (c *Client) trySentinelLabelledHead(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, label string, decision SentinelDecision, opts SentinelSweepOptions) (SentinelSweepEvent, bool, error) {
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	author := pr.GetUser().GetLogin()
	files, err := listPRFilesWithPatch(ctx, c.client, owner, repo, pr)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if opts.PatchObserver != nil {
		c.observeSentinelPatch(ctx, owner, repo, pr, files, opts.PatchObserver)
	}
	findings := sentinel.Evaluate(sentinel.PR{Author: author, Title: pr.GetTitle(), Files: files}, opts.Evaluator)
	if len(findings) == 0 {
		return c.clearStaleSentinelAlert(ctx, displayRepo, owner, repo, pr, label)
	}
	body := SentinelComment(decision, head, author, findings)
	event := SentinelSweepEvent{Number: number, Author: author, Title: pr.GetTitle(), HeadSHA: head, Findings: findings, Blocked: decision.Block, TrustedReason: decision.TrustedReason}
	if d := c.WritebackAllowed(displayRepo, c.sentinelWritebackItem(pr), writeback.KindSentinel, body); !d.Allowed {
		event.Suppressed = true
		event.SuppressedReason = d.Reason
		return event, true, nil
	}
	target, posted, err := c.findOwnMarkerComment(ctx, owner, repo, number, SentinelMarker+" "+head)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if posted || !c.knowsOwnAccounts() {
		// The hive already wrote for this head, or cannot tell its own
		// comment from a forged one: the label holds, nothing to rewrite.
		return SentinelSweepEvent{}, false, nil
	}
	if target, posted, err = c.findOwnMarkerComment(ctx, owner, repo, number, SentinelMarker); err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if !posted || target == nil {
		return SentinelSweepEvent{}, false, nil
	}
	if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, target.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel alert for a new head: %w", err)
	}
	c.noteCommentEdited()
	c.clearSentinelNoticeSection(ctx, displayRepo, number)
	return event, true, nil
}

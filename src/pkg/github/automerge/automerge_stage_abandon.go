package automerge

// The stale contributor PR idle clock (governor.labels.stage.abandon_after)
// and its optional per-repository close (abandon_close_repos).
//
// A contributor PR parked at 071 needs-author or at an adoption-stalled 085
// used to get one note and then nothing, forever. The stage pass now runs
// the idle clock (stagelabels.Record.AbandonStep) after it notes the PR's
// activity: past the threshold the record is flagged abandoned, which the
// dashboard reads as "abandoned Nd: <blocker>" in a "Stale: close or take
// over" group, with a recommend-only delegate rule. The stage and the
// reason are untouched.
//
// The flag costs no extra reads in the common case: the clock runs on the
// activity the store already keeps (LastActivityAt, History). Only when a
// PR first crosses the threshold on a cycle that did not fetch its comment
// and review counts does the pass read them once (issue comments since the
// idle clock started, plus the PR's reviews: two calls), so a reply the
// counts never saw cannot be mistaken for silence.
//
// Closing is opt-in per repository and ACMM L6 only, mirroring
// supersession_sweep.close_contributor_prs (hivecommons/hive#11431): a
// warning notice once, a grace (config.AbandonCloseGrace), then a close with
// a comment that says how to reopen — and only when nobody but the hive
// acted since the notice, no hold label is on the PR and no keep-open label
// (hive/keep-open, the supersession sweep's) says otherwise.

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

const (
	// AbandonNoticeMarker opens the warning notice posted on an abandoned
	// PR in a repository that closes them.
	AbandonNoticeMarker = "<!-- hive:stage-abandoned-notice"
	// AbandonCloseMarker opens the comment the hive closes it with.
	AbandonCloseMarker = "<!-- hive:stage-abandoned-close"
)

// reconcileAbandoned runs the idle clock for one PR once this pass noted
// its activity. contributor is false for the hive's own PRs, which never
// carry the flag (a stray one is cleared).
func (c *Engine) reconcileAbandoned(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, contributor bool, now time.Time) {
	rec, ok := store.Get(key)
	if !ok {
		return
	}
	after := policy.AbandonAfter
	if !contributor {
		after = 0
	}
	countsKnown := obs.Detail.CommentsKnown && obs.Detail.ReviewsKnown
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "stage", rec.Stage.String(), "reason", rec.Reason}
	switch rec.AbandonStep(after, countsKnown, now) {
	case stagelabels.AbandonClear:
		if store.ClearAbandoned(key) {
			c.info("stage labels: abandoned flag cleared", logArgs...)
		}
	case stagelabels.AbandonFlag:
		if store.MarkAbandoned(key, now) {
			c.info("stage labels: contributor PR flagged abandoned", append(logArgs, "idle_since", rec.IdleSince(), "abandon_after", after.String())...)
		}
	case stagelabels.AbandonConfirm:
		since := rec.IdleSince()
		at, err := c.lastOutsideActivitySince(ctx, obs, policy, since)
		if err != nil {
			store.NoteAbandonCheck(key, time.Time{}, now)
			c.warn("stage labels: abandon confirming read failed; retried later", append(logArgs, "error", err)...)
			return
		}
		store.NoteAbandonCheck(key, at, now)
		if !at.IsZero() {
			c.debug("stage labels: abandon confirming read found activity; idle clock restarted", append(logArgs, "activity_at", at)...)
			return
		}
		if store.MarkAbandoned(key, now) {
			c.info("stage labels: contributor PR flagged abandoned (confirmed)", append(logArgs, "idle_since", since, "abandon_after", after.String())...)
		}
	}
}

// outsideActor reports whether a comment or review by u counts as someone
// other than the hive acting: not the App, not one of the hive's own
// identities (policy.HiveAuthor), and not another bot.
func (c *Engine) outsideActor(policy StageLabelPolicy, u *gh.User) bool {
	login := strings.TrimSpace(u.GetLogin())
	if login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if c.transport != nil && normalizeBotLogin(login) == normalizeBotLogin(c.transport.AppBotLogin()) {
		return false
	}
	if policy.HiveAuthor != nil && policy.HiveAuthor(login) {
		return false
	}
	return true
}

// lastOutsideActivitySince is the one confirming read: the latest issue
// comment or review submission on the PR after since by someone other than
// the hive (zero when there is none). Issue comments are listed with
// GitHub's since filter; reviews are one page, plus the last page when there
// are more, so the newest are always seen.
func (c *Engine) lastOutsideActivitySince(ctx context.Context, obs stageObservation, policy StageLabelPolicy, since time.Time) (time.Time, error) {
	var latest time.Time
	note := func(at time.Time, u *gh.User) {
		if at.After(since) && at.After(latest) && c.outsideActor(policy, u) {
			latest = at
		}
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	if !since.IsZero() {
		opts.Since = gh.Ptr(since)
	}
	comments, _, err := c.gh.Issues.ListComments(ctx, obs.Owner, obs.Repo, obs.Number, opts)
	if err != nil {
		return time.Time{}, fmt.Errorf("listing comments for %s#%d: %w", obs.Display, obs.Number, err)
	}
	for _, cm := range comments {
		note(cm.GetCreatedAt().Time, cm.GetUser())
	}
	reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, obs.Owner, obs.Repo, obs.Number, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return time.Time{}, fmt.Errorf("listing reviews for %s#%d: %w", obs.Display, obs.Number, err)
	}
	if resp != nil && resp.LastPage > 1 {
		more, _, err := c.gh.PullRequests.ListReviews(ctx, obs.Owner, obs.Repo, obs.Number, &gh.ListOptions{PerPage: 100, Page: resp.LastPage})
		if err != nil {
			return time.Time{}, fmt.Errorf("listing reviews for %s#%d: %w", obs.Display, obs.Number, err)
		}
		reviews = append(reviews, more...)
	}
	for _, r := range reviews {
		note(r.GetSubmittedAt().Time, r.GetUser())
	}
	return latest, nil
}

// abandonCloseHeld reports why an abandoned PR must not be closed even in
// a repository that opted in: a hold label (any the transport's hold set
// knows, the decision-needed gate, or a hold the pass read provenance for),
// or the keep-open label a person puts on a PR the hive wanted to close.
func (c *Engine) abandonCloseHeld(obs stageObservation) string {
	switch {
	case c.isHeld(obs.Labels):
		return "held"
	case wornHoldLabel(obs.Labels) != "":
		return "held: " + wornHoldLabel(obs.Labels)
	case obs.Hold != nil:
		return "held"
	case hasLabelFold(obs.Labels, hgithub.SupersessionKeepOpenLabel):
		return "keep-open: " + hgithub.SupersessionKeepOpenLabel
	}
	return ""
}

// closeAbandonedIfDue is the per-repository auto-close: post the warning
// notice once, then close after the grace if nobody but the hive acted
// since. It runs after the pass's stage write landed. It returns the number
// of comments posted.
func (c *Engine) closeAbandonedIfDue(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	if policy.AbandonClose == nil || obs.Merged {
		return 0, nil
	}
	rec, ok := store.Get(key)
	if !ok || !rec.Abandoned() || !rec.AbandonClosedAt.IsZero() {
		return 0, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	if !policy.AbandonClose(repo) {
		return 0, nil
	}
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "author", obs.Author, "reason", rec.Reason}
	if why := c.abandonCloseHeld(obs); why != "" {
		c.debug("stage labels: abandoned PR not closed", append(logArgs, "why", why)...)
		return 0, nil
	}
	grace := config.AbandonCloseGrace
	item := writebackItemForObservation(obs)
	if rec.AbandonNoticeAt.IsZero() {
		body := AbandonNoticeComment(obs, rec, grace, now)
		if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionNotice, body); !d.Allowed {
			c.info("stage labels: abandoned PR notice suppressed by the write-back gate", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
			return 0, nil
		}
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			c.warn("stage labels: abandoned PR notice failed", append(logArgs, "error", err)...)
			return 0, err
		}
		store.MarkAbandonNoticed(key, now)
		c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
		c.info("stage labels: abandoned PR warned before closing", append(logArgs, "grace", grace.String())...)
		return 1, nil
	}
	if !rec.AbandonCloseDue(grace, now) {
		return 0, nil
	}
	// A fresh read before the close: a reply the lane's counts never saw
	// keeps the PR open (and clears the flag on the next pass).
	at, err := c.lastOutsideActivitySince(ctx, obs, policy, rec.AbandonNoticeAt)
	if err != nil {
		c.warn("stage labels: abandoned PR close check failed; left open", append(logArgs, "error", err)...)
		return 0, err
	}
	if !at.IsZero() {
		store.NoteAbandonCheck(key, at, now)
		c.info("stage labels: abandoned PR not closed: activity since the notice", append(logArgs, "activity_at", at)...)
		return 0, nil
	}
	body := AbandonCloseComment(rec, grace, now)
	if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionClose, body); !d.Allowed {
		c.info("stage labels: abandoned PR close suppressed by the write-back gate, left open", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
		return 0, nil
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("stage labels: abandoned PR close comment failed; left open", append(logArgs, "error", err)...)
		return 0, err
	}
	c.noteOwnStageComment(obs.Owner, obs.Repo, obs.Number)
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
		c.warn("stage labels: closing the abandoned PR failed; left open", append(logArgs, "error", err)...)
		return 1, err
	}
	store.MarkAbandonClosed(key, now)
	c.info("stage labels: abandoned contributor PR closed as stale", logArgs...)
	return 1, nil
}

// abandonedAgeText renders how long rec has been idle at now ("9d").
func abandonedAgeText(rec stagelabels.Record, now time.Time) string {
	since := rec.AbandonedIdleSince
	if since.IsZero() {
		since = rec.IdleSince()
	}
	var idle time.Duration
	if !since.IsZero() && now.After(since) {
		idle = now.Sub(since)
	}
	return strings.TrimPrefix(stagelabels.AbandonedReasonText(idle, ""), "abandoned ")
}

// abandonedBlockerText is the blocker the park was for, in backticks.
func abandonedBlockerText(rec stagelabels.Record) string {
	blocker := strings.TrimSpace(rec.AbandonedBlocker)
	if blocker == "" {
		blocker = stagelabels.AbandonBlocker(rec.Reason)
	}
	if blocker == "" {
		return "a change only its author or a maintainer can make"
	}
	return "`" + strings.ReplaceAll(blocker, "`", "'") + "`"
}

// graceText renders the grace in hours or days.
func graceText(grace time.Duration) string {
	if grace >= 24*time.Hour && grace%(24*time.Hour) == 0 {
		if n := int(grace / (24 * time.Hour)); n > 1 {
			return fmt.Sprintf("%d days", n)
		}
		return "24 hours"
	}
	return grace.String()
}

// AbandonNoticeComment is the one warning before an abandoned PR is closed.
func AbandonNoticeComment(obs stageObservation, rec stagelabels.Record, grace time.Duration, now time.Time) string {
	return fmt.Sprintf("%s head=%s -->\n"+
		"This pull request has had no push, comment or review for %s while it waits on %s, and the hive cannot move it on its own. "+
		"This repository closes pull requests that stay idle like this, so unless someone comments or pushes here, the hive will close it as stale once %s have passed since this notice.\n\n"+
		"Comment or push to keep it open. A closed pull request can be reopened at any time.",
		AbandonNoticeMarker, obs.HeadSHA, abandonedAgeText(rec, now), abandonedBlockerText(rec), graceText(grace))
}

// AbandonCloseComment is the comment the hive closes an abandoned PR with.
func AbandonCloseComment(rec stagelabels.Record, grace time.Duration, now time.Time) string {
	return fmt.Sprintf("%s -->\n"+
		"Closing as stale: this pull request waited on %s with no push, comment or review for %s, and nobody replied within %s of the notice above. "+
		"Thank you for the work.\n\n"+
		"Reopen if that's wrong: reopen this pull request (or comment and a maintainer will), push, and the hive picks it up again.",
		AbandonCloseMarker, abandonedBlockerText(rec), abandonedAgeText(rec, now), graceText(grace))
}

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
// activity the store already keeps (AbandonActivityAt, History). Only when a
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
	"github.com/hivecommons/hive/pkg/authorkind"
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
// its activity. contributor is false for the hive's own PRs and a
// maintainer's, which never carry the flag (a stray one is cleared).
func (c *Engine) reconcileAbandoned(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, contributor bool, now time.Time) {
	rec, ok := store.Get(key)
	if !ok {
		return
	}
	after := abandonThreshold(policy, rec, contributor)
	countsKnown := obs.Detail.CommentsKnown && obs.Detail.ReviewsKnown
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "stage", rec.Stage.String(), "reason", rec.Reason}
	action := rec.AbandonStep(after, countsKnown, now)
	if (action == stagelabels.AbandonFlag || action == stagelabels.AbandonConfirm) && rec.AbandonTrustedTimer() && !c.abandonTrustedAuthorChecked(ctx, store, key, obs, now) {
		return
	}
	switch action {
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
		at, err := c.lastOutsideActivitySince(ctx, obs, since)
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

// abandonThreshold is how long the PR may idle before it is flagged: 0
// (never) for the hive's own and a maintainer's (contributor false),
// abandon_after_trusted for a trusted contributor's
// (Record.AbandonTrustedTimer), abandon_after for everyone else's.
func abandonThreshold(policy StageLabelPolicy, rec stagelabels.Record, contributor bool) time.Duration {
	switch {
	case !contributor:
		return 0
	case rec.AbandonTrustedTimer():
		return policy.AbandonAfterTrusted
	}
	return policy.AbandonAfter
}

// abandonTrustedAuthorChecked is the guard the trusted contributor timer
// pays once its threshold is crossed, before it flags, warns or closes: the
// cached read of the author's permission on the repository
// (authorRepoAccess). A PR rebuilt from the forge-state cache carries
// no author association, so a collaborator outside
// dashboard.authorized_users who resolves role/trusted would otherwise read
// as a trusted contributor. An author with write, maintain or admin is
// marked (Record.AbandonMaintainerSeen) and any flag cleared; a failed read
// defers to a later pass. It reports whether the caller may go on.
func (c *Engine) abandonTrustedAuthorChecked(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) bool {
	permission, role, err := c.authorRepoAccess(ctx, obs, now)
	switch {
	case err != nil:
		c.warn("stage labels: could not read the author's repository permission; trusted contributor idle clock deferred", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "error", err)
		return false
	case repoWriteLevel(permission, role):
		store.MarkAbandonMaintainerSeen(key)
		store.ClearAbandoned(key)
		c.info("stage labels: idle clock stopped: the author can push to the repository", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "permission", permission, "role", role)
		return false
	}
	return true
}

// abandonTrustedAssociations are the author associations that make a PR a
// maintainer's, not an outside contributor's.
var abandonTrustedAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// abandonMaintainer reports whether this pass sees the PR's author as a
// maintainer from a stable source, never the pass's resolved role: one of
// this hive's owners (policy.OwnerAuthor, dashboard.authorized_users), an
// author the trusted-author or merger authorizers admit (both read
// dashboard.authorized_users; configuration, no GitHub call), a trusted bot
// (auto_merge.trusted_bot_authors), or an OWNER / MEMBER / COLLABORATOR
// author association from the list payload.
func (c *Engine) abandonMaintainer(obs stageObservation, policy StageLabelPolicy) bool {
	login := strings.TrimSpace(obs.Author)
	switch {
	case abandonTrustedAssociations[strings.ToUpper(strings.TrimSpace(obs.AuthorAssociation))]:
		return true
	case login == "":
		return false
	case policy.OwnerAuthor != nil && policy.OwnerAuthor(login):
		return true
	case c.trustedAuthorDecision(login, c.currentTrustedAuthorPolicy().RequireRole).Allowed:
		return true
	case c.isTrustedBotLaneAuthor(login):
		return true
	}
	allowed, _ := c.isTrustedMerger(login)
	return allowed
}

// abandonHiveOwn reports whether the PR is the hive's own: author/hive,
// the App, or one of the hive's identities.
func (c *Engine) abandonHiveOwn(obs stageObservation, policy StageLabelPolicy) bool {
	login := strings.TrimSpace(obs.Author)
	switch {
	case obs.AuthorKind == authorkind.Hive:
		return true
	case login == "":
		return false
	case c.transport != nil && normalizeBotLogin(login) == normalizeBotLogin(c.transport.AppBotLogin()):
		return true
	}
	return policy.HiveAuthor != nil && policy.HiveAuthor(login)
}

// abandonRegistryTrusted reports whether the contributor registry's tier
// for author is one the contributor lane trusts (role/trusted through
// require_tiers): the same local read noteRegistryRole makes, for a pass
// whose resolved role missed it.
func (c *Engine) abandonRegistryTrusted(author string) bool {
	author = strings.TrimSpace(strings.TrimPrefix(author, "@"))
	if c == nil || c.contributorRegistryTier == nil || author == "" {
		return false
	}
	tier, ok := c.contributorRegistryTier(author)
	return ok && stagelabels.RoleFromTier(tier, c.currentContributorLanePolicy().RequireTiers) == stagelabels.RoleTrusted
}

// noteAbandonTrust records, while either idle clock is on, what this pass
// saw of the author: the hive or a maintainer (Record.AbandonMaintainerSeen),
// or else a trusted contributor, role/trusted or a trusted registry tier
// with no maintainer signal (Record.AbandonTrustedSeen). Both are sticky, so
// a pass whose role fell back to untrusted neither flags a maintainer's PR
// nor moves a trusted contributor's to the shorter abandon_after.
func (c *Engine) noteAbandonTrust(store *stagelabels.Store, key string, obs stageObservation, role stagelabels.Role, policy StageLabelPolicy) {
	if policy.AbandonAfter <= 0 && policy.AbandonAfterTrusted <= 0 {
		return
	}
	switch {
	case c.abandonHiveOwn(obs, policy), c.abandonMaintainer(obs, policy):
		store.MarkAbandonMaintainerSeen(key)
	case role == stagelabels.RoleTrusted, c.abandonRegistryTrusted(obs.Author):
		store.MarkAbandonTrustedSeen(key)
	}
}

// abandonContributor reports whether the idle clock applies to the PR at
// all: a contributor's PR — not the hive's own (the App, author/hive, one
// of the hive's identities), not a maintainer's (abandonMaintainer), and
// not one whose record says its author was ever seen as a maintainer
// (Record.AbandonMaintainerSeen). Owners and maintainers decide about their
// own PRs. A trusted contributor's PR is a contributor's, on its own
// threshold (abandonThreshold).
func (c *Engine) abandonContributor(obs stageObservation, policy StageLabelPolicy, rec stagelabels.Record) bool {
	if strings.TrimSpace(obs.Author) == "" || rec.AbandonMaintainerSeen || c.abandonHiveOwn(obs, policy) {
		return false
	}
	return !c.abandonMaintainer(obs, policy)
}

// outsideActor reports whether a comment or review by u counts as someone
// other than the hive acting: not the App and not another bot. A person's
// login configured as project.ai_author is not excluded: it may well be a
// person, whose reply must count.
func (c *Engine) outsideActor(u *gh.User) bool {
	login := strings.TrimSpace(u.GetLogin())
	if login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
		return false
	}
	if c.transport != nil && normalizeBotLogin(login) == normalizeBotLogin(c.transport.AppBotLogin()) {
		return false
	}
	return true
}

// abandonReadPages bounds the confirming read's comment pages (100 each).
const abandonReadPages = 10

// lastOutsideActivitySince is the one confirming read: the latest issue
// comment or review submission on the PR after since by someone other than
// the hive (zero when there is none). Issue comments are listed with
// GitHub's since filter and paged (at most abandonReadPages), so a crowd of
// hive or bot comments cannot hide an author's reply; reviews are one page
// plus the last page when there are more, so the newest are always seen.
func (c *Engine) lastOutsideActivitySince(ctx context.Context, obs stageObservation, since time.Time) (time.Time, error) {
	var latest time.Time
	note := func(at time.Time, u *gh.User) {
		if at.After(since) && at.After(latest) && c.outsideActor(u) {
			latest = at
		}
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	if !since.IsZero() {
		opts.Since = gh.Ptr(since)
	}
	for page := 0; page < abandonReadPages; page++ {
		comments, resp, err := c.gh.Issues.ListComments(ctx, obs.Owner, obs.Repo, obs.Number, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("listing comments for %s#%d: %w", obs.Display, obs.Number, err)
		}
		for _, cm := range comments {
			note(cm.GetCreatedAt().Time, cm.GetUser())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
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
// since. It runs after the pass's stage write landed, and only while the
// stage written (written) is still the park the flag was set in. The PR is
// closed first and the closing comment posted after, so a refused close
// never leaves a comment behind to repeat; a refusal backs off. It returns
// the number of comments posted and whether the PR was closed. The caller
// counts the comments as the hive's own.
func (c *Engine) closeAbandonedIfDue(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, written stagelabels.Stage, now time.Time) (int, bool, error) {
	if policy.AbandonClose == nil || obs.Merged {
		return 0, false, nil
	}
	rec, ok := store.Get(key)
	if !ok || !rec.Abandoned() || !rec.AbandonClosedAt.IsZero() || written != rec.Stage {
		return 0, false, nil
	}
	repo := obs.Owner + "/" + obs.Repo
	if !policy.AbandonClose(repo) {
		return 0, false, nil
	}
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "author", obs.Author, "reason", rec.Reason}
	if why := c.abandonCloseHeld(obs); why != "" {
		// A hold or keep-open label cancels the countdown: once it is
		// lifted the hive warns again and waits a full grace.
		if store.ResetAbandonNotice(key, now) {
			c.info("stage labels: abandoned PR close countdown cancelled", append(logArgs, "why", why)...)
		} else {
			c.debug("stage labels: abandoned PR not closed", append(logArgs, "why", why)...)
		}
		return 0, false, nil
	}
	grace := config.AbandonCloseGrace
	item := writebackItemForObservation(obs)
	if rec.AbandonNoticeAt.IsZero() {
		if rec.AbandonTrustedTimer() && !c.abandonTrustedAuthorChecked(ctx, store, key, obs, now) {
			return 0, false, nil
		}
		// The store saves after the pass, so a restart between the post and
		// the save forgets the notice. The hive's own AbandonNoticeMarker
		// comment posted inside this countdown is taken as the notice, not
		// posted a second time.
		posted, err := c.ownAbandonNoticeSince(ctx, obs, rec.AbandonCountdownStart())
		if err != nil {
			c.warn("stage labels: abandoned PR notice check failed; not warned yet", append(logArgs, "error", err)...)
			return 0, false, err
		}
		if !posted.IsZero() {
			store.MarkAbandonNoticed(key, posted)
			c.info("stage labels: abandoned PR notice already posted; countdown resumed", append(logArgs, "noticed_at", posted)...)
			return 0, false, nil
		}
		body := AbandonNoticeComment(obs, rec, grace, now)
		if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionNotice, body); !d.Allowed {
			c.info("stage labels: abandoned PR notice suppressed by the write-back gate", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
			return 0, false, nil
		}
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			c.warn("stage labels: abandoned PR notice failed", append(logArgs, "error", err)...)
			return 0, false, err
		}
		store.MarkAbandonNoticed(key, now)
		c.info("stage labels: abandoned PR warned before closing", append(logArgs, "grace", grace.String())...)
		return 1, false, nil
	}
	if !rec.AbandonCloseDue(grace, now) {
		return 0, false, nil
	}
	if rec.AbandonTrustedTimer() && !c.abandonTrustedAuthorChecked(ctx, store, key, obs, now) {
		return 0, false, nil
	}
	// A fresh read before the close: a reply the lane's counts never saw
	// keeps the PR open (and clears the flag on the next pass).
	at, err := c.lastOutsideActivitySince(ctx, obs, rec.AbandonNoticeAt)
	if err != nil {
		c.warn("stage labels: abandoned PR close check failed; left open", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if !at.IsZero() {
		store.NoteAbandonCheck(key, at, now)
		c.info("stage labels: abandoned PR not closed: activity since the notice", append(logArgs, "activity_at", at)...)
		return 0, false, nil
	}
	body := AbandonCloseComment(rec, grace, now)
	if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionClose, body); !d.Allowed {
		c.info("stage labels: abandoned PR close suppressed by the write-back gate, left open", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
		return 0, false, nil
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
		store.MarkAbandonCloseTried(key, now)
		c.warn("stage labels: closing the abandoned PR failed; left open, retried later", append(logArgs, "error", err)...)
		return 0, false, err
	}
	store.MarkAbandonClosed(key, now)
	c.info("stage labels: abandoned contributor PR closed as stale", logArgs...)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		// Closed already; the comment is not retried (AbandonClosedAt).
		c.warn("stage labels: abandoned PR closed but its closing comment failed", append(logArgs, "error", err)...)
		return 0, true, err
	}
	return 1, true, nil
}

// ownAbandonNoticeSince returns when the hive's own auto-close notice on
// the PR was posted, if one was posted at or after since, or zero. Only the
// hive's App login counts: anyone can type AbandonNoticeMarker. A hive
// that cannot name its App login reads as "none".
func (c *Engine) ownAbandonNoticeSince(ctx context.Context, obs stageObservation, since time.Time) (time.Time, error) {
	if c.transport == nil || normalizeBotLogin(c.transport.AppBotLogin()) == "" {
		return time.Time{}, nil
	}
	cm, err := c.findOwnMarkerComment(ctx, obs.Owner, obs.Repo, obs.Number, func(body string) bool {
		return strings.HasPrefix(body, AbandonNoticeMarker)
	})
	if err != nil || cm == nil {
		return time.Time{}, err
	}
	at := cm.GetCreatedAt().Time
	if at.Before(since) {
		return time.Time{}, nil
	}
	return at, nil
}

// abandonedAgeText renders how long rec has been idle at now in words a
// contributor reads ("9 days", "5 hours").
func abandonedAgeText(rec stagelabels.Record, now time.Time) string {
	since := rec.AbandonedIdleSince
	if since.IsZero() {
		since = rec.IdleSince()
	}
	var idle time.Duration
	if !since.IsZero() && now.After(since) {
		idle = now.Sub(since)
	}
	return durationWords(idle)
}

// durationWords renders d as whole days, else whole hours, else "less
// than an hour".
func durationWords(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d >= 24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d >= time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return "less than an hour"
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

// graceText renders the grace in words ("24 hours", "2 days").
func graceText(grace time.Duration) string {
	if grace == 24*time.Hour {
		return "24 hours"
	}
	return durationWords(grace)
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

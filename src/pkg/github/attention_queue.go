package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/attention"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The attention pilot's GitHub-native action queue and reminder policy
// (github.notifications.attention_pilot, hivecommons/hive#16361, #16362).
// Once per un-park sweep, for every repository this hive admits attention
// for, SweepAttentionQueue walks the open attention episodes and:
//
//   - resolves the episodes of an item that has been closed and clears the
//     item's Attention section, so no stale ask (or reminder) outlives it;
//   - sends at most one reminder per unresolved episode per
//     github.attention.reminder_interval (default 24h), never while the
//     episode is acknowledged or snoozed, and never when the recipient has
//     commented, reviewed or changed a label on the item since the last
//     ping. A reminder is one comment; it never removes and re-adds a
//     reviewer or requests a review, so a request GitHub, CODEOWNERS or
//     Prow already owns is never duplicated;
//   - keeps one per-repo tracking issue listing every open episode. The
//     issue is created once — its only notifying write — and afterwards
//     edited in place, which notifies nobody.

// attentionNow is the reminder clock; tests replace it.
var attentionNow = time.Now

const (
	// attentionQueueMarker tags the per-repo tracking issue's body.
	attentionQueueMarker = "<!-- hive:attention-queue -->"
	// AttentionQueueTitle is the tracking issue's title (before the filer tag).
	AttentionQueueTitle = "Hive attention queue"
	// attentionQueueMaxLines bounds the tracking issue body.
	attentionQueueMaxLines = 200
)

// AttentionQueueResult reports one repository's attention pass.
type AttentionQueueResult struct {
	// Reminders counts reminder comments posted.
	Reminders int
	// Resolved counts closed items whose open episodes were resolved.
	Resolved int
	// Suppressed counts reminders the write-back gate refused; they are
	// retried on a later pass.
	Suppressed int
	// QueueIssue is the tracking issue number (0 when none exists).
	QueueIssue int
	// QueueCreated and QueueEdited report the tracking issue write.
	QueueCreated, QueueEdited bool
}

// SweepAttentionQueue runs the attention pilot's reminder, closure and
// tracking-issue pass for repo (owner/name). It does nothing unless the
// attention pilot is on and this hive is the repo's attention admitter.
func (c *Client) SweepAttentionQueue(ctx context.Context, repo string) (AttentionQueueResult, error) {
	var res AttentionQueueResult
	if c == nil {
		return res, ErrNoGitHubClient
	}
	if !c.attentionPilot() || c.attentionConfig == nil || c.attentionHiveID == "" {
		return res, nil
	}
	cfg := c.attentionConfig()
	if cfg.Owner(repo) != c.attentionHiveID {
		return res, nil
	}
	store, err := c.WritebackGate().AttentionStore()
	if err != nil {
		return res, err
	}
	owner, name := c.splitRepo(repo)
	now := attentionNow().UTC()
	byItem := map[int][]attention.Episode{}
	var items []int
	for _, e := range store.OpenEpisodes(repo) {
		if _, seen := byItem[e.Key.Item]; !seen {
			items = append(items, e.Key.Item)
		}
		byItem[e.Key.Item] = append(byItem[e.Key.Item], e)
	}
	var errs []error
	for _, n := range items {
		issue, _, err := c.client.Issues.Get(ctx, owner, name, n)
		if err != nil {
			errs = append(errs, fmt.Errorf("attention queue %s#%d: reading item: %w", repo, n, err))
			continue
		}
		if issue.GetState() == "closed" {
			if _, err := store.ResolveItem(repo, n); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Resolved++
			if _, err := c.UpsertPRStatusSection(ctx, repo, n, "attention", ""); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		reminded := false
		for _, e := range byItem[n] {
			if !attention.ReminderDue(e, now, cfg.Reminder()) {
				continue
			}
			sent, err := c.remindAttention(ctx, store, repo, issue, e, now)
			switch {
			case errors.Is(err, writeback.ErrSuppressed):
				res.Suppressed++
			case err != nil:
				errs = append(errs, err)
			case sent:
				res.Reminders++
				reminded = true
			}
		}
		if reminded {
			if _, err := c.UpsertPRStatusSection(ctx, repo, n, "attention", attentionStatus(store.Snapshot(repo, n))); err != nil {
				errs = append(errs, err)
			}
		}
	}
	num, created, edited, err := c.syncAttentionQueueIssue(ctx, repo, store.OpenEpisodes(repo))
	res.QueueIssue, res.QueueCreated, res.QueueEdited = num, created, edited
	if err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

// remindAttention sends one reminder for e unless its recipient acted on the
// item since the last ping, in which case the action is recorded and the
// interval restarts from it. The comment consults the write-back gate; a
// refusal wraps writeback.ErrSuppressed and nothing is written.
func (c *Client) remindAttention(ctx context.Context, store *attention.Store, repo string, issue *gh.Issue, e attention.Episode, now time.Time) (bool, error) {
	n := issue.GetNumber()
	if login, ok := attention.RecipientLogin(e); ok {
		acted, err := c.recipientActedAt(ctx, repo, issue, login, e.LastPing())
		if err != nil {
			return false, fmt.Errorf("attention reminder %s#%d: reading recipient activity: %w", repo, n, err)
		}
		if !acted.IsZero() {
			return false, store.RecordRecipientAction(e.Key, acted)
		}
	}
	body := renderAttentionReminder(e)
	author := safeGetLogin(issue.GetUser())
	item := writeback.Item{
		Number:       n,
		Author:       author,
		HiveAuthored: issue.GetUser().GetType() == "Bot" && c.IsHiveAuthorLogin(author),
		Labels:       issueLabelNames(issue.Labels),
		CreatedAt:    issue.GetCreatedAt().Time,
	}
	if d := c.WritebackAllowed(repo, item, writeback.KindNeedsDecisionComment, body); !d.Allowed {
		return false, fmt.Errorf("attention reminder %s#%d: %w: %s", repo, n, writeback.ErrSuppressed, d.Reason)
	}
	if err := c.CreateIssueComment(WithNotificationSubject(ctx, "attention-reminder:"+e.Key.Hash()), repo, n, body); err != nil { // writeback:gated — WritebackAllowed consulted above
		return false, err
	}
	_, err := store.RecordReminder(e.Key, now)
	return true, err
}

// recipientActedAt returns when login last commented, reviewed or changed a
// label on the item after since; zero when it did not.
func (c *Client) recipientActedAt(ctx context.Context, repo string, issue *gh.Issue, login string, since time.Time) (time.Time, error) {
	var last time.Time
	events, err := c.IssueTimelineSince(ctx, repo, issue.GetNumber(), since)
	if err != nil {
		return last, err
	}
	for _, ev := range events {
		switch ev.Event {
		case "commented", "labeled", "unlabeled", "reviewed":
			if strings.EqualFold(ev.Actor, login) && ev.At.After(last) {
				last = ev.At
			}
		}
	}
	if !issue.IsPullRequest() {
		return last, nil
	}
	// Reviews carry submitted_at, not created_at, so the timeline skips them.
	owner, name := c.splitRepo(repo)
	opts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, name, issue.GetNumber(), opts)
		if err != nil {
			return last, err
		}
		for _, r := range reviews {
			at := r.GetSubmittedAt().Time
			if strings.EqualFold(safeGetLogin(r.GetUser()), login) && at.After(since) && at.After(last) {
				last = at
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return last, nil
		}
		opts.Page = resp.NextPage
	}
}

// renderAttentionReminder is the reminder comment for e. It names the
// recipient (the reminder is for them), never copies other mentions, and
// carries the ack/snooze instructions and the not-approval wording.
func renderAttentionReminder(e attention.Episode) string {
	who := statusText(e.Key.Recipient)
	if login, ok := attention.RecipientLogin(e); ok {
		who = "@" + login
	}
	hash := e.Key.Hash()
	var b strings.Builder
	fmt.Fprintf(&b, "<!-- hive:attention-reminder key=%s n=%d -->\n", hash, e.Reminders+1)
	fmt.Fprintf(&b, "%s reminder %d: this item still needs %s from you (episode `%s`, open since %s).\n\n",
		who, e.Reminders+1, statusText(e.Key.Action), hash[:12], e.FirstSeen.UTC().Format("2006-01-02T15:04Z"))
	b.WriteString("Reply `/hive ack` if you have seen it, or `/hive snooze <duration>` (for example `2d`) to pause reminders. ")
	b.WriteString("Hive sends at most one reminder per interval and none after you comment, review or change a label here.\n\n")
	b.WriteString(attention.NotApproval + "\n")
	return b.String()
}

// renderAttentionQueue is the tracking issue body: one line per open
// episode — who owes what, since when, snoozed until. Item references are
// code spans so an edit never cross-references (or notifies) the item.
func renderAttentionQueue(open []attention.Episode) string {
	var b strings.Builder
	b.WriteString(attentionQueueMarker + "\n")
	b.WriteString("**Hive attention queue** — open attention episodes in this repository: who owes what, since when, snoozed until. ")
	b.WriteString("This issue is edited in place and edits do not notify; an entry leaves the list when its episode resolves or its item closes. ")
	b.WriteString("Reply `/hive ack` or `/hive snooze <duration>` on the item, not here.\n\n")
	if len(open) == 0 {
		b.WriteString("_No open attention episodes._\n")
	}
	for i, e := range open {
		if i == attentionQueueMaxLines {
			fmt.Fprintf(&b, "\n%d further open episodes retained in the durable ledger.\n", len(open)-i)
			break
		}
		fmt.Fprintf(&b, "- `#%d` `%s`: %s owes %s since %s", e.Key.Item, e.Key.Hash()[:12], statusText(e.Key.Recipient), statusText(e.Key.Action), e.FirstSeen.UTC().Format("2006-01-02T15:04Z"))
		if e.State == attention.Acknowledged || !e.AckedAt.IsZero() {
			b.WriteString("; acknowledged")
		}
		if !e.SnoozedUntil.IsZero() {
			fmt.Fprintf(&b, "; snoozed until %s", e.SnoozedUntil.UTC().Format("2006-01-02T15:04Z"))
		}
		b.WriteByte('\n')
	}
	b.WriteString("\n" + attention.NotApproval + "\n")
	return b.String()
}

// syncAttentionQueueIssue keeps repo's tracking issue in step with open. It
// creates the issue only when there is something to list and none exists,
// edits it only when the body changed, and leaves an issue a person closed
// alone (reopen it to resume). Both writes consult the write-back gate.
func (c *Client) syncAttentionQueueIssue(ctx context.Context, repo string, open []attention.Episode) (int, bool, bool, error) {
	owner, name := c.splitRepo(repo)
	label := config.OriginLabel(config.OriginSourceGovernor)
	existing, err := c.findAttentionQueueIssue(ctx, owner, name, label)
	if err != nil {
		// A failed lookup must never open a second queue issue.
		return 0, false, false, fmt.Errorf("attention queue %s: lookup failed: %w", repo, err)
	}
	if existing != nil && existing.GetState() == "closed" {
		return existing.GetNumber(), false, false, nil
	}
	if existing == nil && len(open) == 0 {
		return 0, false, false, nil
	}
	title := issueshape.AgentIssueTitle(config.OriginSourceGovernor, AttentionQueueTitle)
	body := advisory.NeutralizeMentions(renderAttentionQueue(open))
	if leak, ok := c.scanCanaryText(title+"\n"+body, "hive-attention-queue:"+repo); ok && c.canaryFailClosed {
		return 0, false, false, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	}
	body = logscrub.ScrubString(body)
	if existing != nil && strings.TrimSpace(existing.GetBody()) == strings.TrimSpace(body) {
		return existing.GetNumber(), false, false, nil
	}
	item := writeback.Item{HiveAuthored: true}
	if existing != nil {
		item.Number, item.Author, item.Labels = existing.GetNumber(), safeGetLogin(existing.GetUser()), issueLabelNames(existing.Labels)
	}
	if d := c.WritebackAllowed(repo, item, writeback.KindIssueFile, ""); !d.Allowed {
		return item.Number, false, false, fmt.Errorf("attention queue %s: %w: %s", repo, writeback.ErrSuppressed, d.Reason)
	}
	if existing != nil {
		if _, _, err := c.client.Issues.Edit(ctx, owner, name, existing.GetNumber(), &gh.IssueRequest{Body: gh.Ptr(body)}); err != nil { // writeback:gated — WritebackAllowed consulted above
			return existing.GetNumber(), false, false, fmt.Errorf("attention queue %s#%d: edit failed: %w", repo, existing.GetNumber(), err)
		}
		return existing.GetNumber(), false, true, nil
	}
	// The origin label is how the next pass finds the issue; without it
	// every pass would open another one.
	if err := c.ensureOriginLabel(ctx, owner, name, label); err != nil {
		return 0, false, false, fmt.Errorf("attention queue %s: ensuring %s: %w", repo, label, err)
	}
	created, _, err := c.client.Issues.Create(ctx, owner, name, &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body), Labels: gh.Ptr([]string{label})}) // writeback:gated — WritebackAllowed consulted above
	if err != nil {
		return 0, false, false, fmt.Errorf("attention queue %s: create failed: %w", repo, err)
	}
	if c.logger != nil {
		c.logger.Info("attention queue issue opened", "repo", repo, "number", created.GetNumber())
	}
	return created.GetNumber(), true, false, nil
}

// findAttentionQueueIssue returns the hive's own tracking issue in any
// state, preferring an open one. Only hive-authored issues carrying the
// marker qualify, so a squatted title is never adopted.
func (c *Client) findAttentionQueueIssue(ctx context.Context, owner, repo, label string) (*gh.Issue, error) {
	own := c.knowsOwnAccounts()
	var closed *gh.Issue
	opts := &gh.IssueListByRepoOptions{State: "all", Labels: []string{label}, Sort: "created", Direction: "asc", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() || !strings.Contains(is.GetBody(), attentionQueueMarker) {
				continue
			}
			if own && !c.isHiveAccount(is.GetUser()) && !c.IsHiveAuthorLogin(safeGetLogin(is.GetUser())) {
				continue
			}
			if is.GetState() != "closed" {
				return is, nil
			}
			if closed == nil {
				closed = is
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return closed, nil
		}
		opts.Page = resp.NextPage
	}
}

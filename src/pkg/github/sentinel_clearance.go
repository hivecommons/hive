package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// sentinelClearedNote closes the marker comment the sweep rewrites when it
// accepts a label removal on a head it never alerted on (#13602); the
// arguments are the login that removed the label and the label name.
const sentinelClearedNote = "_@%s removed the `%s` label after this head was pushed, so Hive treats this head as reviewed and will not re-apply the label unless new commits are pushed._"

// sentinelCarriedNote closes the marker comment the sweep rewrites when it
// carries a clearance to a head whose flagged changes are identical to the
// ones the maintainer cleared; same arguments as sentinelClearedNote.
const sentinelCarriedNote = "_@%s removed the `%s` label after reviewing an earlier head. This head leaves every flagged change exactly as it was (for example, it only merges the base branch), so Hive carries that review forward and will re-apply the label only when a flagged change differs._"

// sentinelContentPrefix starts the line that records, in the hive's own
// marker comment, the digest of what an alert flagged (sentinelFlaggedDigest).
const sentinelContentPrefix = "<!-- hive-sentinel-content "

var sentinelContentRE = regexp.MustCompile(`<!-- hive-sentinel-content ([0-9a-f]{64}) -->`)

// sentinelFlaggedDigest fingerprints exactly what the findings flag: each
// finding's rule and paths, and for every flagged path its status and the
// sha256 of its patch against the PR's merge base, as GitHub's PR files API
// returns it at evaluation time. Two heads with the same digest show a
// reviewer the same flagged changes, so a clearance of one is a clearance of
// the other; a merge of the base branch that leaves the PR's own change to
// each flagged file alone keeps the digest. It returns "" (no carry-over) when
// any finding names no path or a flagged file has no patch (GitHub omits very
// large ones): what cannot be compared is never treated as unchanged.
func sentinelFlaggedDigest(findings []sentinel.Finding, files []sentinel.File) string {
	byPath := make(map[string]sentinel.File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	var lines []string
	flagged := map[string]bool{}
	for _, f := range findings {
		if len(f.Paths) == 0 {
			return ""
		}
		paths := append([]string(nil), f.Paths...)
		sort.Strings(paths)
		lines = append(lines, "rule\t"+f.Rule+"\t"+strings.Join(paths, "\t"))
		for _, p := range paths {
			flagged[p] = true
		}
	}
	if len(lines) == 0 {
		return ""
	}
	for p := range flagged {
		file, ok := byPath[p]
		if !ok || file.Patch == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(file.Patch))
		lines = append(lines, "file\t"+p+"\t"+file.Status+"\t"+hex.EncodeToString(sum[:]))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// sentinelWithDigest appends the flagged-content digest line to a marker
// comment body; an empty digest leaves the body as it was.
func sentinelWithDigest(body, digest string) string {
	if digest == "" {
		return body
	}
	return body + sentinelContentPrefix + digest + " -->\n"
}

// sentinelCommentDigest reads the flagged-content digest back from a marker
// comment body; "" when it has none (an alert written before digests).
func sentinelCommentDigest(body string) string {
	if m := sentinelContentRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// sentinelLabelHistory is what the PR timeline says about the sentinel label
// and the current head: the newest labeled/unlabeled event for the label and
// the server time of a force push that made head the PR's head.
type sentinelLabelHistory struct {
	lastKind      string
	lastActor     *gh.User
	lastAt        time.Time
	forcePushedAt time.Time
}

func (c *Client) sentinelLabelHistory(ctx context.Context, owner, repo string, number int, head, label string) (sentinelLabelHistory, error) {
	var h sentinelLabelHistory
	opts := &gh.ListOptions{PerPage: labelTimelinePageSize}
	for {
		page, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if err != nil {
			return sentinelLabelHistory{}, fmt.Errorf("reading timeline of %s/%s#%d for sentinel clearance: %w", owner, repo, number, err)
		}
		for _, ev := range page {
			if ev == nil {
				continue
			}
			switch strings.ToLower(ev.GetEvent()) {
			case "labeled", "unlabeled":
				if !strings.EqualFold(strings.TrimSpace(ev.GetLabel().GetName()), label) {
					continue
				}
				at := ev.GetCreatedAt().Time
				if at.IsZero() || !at.After(h.lastAt) {
					continue
				}
				h.lastKind = strings.ToLower(ev.GetEvent())
				h.lastActor = ev.GetActor()
				h.lastAt = at
			case "head_ref_force_pushed":
				if !strings.EqualFold(strings.TrimSpace(ev.GetCommitID()), head) {
					continue
				}
				if at := ev.GetCreatedAt().Time; at.After(h.forcePushedAt) {
					h.forcePushedAt = at
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return h, nil
}

// humanRemoval returns the login that made the newest label event when that
// event is an `unlabeled` by a human who is not one of the hive's accounts.
func (c *Client) humanRemoval(h sentinelLabelHistory) (string, bool) {
	if h.lastKind != "unlabeled" {
		return "", false
	}
	actor := strings.TrimSpace(h.lastActor.GetLogin())
	if actor == "" || c.isHiveAccount(h.lastActor) {
		return "", false
	}
	if strings.EqualFold(h.lastActor.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(actor), "[bot]") {
		return "", false
	}
	return actor, true
}

// sentinelReleasedNote is the same closing line for a release made by a
// dashboard Approve bound to this head.
const sentinelReleasedNote = "_@%s approved this head from the Hive dashboard, so Hive treats it as reviewed and will not re-apply the `%s` label unless new commits are pushed._"

// sentinelClearedAfterLatestPush reports whether the newest label event for
// label on the PR is an `unlabeled` by a human who is not one of the hive's
// own accounts, and whether it happened after head was pushed.
//
// The push time comes only from signals GitHub stamps server-side when the
// SHA arrives, never from anything in the commit itself: a committer date
// is set by whoever made the commit, so a contributor could backdate a
// commit pushed after the maintainer's clearance and have the stale
// clearance honoured on new code (#13602 review). In order: the
// `head_ref_force_pushed` timeline event whose commit_id is head; for an
// ordinary push, the earliest check suite or commit status created for
// head. Without any of those nothing is cleared: not knowing is never a
// clearance.
func (c *Client) sentinelClearedAfterLatestPush(ctx context.Context, owner, repo string, number int, head, label string) (bool, string, error) {
	h, err := c.sentinelLabelHistory(ctx, owner, repo, number, head, label)
	if err != nil {
		return false, "", err
	}
	actor, ok := c.humanRemoval(h)
	if !ok {
		return false, "", nil
	}
	pushedAt := h.forcePushedAt
	if pushedAt.IsZero() {
		arrived, err := c.sentinelHeadArrivedAt(ctx, owner, repo, head)
		if err != nil {
			return false, "", err
		}
		pushedAt = arrived
	}
	if pushedAt.IsZero() || !h.lastAt.After(pushedAt) {
		return false, "", nil
	}
	return true, actor, nil
}

// sentinelClearanceCarriesOver reports whether a maintainer's clearance of an
// earlier head covers head because nothing Sentinel flagged has changed:
// the hive's newest marker comment is a blocking alert whose recorded digest
// equals digest (computed from the current head's PR files), and the newest
// event for the label is a human's removal. The removal is not compared with
// the push time: what makes it a review of this head is that the flagged
// changes are byte-for-byte the ones that were reviewed. A head that changes
// any of them, flags something new, or cannot be fingerprinted (digest "")
// re-alerts as before.
func (c *Client) sentinelClearanceCarriesOver(ctx context.Context, owner, repo string, number int, head, label, digest string) (bool, string, error) {
	if digest == "" {
		return false, "", nil
	}
	own, err := c.newestOwnMarkerComment(ctx, owner, repo, number)
	if err != nil {
		return false, "", err
	}
	if own == nil || !strings.Contains(own.GetBody(), sentinelAlertHeading) || sentinelCommentDigest(own.GetBody()) != digest {
		return false, "", nil
	}
	h, err := c.sentinelLabelHistory(ctx, owner, repo, number, head, label)
	if err != nil {
		return false, "", err
	}
	actor, ok := c.humanRemoval(h)
	if !ok {
		return false, "", nil
	}
	return true, actor, nil
}

// newestOwnMarkerComment is the newest sentinel marker comment one of the
// hive's own accounts wrote on the PR, or nil when there is none.
func (c *Client) newestOwnMarkerComment(ctx context.Context, owner, repo string, number int) (*gh.IssueComment, error) {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("listing comments for sentinel clearance: %w", err)
	}
	var own *gh.IssueComment
	for _, comment := range comments {
		if comment == nil || !strings.Contains(comment.GetBody(), SentinelMarker) || !c.knowsOwnAccounts() || !c.isHiveAccount(comment.GetUser()) {
			continue
		}
		own = comment
	}
	return own, nil
}

// sentinelReleasedByDashboardApproval reports the login on whose behalf the
// hive posted a dashboard approval command bound to head
// (renderPRApprovalCommandComment), or "" when no such comment exists.
//
// A dashboard Approve takes the alert label off as the hive's own account,
// so sentinelClearedAfterLatestPush rightly refuses to read the `unlabeled`
// event as a human clearance — and the sweep then re-alerted the head the
// owner had just approved (dibs#510: approved 00:32Z, re-labelled 00:33Z).
// The approval comment is the durable, hive-authored record of that
// release: its marker names the head the person saw, so it is never
// honoured on a later push. Only comments the hive itself wrote count;
// anyone can type the marker. Unlike markerAuthorTrusted, a hive that cannot
// name its own accounts trusts no author here: the comment releases a
// security alert, and under the legacy any-author rule the PR author could
// forge it for their own head (hivecommons/hive#15262).
func (c *Client) sentinelReleasedByDashboardApproval(ctx context.Context, owner, repo string, number int, head string) (string, error) {
	head = strings.ToLower(strings.TrimSpace(head))
	if head == "" {
		return "", nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("listing comments of %s/%s#%d for sentinel release: %w", owner, repo, number, err)
	}
	for _, comment := range comments {
		if comment == nil || !c.knowsOwnAccounts() || !c.isHiveAccount(comment.GetUser()) {
			continue
		}
		_, actor, boundHead, ok := ParsePRApprovalCommandComment(comment.GetBody())
		if ok && boundHead == head && actor != "" {
			return actor, nil
		}
	}
	return "", nil
}

// sentinelHeadArrivedAt returns the earliest server-stamped created_at of
// a check suite or commit status for sha, which GitHub records when the SHA
// first reaches the repository; zero when the SHA has neither.
func (c *Client) sentinelHeadArrivedAt(ctx context.Context, owner, repo, sha string) (time.Time, error) {
	var earliest time.Time
	note := func(at time.Time) {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	suiteOpts := &gh.ListCheckSuiteOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		suites, resp, err := c.client.Checks.ListCheckSuitesForRef(ctx, owner, repo, sha, suiteOpts)
		if err != nil {
			return time.Time{}, fmt.Errorf("listing check suites of %s/%s@%s for sentinel clearance: %w", owner, repo, sha, err)
		}
		if suites != nil {
			for _, suite := range suites.CheckSuites {
				if suite != nil {
					note(suite.GetCreatedAt().Time)
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		suiteOpts.Page = resp.NextPage
	}
	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		statuses, resp, err := c.client.Repositories.ListStatuses(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return time.Time{}, fmt.Errorf("listing statuses of %s/%s@%s for sentinel clearance: %w", owner, repo, sha, err)
		}
		for _, status := range statuses {
			if status != nil {
				note(status.GetCreatedAt().Time)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}
	return earliest, nil
}

// recordSentinelClearance makes a clearance accepted by
// sentinelClearedAfterLatestPush, sentinelClearanceCarriesOver or sentinelReleasedByDashboardApproval
// durable: the hive's newest marker comment on the PR is rewritten as the
// alert for the current head followed by note (sentinelClearedNote or
// sentinelReleasedNote, already formatted), in the form
// sentinelClearedByMaintainer reads back, so a hub restart (empty
// sentinelHeads) finds "alert for this head, label absent" and stays quiet.
// When the hive has no marker comment on the PR one is posted, as it is when
// the hive cannot name its own accounts: the clearance record must not be
// written into a comment of unverified authorship (hivecommons/hive#15262).
func (c *Client) recordSentinelClearance(ctx context.Context, owner, repo string, number int, note, alert string) error {
	body := alert + "\n" + note + "\n"
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("listing comments for sentinel clearance: %w", err)
	}
	var own *gh.IssueComment
	for _, comment := range comments {
		if comment == nil || !strings.Contains(comment.GetBody(), SentinelMarker) || !c.knowsOwnAccounts() || !c.isHiveAccount(comment.GetUser()) {
			continue
		}
		own = comment
	}
	if own != nil {
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, own.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return fmt.Errorf("recording sentinel clearance: %w", err)
		}
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("recording sentinel clearance: %w", err)
	}
	return nil
}

package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewBacklogSplitMarker heads both the split-out item's body and the
// cross-link comment on the closed host, keyed on the folded finding's
// link-marker key.
const reviewBacklogSplitMarker = "<!-- hive:review-backlog-split "

// DefaultReviewBacklogSplitMaxActions caps the findings one split pass files.
const DefaultReviewBacklogSplitMaxActions = 20

var (
	// reviewBacklogFoldedCheckRE matches the `- [ ] <title> (`file:line`)`
	// task-list line reviewBacklogFoldedComment writes. The file may be
	// empty: a fold by shared symbol or exact-title twin can carry a
	// finding with no File, written as (`:0`).
	reviewBacklogFoldedCheckRE = regexp.MustCompile("(?m)^\\s*- \\[([ xX])\\] (.*) \\(`([^`]*):(\\d+)`\\)\\s*$")
	reviewBacklogFoldedPRRE    = regexp.MustCompile(`PR #(\d+)`)
	reviewBacklogFoldedFPRE    = regexp.MustCompile("(?m)^Fingerprint: `([0-9a-fA-F]+)`")
)

// reviewBacklogFoldedItem is one finding folded into a host issue by
// reviewBacklogFoldedComment, read back from the comment text.
type reviewBacklogFoldedItem struct {
	Key         string
	Title       string
	Perspective review.Perspective
	Finding     outputschema.Finding
	PRNumber    int
	Fingerprint string
	Checked     bool
	// Detail is the comment from its "Perspective:" line on: severity,
	// evidence, finding, summary, fingerprint and footer.
	Detail string
	// Comment is the whole folded comment, kept so a split that the sink
	// folds into another open twin carries it over unchanged.
	Comment string
}

// parseReviewBacklogFoldedComment reads a hive:review-backlog-link comment
// in the layout reviewBacklogFoldedComment (review_backlog_routing.go)
// writes; keep the two in sync.
// It returns false for any other comment and for link comments without a
// `- [ ]`/`- [x]` checklist line (the pre-#13604 bare link carried no
// finding, so there is nothing to split out).
func parseReviewBacklogFoldedComment(body string) (reviewBacklogFoldedItem, bool) {
	i := strings.Index(body, reviewBacklogLinkMarker)
	if i < 0 {
		return reviewBacklogFoldedItem{}, false
	}
	rest := body[i+len(reviewBacklogLinkMarker):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return reviewBacklogFoldedItem{}, false
	}
	key := strings.TrimSpace(rest[:end])
	m := reviewBacklogFoldedCheckRE.FindStringSubmatch(body)
	if key == "" || m == nil {
		return reviewBacklogFoldedItem{}, false
	}
	line, _ := strconv.Atoi(m[4])
	item := reviewBacklogFoldedItem{
		Key:     key,
		Checked: m[1] != " ",
		Finding: outputschema.Finding{Title: strings.TrimSpace(m[2]), File: strings.TrimSpace(m[3]), Line: line},
		Comment: body,
	}
	for _, l := range strings.Split(body, "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "### "); ok {
			item.Title = strings.TrimSpace(t)
			break
		}
	}
	if pm := reviewBacklogPerspectiveRE.FindStringSubmatch(body); pm != nil {
		item.Perspective = review.Perspective(strings.TrimSpace(pm[1]))
	}
	if item.Title == "" {
		item.Title = reviewBacklogIssueTitle(item.Perspective, item.Finding)
	}
	if pm := reviewBacklogFoldedPRRE.FindStringSubmatch(body); pm != nil {
		item.PRNumber, _ = strconv.Atoi(pm[1])
	}
	if fm := reviewBacklogFoldedFPRE.FindStringSubmatch(body); fm != nil {
		item.Fingerprint = strings.ToLower(fm[1])
	}
	if j := strings.Index(body, "\nPerspective:"); j >= 0 {
		item.Detail = body[j+1:]
	}
	return item, true
}

// reviewBacklogSplitEligible is the skip rule for a folded item on a closed
// host. A checked item was dealt with and stays. An unchecked item is split
// out when the host closed as completed (or with no reason): the host's fix
// did not tick it off. When the host closed as not_planned or duplicate, the
// decision is taken to cover the findings that match the host (same
// category and symbol or nearby line, reviewBacklogFoldMatches), so only an
// unchecked item unrelated to the host is split out.
func reviewBacklogSplitEligible(item reviewBacklogFoldedItem, stateReason, hostTitle, hostBody string) bool {
	if item.Checked {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stateReason)) {
	case "not_planned", "duplicate":
		return !reviewBacklogFoldMatches(item.Perspective, item.Finding, hostTitle, hostBody)
	}
	return true
}

// reviewBacklogSplitBody is the split-out item's body: the
// reviewBacklogIssueBody content carried by the folded comment, headed by
// the split marker and a "Split from #N (host closed)" cross-link.
func reviewBacklogSplitBody(owner, repo string, host int, stateReason string, item reviewBacklogFoldedItem) string {
	reason := strings.TrimSpace(stateReason)
	if reason == "" {
		reason = "completed"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n", reviewBacklogSplitMarker, item.Key)
	fmt.Fprintf(&b, "Split from #%d (host closed): this finding was folded into #%d as an additional finding and was still unchecked when #%d closed (%s), so it is filed on its own instead of closing with it.\n\n", host, host, host, reason)
	if item.PRNumber > 0 {
		fmt.Fprintf(&b, "Filed from the Hive review of PR #%d.\n\n", item.PRNumber)
		fmt.Fprintf(&b, "PR: #%d (%s)\n", item.PRNumber, reviewPRURL(owner, repo, item.PRNumber))
	}
	if item.Detail != "" {
		b.WriteString(item.Detail)
	} else {
		fmt.Fprintf(&b, "Evidence: `%s:%d`\nFinding: %s\n\n%s\n", item.Finding.File, item.Finding.Line, item.Finding.Title, reviewBacklogFooter)
	}
	return b.String()
}

// reviewBacklogSplitHostComment cross-links the closed host to the item a
// folded finding was split out into.
func reviewBacklogSplitHostComment(item reviewBacklogFoldedItem, ref ReviewBacklogRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s -->\n", reviewBacklogSplitMarker, item.Key)
	fmt.Fprintf(&b, "The folded finding %q (`%s:%d`) was still unchecked when this issue closed, so it was split out into %s.\n\n%s\n", item.Finding.Title, item.Finding.File, item.Finding.Line, ref.Key, reviewBacklogFooter)
	return b.String()
}

// ReviewBacklogSplitEvent records one folded finding split out of a closed
// host, for the audit log.
type ReviewBacklogSplitEvent struct {
	Repo string
	Host int
	// Number is the issue the finding now lives in; Reused is true when the
	// sink folded it into another open issue rather than filing a new one.
	Number int
	Reused bool
	Key    string
}

// ReviewBacklogSplitSweepOptions configures SweepReviewBacklogSplits.
type ReviewBacklogSplitSweepOptions struct {
	// Since bounds the listing to issues updated after it. Zero means
	// now minus DefaultClosedIssueStageSweepWindow.
	Since time.Time
	// MaxActions caps the findings filed in one pass. Zero means
	// DefaultReviewBacklogSplitMaxActions.
	MaxActions int
	// Audit, when set, is called once per finding split out.
	Audit func(ReviewBacklogSplitEvent)
}

// ReviewBacklogSplitResult reports what one split pass did.
type ReviewBacklogSplitResult struct {
	// Hosts counts closed backlog hosts whose comments were read.
	Hosts int
	Split []ReviewBacklogSplitEvent
	// Skipped counts hosts whose comments could not be read or whose split
	// failed part-way, plus eligible findings the sink filed nothing for
	// (a maintainer rejected a twin elsewhere).
	Skipped int
	// Deferred counts closed backlog hosts in the window that were not
	// read because the pass had already filed MaxActions findings. They
	// are picked up by a later pass only while they stay inside its
	// window, so the caller should log a non-zero count.
	Deferred int
}

// SweepReviewBacklogSplits re-files still-unchecked findings folded into an
// issue that has since closed (hivecommons/hive#13914). A folded finding
// lives only in a hive:review-backlog-link comment on its host, so closing
// the host used to close the finding with it. Hosts are the closed issues
// the review-backlog ledger points findings at; each unchecked folded item
// that passes reviewBacklogSplitEligible is filed through the github_issue
// backlog sink, the host gets a cross-link comment, and the ledger's item
// and fingerprint records move to the new issue. Ledger SplitKeys
// (link-marker key → hosts already split from) make re-runs idempotent.
// One pass files at most opts.MaxActions findings; hosts left unread once
// the cap is hit are counted in Deferred rather than silently dropped.
func (c *Client) SweepReviewBacklogSplits(ctx context.Context, opts ReviewBacklogSplitSweepOptions) (*ReviewBacklogSplitResult, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultReviewBacklogSplitMaxActions
	}
	since := opts.Since
	if since.IsZero() {
		since = time.Now().Add(-DefaultClosedIssueStageSweepWindow)
	}
	hosts, err := loadReviewBacklogHostIndex()
	if err != nil {
		return nil, err
	}
	result := &ReviewBacklogSplitResult{}
	if len(hosts) == 0 {
		return result, nil
	}
	sink, labels := c.reviewBacklogSplitTarget()
	for _, repo := range c.getRepos() {
		repoHosts := hosts[reviewBacklogRepoKey(repo)]
		if len(repoHosts) == 0 {
			continue
		}
		capped := len(result.Split) >= maxActions
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listClosedIssuesSince(ctx, owner, repoName, since)
		if err != nil {
			if capped {
				// Only counting deferred hosts by now; a listing failure
				// is not worth aborting the pass over.
				if c.logger != nil {
					c.logger.Warn("review backlog split: could not count deferred hosts", "repo", repo, "error", err)
				}
				continue
			}
			return result, err
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() || !strings.EqualFold(issue.GetState(), "closed") || issue.GetComments() == 0 || !repoHosts[issue.GetNumber()] {
				continue
			}
			if len(result.Split) >= maxActions {
				result.Deferred++
				continue
			}
			result.Hosts++
			comments, err := c.listIssueComments(ctx, owner, repoName, issue.GetNumber())
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("review backlog split skipped host", "repo", repo, "issue", issue.GetNumber(), "error", err)
				}
				result.Skipped++
				continue
			}
			bodies := make([]string, 0, len(comments))
			for _, cm := range comments {
				if !c.reviewBacklogCommentIsOurs(cm) {
					continue
				}
				bodies = append(bodies, cm.GetBody())
			}
			// Load, split and save under the ledger lock, per host, so the
			// review filing path (fileOutOfScopeReviewBacklog) and this
			// sweep never overwrite each other's ledger update. Events are
			// recorded and audited before a ledger error aborts the pass:
			// the issues they name are already filed and cross-linked.
			split, err := c.splitReviewBacklogHostLocked(ctx, sink, labels, repo, issue, bodies, maxActions-len(result.Split))
			for _, ev := range split.Events {
				result.Split = append(result.Split, ev)
				if opts.Audit != nil {
					opts.Audit(ev)
				}
			}
			result.Skipped += split.Skipped
			if split.Err != nil {
				if c.logger != nil {
					c.logger.Warn("review backlog split failed", "repo", repo, "issue", issue.GetNumber(), "error", split.Err)
				}
				result.Skipped++
			}
			if err != nil {
				return result, err
			}
		}
	}
	if result.Deferred > 0 && c.logger != nil {
		c.logger.Warn("review backlog split: per-pass cap reached, hosts deferred to a later pass", "max_actions", maxActions, "deferred_hosts", result.Deferred)
	}
	return result, nil
}

// reviewBacklogHostSplit is what splitting one host produced.
type reviewBacklogHostSplit struct {
	Events []ReviewBacklogSplitEvent
	// Skipped counts eligible findings the sink filed nothing for.
	Skipped int
	// Err is the per-host split error; the sweep records a skip and moves
	// on. Events already filed before it are still reported.
	Err error
}

// splitReviewBacklogHostLocked runs splitReviewBacklogHost for one host
// inside the ledger lock: load, split, save. The returned error is a
// ledger load or save error (the sweep aborts); a save error still
// returns the events filed so the caller can audit them.
func (c *Client) splitReviewBacklogHostLocked(ctx context.Context, sink ReviewBacklogSink, labels []string, repo string, host *gh.Issue, bodies []string, remaining int) (reviewBacklogHostSplit, error) {
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	ledger, err := loadReviewBacklog("")
	if err != nil {
		return reviewBacklogHostSplit{}, err
	}
	now := time.Now()
	split := c.splitReviewBacklogHost(ctx, sink, labels, repo, host, bodies, ledger, now, remaining)
	if len(split.Events) > 0 {
		if err := ledger.save("", now); err != nil {
			return split, err
		}
	}
	return split, nil
}

// reviewBacklogCommentIsOurs reports whether a host comment was written by
// this hive's App bot. Only such comments may be parsed for folded findings:
// the fold marker and `- [ ]` lines are plain text anyone can post on a
// public issue, and a split re-files them as hive-authored issues and
// repoints ledger items, so the author is the trust boundary. Only the
// configured hive bot logins match; with none configured (PAT auth) this
// fails closed rather than admitting arbitrary third-party bots.
func (c *Client) reviewBacklogCommentIsOurs(cm *gh.IssueComment) bool {
	if cm == nil || cm.GetUser() == nil {
		return false
	}
	logins := c.hiveBotLogins()
	if len(logins) == 0 {
		return false
	}
	return isHiveBotLogin(logins, cm.GetUser().GetLogin())
}

// loadReviewBacklogHostIndex reads the ledger once to build
// reviewBacklogHostIndex; the sweep reloads the ledger per host under the
// lock before mutating it.
func loadReviewBacklogHostIndex() (map[string]map[int]bool, error) {
	reviewBacklogLedgerMu.Lock()
	defer reviewBacklogLedgerMu.Unlock()
	l, err := loadReviewBacklog("")
	if err != nil {
		return nil, err
	}
	return reviewBacklogHostIndex(l), nil
}

// reviewBacklogHostIndex is repo → GitHub issue numbers the ledger has
// filed or folded findings into.
func reviewBacklogHostIndex(l *reviewBacklogLedger) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	for _, rec := range l.Items {
		if rec.IssueNumber <= 0 || (rec.Destination != "" && rec.Destination != config.ReviewBacklogGitHubIssue) {
			continue
		}
		key := reviewBacklogRepoKey(rec.Repo)
		if out[key] == nil {
			out[key] = map[int]bool{}
		}
		out[key][rec.IssueNumber] = true
	}
	return out
}

// reviewBacklogSplitTarget is the github_issue sink and labels split items
// are filed with: folded hosts are GitHub issues, so the split lands next
// to them whatever review.backlog.destination says, and carries the
// agent/reviewer label when that is not github_issue.
func (c *Client) reviewBacklogSplitTarget() (ReviewBacklogSink, []string) {
	routing, _ := c.reviewBacklogRoutingConfig()
	sink, labels := c.reviewBacklogTarget(routing)
	if sink.Destination() != config.ReviewBacklogGitHubIssue {
		sink = c.ReviewBacklogIssueSink()
		labels = appendLabelOnce(labels, reviewBacklogLabel)
	}
	return sink, labels
}

// splitReviewBacklogHost files every eligible folded item in comments on
// the closed host and updates ledger in place. It stops after remaining
// splits (the pass budget left for this host); the caller saves the
// ledger. An eligible item the sink files nothing for — a maintainer
// rejected the same finding elsewhere — is logged and counted in Skipped,
// not marked split, so it is retried while the host stays in the window.
func (c *Client) splitReviewBacklogHost(ctx context.Context, sink ReviewBacklogSink, labels []string, repo string, host *gh.Issue, comments []string, ledger *reviewBacklogLedger, now time.Time, remaining int) reviewBacklogHostSplit {
	owner, repoName := c.splitRepo(repo)
	hostNumber := host.GetNumber()
	stateReason := host.GetStateReason()
	var out reviewBacklogHostSplit
	for _, body := range comments {
		if len(out.Events) >= remaining {
			break
		}
		item, ok := parseReviewBacklogFoldedComment(body)
		if !ok || ledger.splitFrom(item.Key, hostNumber) {
			continue
		}
		if !reviewBacklogSplitEligible(item, stateReason, host.GetTitle(), host.GetBody()) {
			continue
		}
		ref, err := sink.CreateItem(ctx, ReviewBacklogItem{
			Repo:        repo,
			Title:       item.Title,
			Body:        reviewBacklogSplitBody(owner, repoName, hostNumber, stateReason, item),
			Labels:      labels,
			Perspective: item.Perspective,
			Finding:     item.Finding,
			SplitFrom:   hostNumber,
		})
		if err != nil && ref.ID == "" {
			out.Err = err
			return out
		}
		createErr := err
		if ref.ID == "" {
			if c.logger != nil {
				c.logger.Warn("review backlog split: sink filed nothing for folded finding", "repo", repo, "host", hostNumber, "key", item.Key, "finding", item.Finding.Title)
			}
			out.Skipped++
			continue
		}
		if ref.Reused && createErr == nil {
			// The sink folded it into another open twin: carry the whole
			// folded comment there so that twin's close splits it again.
			if err := sink.AppendToItem(ctx, repo, ref, item.Comment); err != nil {
				out.Err = err
				return out
			}
		}
		hostRef := ReviewBacklogRef{ID: strconv.Itoa(hostNumber), Key: fmt.Sprintf("#%d", hostNumber), Number: hostNumber}
		if err := sink.AppendToItem(ctx, repo, hostRef, reviewBacklogSplitHostComment(item, ref)); err != nil && c.logger != nil {
			c.logger.Warn("review backlog split: host cross-link comment failed", "repo", repo, "issue", hostNumber, "split", ref.Key, "error", err)
		}
		ledger.recordReviewBacklogSplit(repo, hostNumber, item, ref, sink.Destination(), now)
		if ref.Number > 0 {
			c.recordWriteAudit(AuditActionReviewBacklogIssueFiled, hiveWriteMeta(),
				WriteTarget{Repo: repo, Number: ref.Number},
				"split_from", strconv.Itoa(hostNumber),
				"pr", strconv.Itoa(item.PRNumber),
				"perspective", string(item.Perspective),
				"reused", strconv.FormatBool(ref.Reused),
				"destination", sink.Destination())
		}
		if c.logger != nil {
			c.logger.Info("review backlog: folded finding split out of closed host", "repo", repo, "host", hostNumber, "issue", ref.Key, "reused", ref.Reused)
		}
		out.Events = append(out.Events, ReviewBacklogSplitEvent{Repo: repo, Host: hostNumber, Number: ref.Number, Reused: ref.Reused, Key: item.Key})
		if createErr != nil {
			// The item exists; the ledger records it so a retry does not
			// file it twice.
			out.Err = createErr
			return out
		}
	}
	return out
}

// recordReviewBacklogSplit points the folded finding's ledger records at
// the item it was split out into and marks key done for host. Hosts
// accumulate: when the split lands in an open twin that later closes, the
// key is split again from the twin and both hosts stay recorded, so a
// re-listed original host is still a no-op.
func (l *reviewBacklogLedger) recordReviewBacklogSplit(repo string, host int, item reviewBacklogFoldedItem, ref ReviewBacklogRef, destination string, now time.Time) {
	l.markSplitFrom(item.Key, host)
	fp := item.Fingerprint
	if rec, ok := l.Items[item.Key]; ok {
		rec.IssueNumber, rec.IssueURL, rec.ItemKey, rec.Destination = ref.Number, ref.URL, ref.Key, destination
		l.Items[item.Key] = rec
		if fp == "" {
			fp = rec.Fingerprint
		}
	}
	if fp == "" {
		return
	}
	known, ok := l.fingerprint(repo, fp)
	if ok && known.ItemNumber != host {
		// Another item already owns this fingerprint; leave it.
		return
	}
	if !ok {
		known = reviewBacklogFingerprintRecord{FiledAt: now.UTC()}
		if item.PRNumber > 0 {
			known.PRs = []int{item.PRNumber}
		}
	}
	known.Destination, known.ItemID, known.ItemKey, known.ItemNumber, known.ItemURL = destination, ref.ID, ref.Key, ref.Number, ref.URL
	known.UpdatedAt = now.UTC()
	l.setFingerprint(repo, fp, known)
}

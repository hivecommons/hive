package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// A hive-authored advisory checklist resolves itself (hivecommons/hive#15754).
// hivecommons/hive#15734 was a dedupe checklist parked at 045 "Decision
// required: yes"; by the time the owner looked, the hive had already closed
// or merged most of its items, yet the row still asked a person to click.
// Each close-on-merge backfill now ticks `- [ ] #N …` to `- [x]` once #N is
// closed or merged and, when every item is resolved, posts one short comment
// and closes the checklist as completed — for every hive-authored dedupe
// checklist, not only the triage/advisory-labelled ones. The
// dedupe.advisory_autoclose knob (default on) turns it off.

// advisoryChecklistItemPattern is one task-list line: the box and the text
// after it.
var advisoryChecklistItemPattern = regexp.MustCompile(`(?m)^([ \t]*[-*+][ \t]+\[)([ xX])(\][ \t]*)(.*)$`)

// advisoryChecklistLeadRefPattern is the item a line is about: the first
// reference, optionally after "PR" / "issue".
var advisoryChecklistLeadRefPattern = regexp.MustCompile(`^(?i:(?:pr|issue)[ \t]+)?(?:([\w.-]+/[\w.-]+))?#(\d+)\b`)

// Result reasons of the item pass; a backfill pass that only ticked boxes
// or found items still open leaves a triage/advisory checklist to the
// label path's own refs/expiry rules.
const (
	advisoryItemsResolvedReason = "advisory_items_resolved"
	advisoryItemsOpenReason     = "advisory_items_open"
	advisoryItemsTickedReason   = "advisory_items_ticked"
)

// advisoryChecklistItem is one task-list line of a checklist. Ref is the
// zero value for a line that does not lead with an item: nothing can
// resolve it but a person ticking it.
type advisoryChecklistItem struct {
	Ref  ClaimedRef
	Done bool
	// boxAt is the byte offset of the box character in the body.
	boxAt int
}

// advisoryChecklistItems lists the task-list lines of body, resolving bare
// numbers against defaultRepo.
func advisoryChecklistItems(body, defaultRepo string) []advisoryChecklistItem {
	var out []advisoryChecklistItem
	for _, m := range advisoryChecklistItemPattern.FindAllStringSubmatchIndex(body, -1) {
		it := advisoryChecklistItem{Done: body[m[4]:m[5]] != " ", boxAt: m[4]}
		if ref := advisoryChecklistLeadRefPattern.FindStringSubmatch(strings.TrimSpace(body[m[8]:m[9]])); ref != nil {
			if n, err := strconv.Atoi(ref[2]); err == nil && n > 0 {
				it.Ref = ClaimedRef{Repo: defaultRepo, Issue: n}
				if ref[1] != "" {
					it.Ref.Repo = ref[1]
				}
			}
		}
		out = append(out, it)
	}
	return out
}

// AdvisoryChecklistProgress counts a checklist body's task-list items and
// how many are ticked; the stage tracker reads "k/N resolved" from it.
func AdvisoryChecklistProgress(body string) (items, resolved int) {
	for _, it := range advisoryChecklistItems(strings.ReplaceAll(body, "\r\n", "\n"), "") {
		items++
		if it.Done {
			resolved++
		}
	}
	return items, resolved
}

// advisoryRefOutcome is how a referenced item was resolved.
type advisoryRefOutcome string

const (
	advisoryRefOpen   advisoryRefOutcome = "open"
	advisoryRefClosed advisoryRefOutcome = "closed"
	advisoryRefMerged advisoryRefOutcome = "merged"
)

// dedupeRefOutcome reads a referenced issue or PR through the issues API;
// a PR's merged_at tells merged from closed.
func (c *Client) dedupeRefOutcome(ctx context.Context, ref ClaimedRef) (advisoryRefOutcome, error) {
	owner, repoName := c.splitRepo(ref.Repo)
	item, _, err := c.client.Issues.Get(ctx, owner, repoName, ref.Issue)
	if err != nil {
		return "", err
	}
	switch {
	case !strings.EqualFold(item.GetState(), "closed"):
		return advisoryRefOpen, nil
	case item.GetPullRequestLinks().GetMergedAt().Time.IsZero():
		return advisoryRefClosed, nil
	}
	return advisoryRefMerged, nil
}

// advisoryItemsResolvedComment is the one closing comment: "All 3 items
// resolved (#1 closed, #2 merged, #3 ticked) — closing this advisory."
func advisoryItemsResolvedComment(defaultRepo string, items []advisoryChecklistItem, outcomes []advisoryRefOutcome) string {
	parts := make([]string, 0, len(items))
	for i, it := range items {
		word := string(outcomes[i])
		if outcomes[i] == "" {
			word = "resolved"
		}
		if it.Ref.Issue <= 0 {
			parts = append(parts, "item "+strconv.Itoa(i+1)+" "+word)
			continue
		}
		ref := "#" + strconv.Itoa(it.Ref.Issue)
		if !strings.EqualFold(it.Ref.Repo, defaultRepo) {
			ref = it.Ref.Repo + ref
		}
		parts = append(parts, ref+" "+word)
	}
	noun := "items"
	if len(items) == 1 {
		noun = "item"
	}
	return fmt.Sprintf("%s\nAll %d %s resolved (%s) — closing this advisory.", dedupeAdvisoryCloseMarker, len(items), noun, strings.Join(parts, ", "))
}

// sweepAdvisoryChecklistItems is the item pass over one open issue. It
// reports handled=false for anything that is not a hive-authored dedupe
// checklist with task-list items, or when dedupe.advisory_autoclose is off.
// Otherwise it ticks the items whose reference is closed or merged and,
// once every item is resolved, closes the checklist as completed.
func (c *Client) sweepAdvisoryChecklistItems(ctx context.Context, repo string, issue *gh.Issue, opts CloseOnMergeOptions) (CloseOnMergeResult, bool) {
	res := CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber()}
	if opts.DisableAdvisoryAutoclose {
		return res, false
	}
	body := strings.ReplaceAll(issue.GetBody(), "\r\n", "\n")
	refRepo, _, ok := parseDedupeChecklist(body)
	if !ok {
		return res, false
	}
	author := safeGetLogin(issue.GetUser())
	if !opts.Identity.IsZero() && !opts.Identity.Matches(author) {
		return res, false
	}
	owner, repoName := c.splitRepo(repo)
	fullRepo := owner + "/" + repoName
	if refRepo == "" {
		refRepo = fullRepo
	}
	items := advisoryChecklistItems(body, refRepo)
	if len(items) == 0 || len(items) > dedupeAdvisoryMaxRefs {
		return res, false
	}
	ctx = WithRESTCaller(ctx, "hive:close_on_merge")
	number := issue.GetNumber()

	outcomes := make([]advisoryRefOutcome, len(items))
	ticked := []byte(body)
	changed, open := false, 0
	for i, it := range items {
		if it.Done {
			continue
		}
		if it.Ref.Issue <= 0 || (it.Ref.Issue == number && strings.EqualFold(it.Ref.Repo, fullRepo)) {
			open++
			continue
		}
		outcome, err := c.dedupeRefOutcome(ctx, it.Ref)
		if err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
			if opts.Logger != nil {
				opts.Logger.Warn("advisory checklist: reading referenced item failed", "repo", repo, "issue", number, "ref", fmt.Sprintf("%s#%d", it.Ref.Repo, it.Ref.Issue), "error", err)
			}
			return res, true
		}
		if outcome == advisoryRefOpen {
			open++
			continue
		}
		outcomes[i] = outcome
		ticked[it.boxAt] = 'x'
		changed = true
	}

	item := writeback.Item{Number: number, Author: author, Labels: issueLabelNames(issue.Labels), CreatedAt: issue.GetCreatedAt().Time, HiveAuthored: true}
	if changed {
		if d := c.WritebackAllowed(repo, item, writeback.KindIssueClose, "tick resolved advisory checklist items"); !d.Allowed {
			res.Action, res.Reason = CloseOnMergeSkipped, "writeback_gated"
			return res, true
		}
		if _, _, err := c.client.Issues.Edit(ctx, owner, repoName, number, &gh.IssueRequest{Body: gh.Ptr(string(ticked))}); err != nil { // writeback:gated
			res.Action, res.Reason = CloseOnMergeNoop, "advisory_tick_failed"
			return res, true
		}
	}
	if open > 0 {
		res.Action, res.Reason = CloseOnMergeNoop, advisoryItemsOpenReason
		if changed {
			res.Reason = advisoryItemsTickedReason
		}
		return res, true
	}
	if d := c.WritebackAllowed(repo, item, writeback.KindIssueClose, "close resolved advisory checklist"); !d.Allowed {
		res.Action, res.Reason = CloseOnMergeSkipped, "writeback_gated"
		return res, true
	}
	// Items an earlier sweep ticked are read once more for the comment's
	// "closed" / "merged"; a failed read just says "resolved".
	for i, it := range items {
		if it.Done && it.Ref.Issue > 0 {
			if outcome, err := c.dedupeRefOutcome(ctx, it.Ref); err == nil && outcome != advisoryRefOpen {
				outcomes[i] = outcome
			}
		}
	}
	return c.closeDedupeAdvisory(ctx, res, repo, owner, repoName, number, advisoryItemsResolvedComment(refRepo, items, outcomes), advisoryItemsResolvedReason), true
}

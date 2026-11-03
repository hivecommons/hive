package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The dedupe agent files one checklist per repository and starts its body with
// dedupeChecklistMarkerPrefix; line two is the verdict, "Decision required:
// yes|no" (hivecommons/hive#11864). The policy forbids the agent from closing
// anything, and the owner should never have to click through a checklist the
// hive is already working (hivecommons/hive#15754): the close-on-merge
// backfill ticks each `- [ ] #N …` item as #N closes or merges and closes the
// checklist as completed once every item is resolved. The checklist is never
// parked on a person, whatever its verdict line says.
const (
	dedupeChecklistMarkerPrefix = "<!-- hive:dedupe-checklist repo="
	// dedupeAdvisoryCloseMarker tags the hive's closing comment. An open
	// checklist that already carries it was reopened by a person after the
	// hive closed it, and is left alone.
	dedupeAdvisoryCloseMarker = "<!-- hive-dedupe-advisory-close -->"
	// DedupeAdvisoryMaxAge is how long an advisory checklist stays open
	// while some item it names is still open.
	DedupeAdvisoryMaxAge = 7 * 24 * time.Hour
	// dedupeAdvisoryMaxRefs bounds the API reads one checklist may cost per
	// sweep; a checklist naming more items than this only expires.
	dedupeAdvisoryMaxRefs = 100
	// dedupeAdvisoryMaxNamed bounds the items the closing comment lists by
	// name before it says "and N more".
	dedupeAdvisoryMaxNamed = 8
)

var (
	dedupeChecklistRepoPattern = regexp.MustCompile(`^<!-- hive:dedupe-checklist repo=([\w.-]+/[\w.-]+)\s*-->`)
	dedupeDecisionPattern      = regexp.MustCompile(`(?i)^decision required:\s*(yes|no)\b`)
	htmlCommentPattern         = regexp.MustCompile(`(?s)<!--.*?-->`)
	// dedupeTaskLineOpenPattern is one unticked task line; the sweep
	// rewrites its box to `[x]` once the item it names is resolved.
	dedupeTaskLineOpenPattern = regexp.MustCompile(`^(\s*[-*]\s+)\[ \](.*)$`)
	dedupeTaskLineDonePattern = regexp.MustCompile(`^\s*[-*]\s+\[[xX]\]`)
)

const dedupeAdvisoryExpiredComment = dedupeAdvisoryCloseMarker + "\nClosing: this advisory dedupe checklist expired after 7 days."

// dedupeChecklistVerdict is the "Decision required" line under the marker.
type dedupeChecklistVerdict int

const (
	dedupeVerdictUnknown dedupeChecklistVerdict = iota
	dedupeVerdictNo
	dedupeVerdictYes
)

// parseDedupeChecklist reports whether body is a dedupe checklist, the repo
// its marker names, and the verdict on the first non-blank line after it.
func parseDedupeChecklist(body string) (repo string, verdict dedupeChecklistVerdict, ok bool) {
	body = strings.TrimLeft(strings.ReplaceAll(body, "\r\n", "\n"), " \t\n")
	if !strings.HasPrefix(body, dedupeChecklistMarkerPrefix) {
		return "", dedupeVerdictUnknown, false
	}
	lines := strings.Split(body, "\n")
	if m := dedupeChecklistRepoPattern.FindStringSubmatch(lines[0]); m != nil {
		repo = m[1]
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := dedupeDecisionPattern.FindStringSubmatch(line); m != nil {
			if strings.EqualFold(m[1], "yes") {
				return repo, dedupeVerdictYes, true
			}
			return repo, dedupeVerdictNo, true
		}
		break
	}
	return repo, dedupeVerdictUnknown, true
}

// dedupeChecklistRefs lists the distinct items a checklist names as `#N` or
// `owner/repo#N`, resolving bare numbers against defaultRepo and leaving out
// the checklist itself.
func dedupeChecklistRefs(body, defaultRepo, selfRepo string, self int) []ClaimedRef {
	body = htmlCommentPattern.ReplaceAllString(body, "")
	seen := map[string]bool{}
	var out []ClaimedRef
	for _, m := range issueRefPattern.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil || n <= 0 {
			continue
		}
		repo := defaultRepo
		if m[1] != "" {
			repo = m[1]
		}
		if n == self && strings.EqualFold(repo, selfRepo) {
			continue
		}
		key := strings.ToLower(repo) + "#" + strconv.Itoa(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ClaimedRef{Repo: repo, Issue: n})
	}
	return out
}

// dedupeChecklistItem is one task line of a checklist: its line index in
// the body, whether it is ticked, and the item it is about — the first
// reference on the line ("- [ ] #15312 — close as superseded by #15646" is
// about #15312; it is resolved when #15312 closes, whatever #15646 does).
type dedupeChecklistItem struct {
	Line int
	Done bool
	Ref  ClaimedRef
}

// dedupeChecklistItems reads the task lines of body. A task line that names
// nothing is skipped: there is no item whose state could resolve it.
func dedupeChecklistItems(body, defaultRepo, selfRepo string, self int) []dedupeChecklistItem {
	var out []dedupeChecklistItem
	for i, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		done := dedupeTaskLineDonePattern.MatchString(line)
		if !done && !dedupeTaskLineOpenPattern.MatchString(line) {
			continue
		}
		refs := dedupeChecklistRefs(line, defaultRepo, selfRepo, self)
		if len(refs) == 0 {
			continue
		}
		out = append(out, dedupeChecklistItem{Line: i, Done: done, Ref: refs[0]})
	}
	return out
}

// dedupeTickItems returns body with the task lines at the given indexes
// ticked. The body is otherwise untouched, so the write is idempotent.
func dedupeTickItems(body string, lines []int) string {
	tick := map[int]bool{}
	for _, l := range lines {
		tick[l] = true
	}
	parts := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i := range parts {
		if !tick[i] {
			continue
		}
		if m := dedupeTaskLineOpenPattern.FindStringSubmatch(parts[i]); m != nil {
			parts[i] = m[1] + "[x]" + m[2]
		}
	}
	return strings.Join(parts, "\n")
}

// dedupeResolvedRef is one resolved item with the word for how it resolved.
type dedupeResolvedRef struct {
	Ref  ClaimedRef
	Word string
}

// dedupeAdvisoryResolvedComment is the closing note: "All 4 items resolved
// (#15312 closed, #15646 merged, …) — closing this advisory."
func dedupeAdvisoryResolvedComment(selfRepo string, resolved []dedupeResolvedRef) string {
	named := make([]string, 0, len(resolved))
	for i, r := range resolved {
		if i == dedupeAdvisoryMaxNamed {
			named = append(named, fmt.Sprintf("and %d more", len(resolved)-i))
			break
		}
		ref := "#" + strconv.Itoa(r.Ref.Issue)
		if !strings.EqualFold(r.Ref.Repo, selfRepo) {
			ref = r.Ref.Repo + ref
		}
		named = append(named, ref+" "+r.Word)
	}
	noun := "items"
	if len(resolved) == 1 {
		noun = "item"
	}
	return fmt.Sprintf("%s\nAll %d %s resolved (%s) — closing this advisory.", dedupeAdvisoryCloseMarker, len(resolved), noun, strings.Join(named, ", "))
}

// sweepDedupeAdvisoryChecklist handles one open issue for the close-on-merge
// backfill. It reports handled=false for anything that is not an open
// hive-authored dedupe checklist, so the backfill's own checks still run.
// Otherwise it ticks the checklist's resolved items, and closes it once every
// item is resolved or it is older than DedupeAdvisoryMaxAge.
func (c *Client) sweepDedupeAdvisoryChecklist(ctx context.Context, repo string, issue *gh.Issue, opts CloseOnMergeOptions) (CloseOnMergeResult, bool) {
	res := CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber()}
	refRepo, _, ok := parseDedupeChecklist(issue.GetBody())
	if !ok {
		return res, false
	}
	if login := safeGetLogin(issue.GetUser()); login != "" && !strings.EqualFold(issue.GetUser().GetType(), "Bot") && !c.IsHiveAuthorLogin(login) {
		// A person who pasted the marker wrote a note, not a hive checklist.
		return res, false
	}
	if opts.AdvisoryAutocloseOff {
		res.Action, res.Reason = CloseOnMergeSkipped, "dedupe_advisory_autoclose_off"
		return res, true
	}
	if refRepo == "" {
		refRepo = repo
	}
	ctx = WithRESTCaller(ctx, "hive:close_on_merge")
	owner, repoName := c.splitRepo(repo)
	fullRepo := owner + "/" + repoName
	number := issue.GetNumber()
	labels := issueLabelNames(issue.Labels)
	gateItem := writeback.Item{Number: number, Author: safeGetLogin(issue.GetUser()), Labels: labels, CreatedAt: issue.GetCreatedAt().Time, HiveAuthored: true}

	now := opts.now()
	expired := !issue.GetCreatedAt().Time.IsZero() && now.Sub(issue.GetCreatedAt().Time) >= DedupeAdvisoryMaxAge
	comment, reason := dedupeAdvisoryExpiredComment, "dedupe_advisory_expired"
	if !expired {
		resolved, pending, tick, err := c.dedupeChecklistProgress(ctx, issue.GetBody(), refRepo, fullRepo, number)
		if err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, err.reason
			if err.cause != nil && opts.Logger != nil {
				opts.Logger.Warn("dedupe advisory sweep: reading referenced item failed", "repo", repo, "issue", number, "ref", err.ref, "error", err.cause)
			}
			return res, true
		}
		if len(tick) > 0 {
			body := dedupeTickItems(issue.GetBody(), tick)
			if d := c.WritebackAllowed(fullRepo, gateItem, writeback.KindParentIssueEdit, "tick resolved advisory items"); d.Allowed {
				if _, _, err := c.client.Issues.Edit(ctx, owner, repoName, number, &gh.IssueRequest{Body: gh.Ptr(body)}); err != nil { // writeback:gated
					res.Action, res.Reason = CloseOnMergeNoop, "body_edit_failed"
					if opts.Logger != nil {
						opts.Logger.Warn("dedupe advisory sweep: ticking resolved items failed", "repo", repo, "issue", number, "error", err)
					}
					return res, true
				}
				res.Ticked = len(tick)
			}
		}
		if pending > 0 {
			res.Action, res.Reason = CloseOnMergeNoop, "dedupe_advisory_refs_open"
			return res, true
		}
		comment, reason = dedupeAdvisoryResolvedComment(fullRepo, resolved), "dedupe_advisory_refs_closed"
	}

	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
		return res, true
	}
	for _, cm := range comments {
		if cm != nil && strings.Contains(cm.GetBody(), dedupeAdvisoryCloseMarker) {
			res.Action, res.Reason = CloseOnMergeSkipped, "reopened_after_close"
			return res, true
		}
	}
	if d := c.WritebackAllowed(fullRepo, gateItem, writeback.KindIssueClose, "close resolved advisory checklist"); !d.Allowed {
		res.Action, res.Reason = CloseOnMergeSkipped, "writeback_gated"
		return res, true
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(comment)}); err != nil { // writeback:gated
		res.Action, res.Reason = CloseOnMergeNoop, "close_comment_failed"
		return res, true
	}
	if err := c.CloseIssue(ctx, repo, number, IssueCloseOptions{ // writeback:gated
		OverrideReason:          "advisory dedupe checklist",
		SuppressOverrideComment: true,
		// Not "not_planned": that would read as a maintainer rejecting the
		// dedupe lane's filing (see rejectedStateReasons).
		StateReason: IssueStateReasonCompleted,
	}); err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_failed"
		return res, true
	}
	res.Action, res.Reason = CloseOnMergeClosed, reason
	return res, true
}

// dedupeProgressError is a read that stopped the sweep: its result reason
// and, for a failed API read, the item and cause for the log.
type dedupeProgressError struct {
	reason string
	ref    string
	cause  error
}

// dedupeChecklistProgress reads the state of every item a checklist names.
// With task lines, the items are the task lines (an already-ticked line
// costs no read); without any, every reference in the body is an item, as
// the older checklists had no task lines. It returns the resolved items,
// how many are still open, and the task lines to tick.
func (c *Client) dedupeChecklistProgress(ctx context.Context, body, refRepo, fullRepo string, number int) (resolved []dedupeResolvedRef, pending int, tick []int, perr *dedupeProgressError) {
	items := dedupeChecklistItems(body, refRepo, fullRepo, number)
	if len(items) == 0 {
		for _, ref := range dedupeChecklistRefs(htmlCommentPattern.ReplaceAllString(body, ""), refRepo, fullRepo, number) {
			items = append(items, dedupeChecklistItem{Line: -1, Ref: ref})
		}
	}
	if len(items) == 0 || len(items) > dedupeAdvisoryMaxRefs {
		return nil, 0, nil, &dedupeProgressError{reason: "dedupe_advisory_no_refs_to_track"}
	}
	for _, it := range items {
		if it.Done {
			resolved = append(resolved, dedupeResolvedRef{Ref: it.Ref, Word: "resolved"})
			continue
		}
		open, word, err := c.dedupeRefState(ctx, it.Ref)
		if err != nil {
			return nil, 0, nil, &dedupeProgressError{reason: "issue_read_failed", ref: fmt.Sprintf("%s#%d", it.Ref.Repo, it.Ref.Issue), cause: err}
		}
		if open {
			pending++
			continue
		}
		resolved = append(resolved, dedupeResolvedRef{Ref: it.Ref, Word: word})
		if it.Line >= 0 {
			tick = append(tick, it.Line)
		}
	}
	return resolved, pending, tick, nil
}

// dedupeRefState reports whether a referenced issue or PR is still open and,
// when it is not, the word for how it ended ("merged" for a merged PR,
// "closed" otherwise). The issues API serves both.
func (c *Client) dedupeRefState(ctx context.Context, ref ClaimedRef) (open bool, word string, err error) {
	owner, repoName := c.splitRepo(ref.Repo)
	item, _, err := c.client.Issues.Get(ctx, owner, repoName, ref.Issue)
	if err != nil {
		return false, "", err
	}
	if !strings.EqualFold(item.GetState(), "closed") {
		return true, "", nil
	}
	if item.IsPullRequest() && item.GetPullRequestLinks().MergedAt != nil {
		return false, "merged", nil
	}
	return false, "closed", nil
}

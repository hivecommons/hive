package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// The dedupe agent files one checklist per repository and starts its body with
// dedupeChecklistMarkerPrefix; line two is the verdict, "Decision required:
// yes|no" (hivecommons/hive#11864). An advisory-only checklist carries the
// triage/advisory label instead of needs-decision, and the policy forbids the
// agent from closing anything, so the close-on-merge backfill closes it here
// (hivecommons/hive#11871).
const (
	dedupeChecklistMarkerPrefix = "<!-- hive:dedupe-checklist repo="
	// dedupeAdvisoryCloseMarker tags the hive's closing comment. An open
	// checklist that already carries it was reopened by a person after the
	// hive closed it, and is left alone.
	dedupeAdvisoryCloseMarker = "<!-- hive-dedupe-advisory-close -->"
	// DedupeAdvisoryMaxAge is how long an advisory-only checklist stays open.
	DedupeAdvisoryMaxAge = 7 * 24 * time.Hour
	// dedupeAdvisoryMaxRefs bounds the API reads one checklist may cost per
	// sweep; a checklist naming more items than this only expires.
	dedupeAdvisoryMaxRefs = 100
)

var (
	dedupeChecklistRepoPattern = regexp.MustCompile(`^<!-- hive:dedupe-checklist repo=([\w.-]+/[\w.-]+)\s*-->`)
	dedupeDecisionPattern      = regexp.MustCompile(`(?i)^decision required:\s*(yes|no)\b`)
	htmlCommentPattern         = regexp.MustCompile(`(?s)<!--.*?-->`)
)

const (
	dedupeAdvisoryRefsClosedComment = dedupeAdvisoryCloseMarker + "\nClosing: every item this advisory checklist references is now closed or merged."
	dedupeAdvisoryExpiredComment    = dedupeAdvisoryCloseMarker + "\nClosing: this advisory dedupe checklist expired after 7 days."
)

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

// sweepDedupeAdvisoryChecklist handles one open issue for the close-on-merge
// backfill. It reports handled=false for anything that is not an open
// triage/advisory dedupe checklist, so the backfill's own checks still run.
// Otherwise it reclassifies a checklist whose verdict turned to "Decision
// required: yes", or closes it once it is older than DedupeAdvisoryMaxAge or
// every item it references is closed or merged.
func (c *Client) sweepDedupeAdvisoryChecklist(ctx context.Context, repo string, issue *gh.Issue, opts CloseOnMergeOptions) (CloseOnMergeResult, bool) {
	res := CloseOnMergeResult{Repo: repo, Issue: issue.GetNumber()}
	labels := issueLabelNames(issue.Labels)
	if !labelPresent(labels, dedupeAdvisoryLabelName) {
		return res, false
	}
	refRepo, verdict, ok := parseDedupeChecklist(issue.GetBody())
	if !ok {
		return res, false
	}
	if refRepo == "" {
		refRepo = repo
	}
	ctx = WithRESTCaller(ctx, "hive:close_on_merge")
	owner, repoName := c.splitRepo(repo)
	fullRepo := owner + "/" + repoName
	number := issue.GetNumber()

	if verdict == dedupeVerdictYes {
		return c.reclassifyDedupeChecklist(ctx, repo, owner, repoName, issue, opts), true
	}
	if c.hasParkingLabel(labels) {
		res.Action, res.Reason = CloseOnMergeSkipped, "dedupe_advisory_parked"
		return res, true
	}

	now := opts.now()
	expired := !issue.GetCreatedAt().Time.IsZero() && now.Sub(issue.GetCreatedAt().Time) >= DedupeAdvisoryMaxAge
	comment, reason := dedupeAdvisoryExpiredComment, "dedupe_advisory_expired"
	if !expired {
		refs := dedupeChecklistRefs(issue.GetBody(), refRepo, fullRepo, number)
		if len(refs) == 0 || len(refs) > dedupeAdvisoryMaxRefs {
			res.Action, res.Reason = CloseOnMergeNoop, "dedupe_advisory_no_refs_to_track"
			return res, true
		}
		for _, ref := range refs {
			open, err := c.dedupeRefOpen(ctx, ref)
			if err != nil {
				res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
				if opts.Logger != nil {
					opts.Logger.Warn("dedupe advisory close: reading referenced item failed", "repo", repo, "issue", number, "ref", fmt.Sprintf("%s#%d", ref.Repo, ref.Issue), "error", err)
				}
				return res, true
			}
			if open {
				res.Action, res.Reason = CloseOnMergeNoop, "dedupe_advisory_refs_open"
				return res, true
			}
		}
		comment, reason = dedupeAdvisoryRefsClosedComment, "dedupe_advisory_refs_closed"
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
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(comment)}); err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "close_comment_failed"
		return res, true
	}
	if err := c.CloseIssue(ctx, repo, number, IssueCloseOptions{
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

// dedupeRefOpen reports whether a referenced issue or PR is still open; the
// issues API serves both, and a merged PR reads as closed.
func (c *Client) dedupeRefOpen(ctx context.Context, ref ClaimedRef) (bool, error) {
	owner, repoName := c.splitRepo(ref.Repo)
	item, _, err := c.client.Issues.Get(ctx, owner, repoName, ref.Issue)
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(item.GetState(), "closed"), nil
}

// reclassifyDedupeChecklist parks an advisory checklist the dedupe agent
// rewrote with an action-required row: it swaps triage/advisory for the
// needs-decision label and posts the "What to reply" notice, which the unpark
// sweep then keeps current. A label-driven repo gets the label only, as the
// unpark sweep posts no notice there.
func (c *Client) reclassifyDedupeChecklist(ctx context.Context, repo, owner, repoName string, issue *gh.Issue, opts CloseOnMergeOptions) CloseOnMergeResult {
	number := issue.GetNumber()
	res := CloseOnMergeResult{Repo: repo, Issue: number, Action: CloseOnMergeSkipped, Reason: "dedupe_reclassified_needs_decision"}
	label := c.needsDecisionLabel()
	if !labelPresent(issueLabelNames(issue.Labels), label) {
		if err := c.EnsureIssueLabel(ctx, repo, label, "", ""); err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "label_failed"
			return res
		}
		if err := c.AddLabels(ctx, repo, number, []string{label}); err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "label_failed"
			return res
		}
	}
	if opts.LabelDriven == nil || !(opts.LabelDriven(repo) || opts.LabelDriven(owner+"/"+repoName)) {
		comments, err := c.listIssueComments(ctx, owner, repoName, number)
		if err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "issue_read_failed"
			return res
		}
		if err := c.ensureUnparkNoticeComment(ctx, owner, repoName, number, issue.GetBody(), issue.GetUser(), comments, false); err != nil {
			res.Action, res.Reason = CloseOnMergeNoop, "notice_failed"
			return res
		}
	}
	// Removed last: until it goes, the next sweep retries the steps above.
	if err := c.RemoveLabel(ctx, repo, number, dedupeAdvisoryLabelName); err != nil {
		res.Action, res.Reason = CloseOnMergeNoop, "label_failed"
		return res
	}
	if opts.Logger != nil {
		opts.Logger.Info("dedupe checklist reclassified as needs-decision", "repo", repo, "issue", number, "label", label)
	}
	return res
}

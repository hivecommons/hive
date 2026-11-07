package github

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewFollowUpsCommentMarker tags the one PR comment that lists findings
// held below review.backlog.min_severity (hivecommons/hive#17065). It is
// edited in place on every pass, like the claim ledger, so a PR carries one
// such comment however many reviews it gets.
const reviewFollowUpsCommentMarker = "<!-- hive:review-followups-comment v1 -->"

// reviewFollowUpsSummaryMaxLen bounds the one-line summary quoted per held
// finding in the Follow-ups comment.
const reviewFollowUpsSummaryMaxLen = 200

// reviewBacklogHeldFinding is a finding below review.backlog.min_severity:
// listed in the PR's Follow-ups comment, never filed as an item.
type reviewBacklogHeldFinding struct {
	Perspective review.Perspective
	Finding     outputschema.Finding
}

// reviewBacklogPending is a finding that needs a new backlog item, held
// back until the whole report is read so findings sharing a perspective
// and scope are filed as one item.
type reviewBacklogPending struct {
	perspective review.Perspective
	finding     outputschema.Finding
	key         string
	fp          string
	record      reviewBacklogRecord
	outOfScope  bool
}

// groupReviewBacklogPending groups pending findings by family — perspective
// and scope — in first-seen order, keeping each group's findings in report
// order.
func groupReviewBacklogPending(pending []reviewBacklogPending) [][]reviewBacklogPending {
	var order []string
	groups := map[string][]reviewBacklogPending{}
	for _, p := range pending {
		k := string(p.perspective) + "\x00" + strconv.FormatBool(p.outOfScope)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	out := make([][]reviewBacklogPending, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	return out
}

type reviewBacklogGroupInput struct {
	req          ReviewRequest
	ledger       *reviewBacklogLedger
	sink         ReviewBacklogSink
	labels       []string
	destination  string
	headSHA      string
	blockAt      string
	baseProbe    *reviewBacklogBaseProbe
	changedFiles func(context.Context, string) (bool, bool)
	group        []reviewBacklogPending
	now          time.Time
}

// createReviewBacklogGroup files one backlog item for group: the finding's
// own item when the group has one, else one collapsed item with a checklist
// (hivecommons/hive#17065). Every finding in the group is recorded in the
// ledger against the item, so a later head or PR raising any of them is
// de-duplicated as before. ok is false when nothing was filed; a non-nil
// error with ok true means the item exists but a follow-up step failed.
func (c *Client) createReviewBacklogGroup(ctx context.Context, in reviewBacklogGroupInput) (reviewBacklogFiled, bool, error) {
	req, ledger, sink, group := in.req, in.ledger, in.sink, in.group
	if len(group) == 0 {
		return reviewBacklogFiled{}, false, nil
	}
	first := group[0]
	perspective := first.perspective
	var missingFile string
	blocked, filesKnown := false, true
	for _, p := range group {
		if in.baseProbe.FileMissingOnBase(ctx, p.finding.File) {
			blocked = true
			if missingFile == "" {
				missingFile = p.finding.File
			}
			continue
		}
		touched, known := in.changedFiles(ctx, p.finding.File)
		if touched || !known {
			blocked = true
		}
		if !known {
			filesKnown = false
		}
	}
	itemLabels := in.labels
	if blocked {
		itemLabels = blockedBacklogLabels(in.labels)
	}
	var title, body string
	if len(group) == 1 {
		title = reviewBacklogIssueTitle(perspective, first.finding)
		body = c.reviewBacklogIssueBody(req, perspective, first.finding, in.headSHA, in.blockAt, first.fp)
	} else {
		title = reviewBacklogGroupTitle(req, perspective, len(group))
		body = c.reviewBacklogGroupBody(req, perspective, group, in.headSHA, in.blockAt)
	}
	switch {
	case missingFile != "":
		body = blockedBacklogNote(req.Repo, req.Number, missingFile, in.baseProbe.BaseRef(ctx)) + body
	case blocked:
		note := blockedOnReviewedPRNote(req.Repo, req.Number)
		if !filesKnown {
			note = strings.Replace(note, "is about code that PR is changing", "could not be checked against the PR's changed files", 1)
		}
		body = note + body
	}
	ref, err := sink.CreateItem(ctx, ReviewBacklogItem{
		Repo:        req.Repo,
		Title:       title,
		Body:        body,
		Labels:      itemLabels,
		Perspective: perspective,
		Finding:     first.finding,
	})
	if err != nil && ref.ID == "" {
		return reviewBacklogFiled{}, false, err
	}
	createErr := err
	if ref.ID == "" {
		return reviewBacklogFiled{}, false, nil
	}
	if ref.Reused && createErr == nil {
		// hivecommons/hive#13604: the comment must carry the whole
		// finding or it is lost when the twin closes; one per finding so
		// the split sweep reads each back.
		for _, p := range group {
			if err := sink.AppendToItem(ctx, req.Repo, ref, c.reviewBacklogFoldedComment(req, perspective, p.finding, in.headSHA, p.fp, p.key)); err != nil {
				return reviewBacklogFiled{}, false, err
			}
		}
	}
	if !ref.Reused && ref.SameFileAs > 0 && createErr == nil {
		// hivecommons/hive#13906: the item exists, so a failed
		// cross-link must not fail the pass and re-file it.
		if err := sink.AppendToItem(ctx, req.Repo, ref, reviewBacklogSameFileComment(first.finding, ref.SameFileAs, ref.SameFilePath)); err != nil {
			c.logger.Warn("review backlog: same-file cross-link comment failed",
				slog.String("repo", req.Repo), slog.String("item", ref.Key),
				slog.Int("same_file_as", ref.SameFileAs), slog.String("error", err.Error()))
		}
	}
	if ref.Number > 0 {
		c.recordWriteAudit(AuditActionReviewBacklogIssueFiled, hiveWriteMeta(),
			WriteTarget{Repo: req.Repo, Number: ref.Number},
			"pr", strconv.Itoa(req.Number),
			"perspective", string(perspective),
			"findings", strconv.Itoa(len(group)),
			"reused", strconv.FormatBool(ref.Reused),
			"blocked", strconv.FormatBool(blocked),
			"destination", in.destination,
			"review_agent", req.Agent)
	}
	for _, p := range group {
		record := p.record
		record.IssueNumber, record.IssueURL, record.ItemKey = ref.Number, ref.URL, ref.Key
		ledger.Items[p.key] = record
		ledger.addPRFinding(req.Repo, req.Number, p.finding, ref, in.headSHA)
		fpRecord := reviewBacklogFingerprintRecord{
			Destination: in.destination,
			ItemID:      ref.ID,
			ItemKey:     ref.Key,
			ItemNumber:  ref.Number,
			ItemURL:     ref.URL,
			PRs:         []int{req.Number},
			FiledAt:     in.now.UTC(),
			UpdatedAt:   in.now.UTC(),
			File:        normaliseFingerprintPath(p.finding.File),
			Title:       strings.TrimSpace(p.finding.Title),
		}
		if !ref.Reused {
			// A filing folded into an open twin keeps the twin's own
			// state (#13364). Keep source provenance for new items,
			// but only park findings related to the PR.
			fpRecord.BlockedOnPR = req.Number
			if blocked {
				fpRecord.BlockState = reviewBacklogBlockBlocked
			}
		}
		ledger.setFingerprint(req.Repo, p.fp, fpRecord)
	}
	return reviewBacklogFiled{number: ref.Number, key: ref.Key, url: ref.URL, title: title, folded: ref.Reused, status: reviewBacklogListedFiled}, true, createErr
}

// reviewBacklogGroupTitle names a collapsed item: `[reviewer] Review backlog
// (<perspective>): <repo>#<pr> — N follow-ups`. It keeps the `Review backlog
// (<perspective>):` lead every backlog item has, so title canonicalisation
// and the fold matcher treat it like any other item.
func reviewBacklogGroupTitle(req ReviewRequest, p review.Perspective, n int) string {
	return reviewBacklogIssueTitle(p, outputschema.Finding{Title: fmt.Sprintf("%s#%d — %d follow-ups", strings.TrimSpace(req.Repo), req.Number, n)})
}

// reviewBacklogGroupBody is the body of a collapsed item: one checklist
// entry per finding, each with its severity, evidence, summary and
// fingerprint so nothing a per-finding item would have carried is lost.
func (c *Client) reviewBacklogGroupBody(req ReviewRequest, p review.Perspective, group []reviewBacklogPending, headSHA, blockAt string) string {
	owner, repoName := c.splitRepo(req.Repo)
	var b strings.Builder
	fmt.Fprintf(&b, "Filed from %s in the Hive review of PR #%d: %d %s findings collapsed into one item. Tick each off as it is addressed.\n\n", reviewBacklogReason(group[0].finding, blockAt), req.Number, len(group), p)
	fmt.Fprintf(&b, "PR: #%d (%s)\n", req.Number, reviewPRURL(owner, repoName, req.Number))
	fmt.Fprintf(&b, "Perspective: %s\n\n", p)
	b.WriteString("### Findings\n\n")
	for _, g := range group {
		f := g.finding
		file := strings.TrimSpace(f.File)
		fmt.Fprintf(&b, "- [ ] **%s** — %s (`%s:%d`)\n", strings.TrimSpace(f.Title), reviewFindingSeverityLine(f), file, f.Line)
		if link := reviewFindingPermalink(owner, repoName, headSHA, f); link != "" {
			fmt.Fprintf(&b, "  %s\n", link)
		}
		if summary := strings.TrimSpace(f.Summary); summary != "" {
			fmt.Fprintf(&b, "  %s\n", summary)
		}
		fmt.Fprintf(&b, "  Fingerprint: `%s`\n", g.fp)
	}
	fmt.Fprintf(&b, "\n%s\n", reviewBacklogFooter)
	return b.String()
}

// upsertReviewFollowUpsComment lists the findings held below
// review.backlog.min_severity in the PR's single Follow-ups comment, edited
// in place; entries already there (including ones a human ticked) are kept
// and new ones appended. Best effort: a failure is logged, the pass is not
// failed, and the findings return on the next review of the PR.
func (c *Client) upsertReviewFollowUpsComment(ctx context.Context, req ReviewRequest, held []reviewBacklogHeldFinding, minSeverity string) {
	res, err := c.EditIssueMarkerComment(ctx, req.Repo, req.Number, reviewFollowUpsCommentMarker, func(previous string) string {
		return renderReviewFollowUpsComment(previous, held, minSeverity)
	})
	if err != nil {
		c.logger.Warn("review backlog: follow-ups comment not updated",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.Int("held", len(held)), slog.String("error", err.Error()))
		return
	}
	if res.Created || res.Edited {
		c.recordWriteAudit(AuditActionReviewBacklogSummaryPosted, hiveWriteMeta(),
			WriteTarget{Repo: req.Repo, Number: req.Number},
			"held", strconv.Itoa(len(held)),
			"min_severity", minSeverity,
			"review_agent", req.Agent)
	}
}

// renderReviewFollowUpsComment merges held into previous (the comment's
// prior body, "" when none) and returns the full comment. Checklist lines
// are keyed on their text after the checkbox so a ticked entry stays ticked
// and a re-reported finding is not listed twice.
func renderReviewFollowUpsComment(previous string, held []reviewBacklogHeldFinding, minSeverity string) string {
	type entry struct {
		checked bool
		text    string
	}
	var entries []entry
	seen := map[string]bool{}
	for _, line := range strings.Split(previous, "\n") {
		trimmed := strings.TrimSpace(line)
		var checked bool
		var text string
		switch {
		case strings.HasPrefix(trimmed, "- [ ] "):
			text = strings.TrimSpace(strings.TrimPrefix(trimmed, "- [ ] "))
		case strings.HasPrefix(trimmed, "- [x] "), strings.HasPrefix(trimmed, "- [X] "):
			checked, text = true, strings.TrimSpace(trimmed[len("- [x] "):])
		default:
			continue
		}
		if text == "" || seen[text] {
			continue
		}
		seen[text] = true
		entries = append(entries, entry{checked: checked, text: text})
	}
	for _, h := range held {
		text := reviewFollowUpLine(h)
		if seen[text] {
			continue
		}
		seen[text] = true
		entries = append(entries, entry{text: text})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].text < entries[j].text })
	var b strings.Builder
	b.WriteString(reviewFollowUpsCommentMarker)
	fmt.Fprintf(&b, "\n### Review follow-ups (below `review.backlog.min_severity: %s`)\n", minSeverity)
	b.WriteString("Non-blocking findings below the backlog filing line are listed here instead of as issues, one comment per PR, updated in place. Tick them off as they are fixed; set `review.backlog.min_severity: P3` to file every finding.\n\n")
	for _, e := range entries {
		box := "[ ]"
		if e.checked {
			box = "[x]"
		}
		fmt.Fprintf(&b, "- %s %s\n", box, e.text)
	}
	return b.String()
}

// reviewFollowUpLine is one held finding's checklist text:
// `**P3 low** docs-currency — <title> (`file:line`): <summary>`.
func reviewFollowUpLine(h reviewBacklogHeldFinding) string {
	f := h.Finding
	text := fmt.Sprintf("**P%d %s** %s — %s (`%s:%d`)", ReviewFindingPriority(f.Severity), strings.ToLower(strings.TrimSpace(string(f.Severity))), h.Perspective, strings.TrimSpace(f.Title), strings.TrimSpace(f.File), f.Line)
	summary := strings.Join(strings.Fields(f.Summary), " ")
	if r := []rune(summary); len(r) > reviewFollowUpsSummaryMaxLen {
		summary = string(r[:reviewFollowUpsSummaryMaxLen-1]) + "…"
	}
	if summary != "" {
		text += ": " + summary
	}
	return text
}

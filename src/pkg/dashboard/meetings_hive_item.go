package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// One issue or PR for the meeting attendee ("Betty, what is pull request
// 18741 about?"). In the scored live test of 2026-11-17 she gave the title
// and the author and said she had no description or CI status: the lookup
// printed only the title, author, CI word and labels of the status row.
//
// The brief now carries a short summary of the description, the state,
// the review and CI status and what is blocking the item. Everything is
// read from memory: the published status snapshot (the row, its merge
// verdict, review state, failing checks, mergeable state and labels) and
// the forge cache's copy of the open item, which holds the body the status
// row drops. No GitHub call is made.

const (
	// hiveItemMaxChars bounds the whole item brief, so the spoken answer
	// stays short.
	hiveItemMaxChars = 700
	// hiveItemSummaryMaxChars and hiveItemSummaryMaxSentences bound the
	// summary of the description.
	hiveItemSummaryMaxChars     = 240
	hiveItemSummaryMaxSentences = 2
	// hiveItemMaxLabels bounds the labels listed.
	hiveItemMaxLabels = 5
	// hiveItemMaxFailing bounds the failing checks named.
	hiveItemMaxFailing = 3
	// hiveItemReasonCap truncates one blocking reason.
	hiveItemReasonCap = 140
	// hiveItemAnswerGuide keeps the spoken answer under about 12 seconds.
	hiveItemAnswerGuide = " Answer in at most three short sentences: what it is about, its state, and what is blocking it."

	hiveItemKindPR         = "pr"
	hiveItemCIFailure      = "failure"
	hiveItemCIRunning      = "running"
	hiveItemStateClosed    = "closed"
	hiveItemMergeDirty     = "dirty"
	hiveItemMergeBlocked   = "blocked"
	hiveItemMergeBehind    = "behind"
	hiveItemReviewChanges  = "changes_requested"
	hiveItemLabelLGTM      = "lgtm"
	hiveItemLabelApproved  = "approved"
	hiveItemNothingBlocked = "nothing recorded"
)

// HiveItemBrief describes one open issue or PR by number from the hive's
// live state, so a lookup is answered in-process instead of going to the
// research agents. ok is false when no open item has that number.
func (s *Server) HiveItemBrief(ctx context.Context, number int) (string, bool) {
	if ctx.Err() != nil || number <= 0 {
		return "", false
	}
	status := s.chatStatusSnapshot()
	if status == nil {
		return "", false
	}
	now := time.Now()
	for _, repo := range status.Repos {
		for _, it := range repoOpenItems(repo) {
			if it.Number != number {
				continue
			}
			body := it.Issue.Body
			if strings.TrimSpace(body) == "" {
				body = s.hiveItemCachedBody(overviewRepoName(repo), it.Kind, number)
			}
			return hiveItemBrief(overviewRepoName(repo), it, body, status.HiveID, now), true
		}
	}
	return "", false
}

// hiveItemCachedBody is the item's description from the forge cache's
// in-memory open-items snapshot (the same one the open counts are read
// from). Empty when the cache is off or does not hold the item.
func (s *Server) hiveItemCachedBody(repo, kind string, number int) string {
	if s == nil || s.deps == nil || s.deps.MetricsCollector == nil {
		return ""
	}
	fc := s.deps.MetricsCollector.ForgeCounts()
	if fc == nil {
		return ""
	}
	snap, ok := fc.OpenItems(repo)
	if !ok {
		return ""
	}
	if kind == hiveItemKindPR {
		return snap.PRs[number].Body
	}
	return snap.Issues[number].Body
}

// hiveItemBrief words one item: what it is, what it is about, its state,
// review and CI, and what is blocking it.
func hiveItemBrief(repo string, it openItemRef, body, hiveID string, now time.Time) string {
	var b strings.Builder
	var labels []string
	var blocking []string
	if it.Kind == hiveItemKindPR {
		pr, verdict := it.PR.pr, it.PR.verdict
		if it.Sidelined {
			w := it.WorkPR
			pr = github.PullRequest{Number: w.Number, Title: w.Title, Author: w.Author, Labels: w.Labels, Draft: w.Draft, CreatedAt: w.CreatedAt, BaseRef: w.BaseRef}
		}
		labels = pr.Labels
		fmt.Fprintf(&b, "PR %s#%d %q by %s.", repo, it.Number, chatTruncate(pr.Title, hiveStatusTextCap), pr.Author)
		hiveItemAbout(&b, body)
		b.WriteString(" State: " + hiveItemPRState(pr))
		if !pr.CreatedAt.IsZero() && now.After(pr.CreatedAt) {
			b.WriteString(", opened " + hiveStatusAge(now.Sub(pr.CreatedAt)) + " ago")
		}
		b.WriteString(". Review: " + hiveItemReview(pr) + ". CI: " + hiveItemCI(pr) + ".")
		blocking = hiveItemPRBlocking(pr, verdict)
	} else {
		title, author, created := it.Issue.Title, it.Issue.Author, it.Issue.CreatedAt
		labels = it.Issue.Labels
		if it.Sidelined {
			title, author, created, labels = it.WorkIssue.Title, it.WorkIssue.Author, it.WorkIssue.CreatedAt, it.WorkIssue.Labels
		}
		fmt.Fprintf(&b, "Issue %s#%d %q", repo, it.Number, chatTruncate(title, hiveStatusTextCap))
		if author != "" {
			b.WriteString(" by " + author)
		}
		b.WriteString(".")
		hiveItemAbout(&b, body)
		b.WriteString(" State: open")
		if !created.IsZero() && now.After(created) {
			b.WriteString(", opened " + hiveStatusAge(now.Sub(created)) + " ago")
		}
		if len(it.Issue.Assignees) > 0 {
			b.WriteString(", assigned to " + strings.Join(it.Issue.Assignees, ", "))
		}
		b.WriteString(".")
	}
	if it.Held {
		hold := "on hold"
		if held := holdLabels(labels, hiveID); len(held) > 0 {
			hold += " (label " + strings.Join(held, ", ") + ")"
		}
		blocking = append([]string{hold}, blocking...)
	}
	if it.Sidelined && it.Reason != "" {
		blocking = append(blocking, chatTruncate(it.Reason, hiveItemReasonCap))
	}
	if len(blocking) == 0 {
		blocking = []string{hiveItemNothingBlocked}
	}
	b.WriteString(" Blocking it: " + strings.Join(blocking, "; ") + ".")
	if stage := chatStageLabels(labels); len(stage) > 0 {
		b.WriteString(" Stage: " + strings.Join(stage, ", ") + ".")
	}
	if other := chatOtherLabels(labels); len(other) > 0 {
		more := len(other) - hiveItemMaxLabels
		if more > 0 {
			other = other[:hiveItemMaxLabels]
		}
		b.WriteString(" Labels: " + strings.Join(other, ", "))
		if more > 0 {
			fmt.Fprintf(&b, " and %d more", more)
		}
		b.WriteString(".")
	}
	return chatTruncate(b.String(), hiveItemMaxChars) + hiveItemAnswerGuide
}

// hiveItemAbout appends the summary of the description, or says the hive
// holds none, so "I have no description" is only said when it is true.
func hiveItemAbout(b *strings.Builder, body string) {
	if sum := hiveItemSummary(body); sum != "" {
		b.WriteString(" About: " + sum)
		return
	}
	b.WriteString(" About: the hive holds no description for it, only the title.")
}

// hiveItemPRState is open, draft, merged or closed.
func hiveItemPRState(pr github.PullRequest) string {
	switch {
	case !pr.MergedAt.IsZero():
		return "merged"
	case !pr.ClosedAt.IsZero() || strings.EqualFold(pr.State, hiveItemStateClosed):
		return "closed without merging"
	case pr.Draft:
		return "open, still a draft"
	}
	return "open"
}

// hiveItemReview words the review status the hive holds: the recorded
// review state, the lgtm and approved labels, and who a review is
// requested from.
func hiveItemReview(pr github.PullRequest) string {
	var parts []string
	if pr.ReviewState != "" {
		parts = append(parts, strings.ReplaceAll(strings.ToLower(pr.ReviewState), "_", " "))
	}
	for _, want := range []string{hiveItemLabelLGTM, hiveItemLabelApproved} {
		for _, l := range pr.Labels {
			if strings.EqualFold(l, want) {
				parts = append(parts, "has the "+want+" label")
			}
		}
	}
	if len(pr.RequestedReviewers) > 0 {
		parts = append(parts, "waiting on a review from "+strings.Join(pr.RequestedReviewers, ", "))
	}
	if len(parts) == 0 {
		return "no review recorded yet"
	}
	return strings.Join(parts, ", ")
}

// hiveItemCI words the check rollup the hive holds.
func hiveItemCI(pr github.PullRequest) string {
	ci := pr.CIStatus
	if ci == "" && pr.CIChecksRunning {
		ci = hiveItemCIRunning
	}
	if ci == "" {
		return "no result recorded yet"
	}
	if failing := pr.FailingChecks; len(failing) > 0 {
		more := len(failing) - hiveItemMaxFailing
		if more > 0 {
			failing = failing[:hiveItemMaxFailing]
		}
		ci += ", failing " + strings.Join(failing, ", ")
		if more > 0 {
			ci += fmt.Sprintf(" and %d more", more)
		}
	}
	if pr.CIStatus != "" && pr.CIChecksRunning {
		ci += ", some checks still running"
	}
	return ci
}

// hiveItemPRBlocking lists what stands between the PR and a merge, from
// the fields the hive already holds.
func hiveItemPRBlocking(pr github.PullRequest, verdict *github.MergeVerdict) []string {
	var out []string
	if pr.Draft {
		out = append(out, "it is still a draft")
	}
	if pr.CIStatus == hiveItemCIFailure {
		out = append(out, "CI is failing")
	}
	base := pr.BaseRef
	if base == "" {
		base = "the base branch"
	}
	switch strings.ToLower(pr.MergeableState) {
	case hiveItemMergeDirty:
		out = append(out, "merge conflicts with "+base)
	case hiveItemMergeBehind:
		out = append(out, "behind "+base)
	case hiveItemMergeBlocked:
		out = append(out, "GitHub reports the merge blocked by required checks or reviews")
	}
	if strings.EqualFold(pr.ReviewState, hiveItemReviewChanges) {
		out = append(out, "a reviewer requested changes")
	}
	if verdict != nil {
		switch verdict.State {
		case github.MergeVerdictBlocked, github.MergeVerdictOutstanding:
			if verdict.Reason != "" {
				out = append(out, chatTruncate(verdict.Reason, hiveItemReasonCap))
			}
		case github.MergeVerdictEligible:
			if len(out) == 0 {
				out = append(out, "nothing, it is eligible to merge")
			}
		}
	}
	return out
}

var (
	hiveItemComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
	hiveItemFence     = regexp.MustCompile("(?s)```.*?```")
	hiveItemImage     = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	hiveItemLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	hiveItemURL       = regexp.MustCompile(`https?://\S+`)
	hiveItemHTMLTag   = regexp.MustCompile(`<[^>]+>`)
	hiveItemListMark  = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	hiveItemCheckbox  = regexp.MustCompile(`^(?:[-*+]\s+)?\[[ xX]\]`)
	hiveItemTrailer   = regexp.MustCompile(`(?i)^\W*(?:(?:signed-off-by|co-authored-by|generated with)\b|(?:fix(?:es|ed)?|close[sd]?|resolve[sd]?|refs?|part of)\s*:?\s+[\w./-]*#\d+)`)
	hiveItemSentence  = regexp.MustCompile(`[.!?](?:\s|$)`)
	hiveItemEmphasis  = strings.NewReplacer("**", "", "__", "", "`", "", "*", "")
	hiveItemBlockMark = []string{"#", "|", ">", "---", "==="}
)

// hiveItemSummary is the first meaningful sentences of an issue or PR
// body: Markdown structure (headings, tables, checklists, code, images,
// comments, commit trailers) is dropped and what is left is cut to
// hiveItemSummaryMaxSentences and hiveItemSummaryMaxChars.
func hiveItemSummary(body string) string {
	body = hiveItemComment.ReplaceAllString(body, " ")
	body = hiveItemFence.ReplaceAllString(body, " ")
	var kept []string
lines:
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || hiveItemCheckbox.MatchString(line) {
			continue
		}
		for _, mark := range hiveItemBlockMark {
			if strings.HasPrefix(line, mark) {
				continue lines
			}
		}
		line = hiveItemListMark.ReplaceAllString(line, "")
		line = hiveItemImage.ReplaceAllString(line, "")
		line = hiveItemLink.ReplaceAllString(line, "$1")
		line = hiveItemURL.ReplaceAllString(line, "")
		line = hiveItemHTMLTag.ReplaceAllString(line, "")
		line = strings.TrimSpace(hiveItemEmphasis.Replace(line))
		if line == "" || hiveItemTrailer.MatchString(line) {
			continue
		}
		if last := line[len(line)-1]; last != '.' && last != '!' && last != '?' {
			line = strings.TrimRight(line, ":;,") + "."
		}
		kept = append(kept, line)
	}
	text := strings.Join(strings.Fields(strings.Join(kept, " ")), " ")
	end := 0
	for n := 0; n < hiveItemSummaryMaxSentences; n++ {
		loc := hiveItemSentence.FindStringIndex(text[end:])
		if loc == nil {
			end = len(text)
			break
		}
		end += loc[0] + 1
	}
	text = strings.TrimSpace(text[:end])
	if r := []rune(text); len(r) > hiveItemSummaryMaxChars {
		text = strings.TrimSpace(string(r[:hiveItemSummaryMaxChars-1])) + "…"
	}
	return text
}

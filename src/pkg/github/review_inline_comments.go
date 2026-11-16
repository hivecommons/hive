package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Inline review comments (hivecommons/hive#18531).
//
// A reviewer used to be able to publish only one review body, so every
// finding landed in a single long comment a maintainer could not ack or
// resolve one at a time. A ReviewRequest may now carry Comments: findings
// anchored to (path, line, side) of the PR diff. The relay submits them
// together with the summary body and the verdict event in ONE create-review
// call, so GitHub opens one resolvable thread per finding and sends a single
// notification for the whole review. Comments are never posted one by one.
//
// A comment the diff cannot anchor (the line is not in a hunk, the file is
// not in the PR, the patch is binary or too large) is folded into the
// summary body instead of failing the review. When GitHub still rejects the
// anchors with a 422, the review is resubmitted once with every comment
// folded into the body.

// ReviewInlineComment is one line-anchored finding in a ReviewRequest. Line
// (and StartLine for a range) are file line numbers on Side: "RIGHT" (the
// PR head, the default) or "LEFT" (the base). Body is the finding text.
type ReviewInlineComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

const (
	// maxReviewInlineComments bounds one review. A reviewer with more
	// line-level findings than this is better served by a summary, and the
	// cap keeps one request from opening an unbounded number of threads.
	maxReviewInlineComments = 50
	// maxReviewInlineCommentBytes mirrors GitHub's per-comment body limit.
	maxReviewInlineCommentBytes = 65536
	// reviewInlineFilesMaxPages bounds the diff fetch used for anchoring
	// (100 files per page). A larger PR is treated as unknown: comments are
	// sent as-is and the 422 fallback catches a bad anchor.
	reviewInlineFilesMaxPages = 30

	reviewSideRight = "RIGHT"
	reviewSideLeft  = "LEFT"

	// reviewInlineFoldHeading introduces findings that could not be posted
	// on a line.
	reviewInlineFoldHeading = "**Findings not on a changed line**"
)

// normalizedSide returns RIGHT or LEFT, or "" for an invalid side.
func (rc ReviewInlineComment) normalizedSide() string {
	switch strings.ToUpper(strings.TrimSpace(rc.Side)) {
	case "", reviewSideRight:
		return reviewSideRight
	case reviewSideLeft:
		return reviewSideLeft
	}
	return ""
}

// inlineCommentsShapeError returns why comments cannot be relayed, or "".
func inlineCommentsShapeError(comments []ReviewInlineComment) string {
	if len(comments) > maxReviewInlineComments {
		return fmt.Sprintf("review request carries %d inline comments; at most %d are allowed, put the rest in the summary body", len(comments), maxReviewInlineComments)
	}
	for i, rc := range comments {
		switch {
		case strings.TrimSpace(rc.Path) == "":
			return fmt.Sprintf("inline comment %d requires path", i)
		case strings.HasPrefix(strings.TrimSpace(rc.Path), "/"):
			return fmt.Sprintf("inline comment %d path must be repository-relative, not %q", i, rc.Path)
		case rc.Line <= 0:
			return fmt.Sprintf("inline comment %d requires a positive line", i)
		case rc.StartLine < 0 || (rc.StartLine > 0 && rc.StartLine >= rc.Line):
			return fmt.Sprintf("inline comment %d start_line must be below line", i)
		case rc.normalizedSide() == "":
			return fmt.Sprintf("inline comment %d side must be RIGHT or LEFT", i)
		case strings.TrimSpace(rc.Body) == "":
			return fmt.Sprintf("inline comment %d requires body", i)
		case len(rc.Body) > maxReviewInlineCommentBytes:
			return fmt.Sprintf("inline comment %d body exceeds %d bytes", i, maxReviewInlineCommentBytes)
		}
		if reason := redactedQuoteRefusal(rc.Body); reason != "" {
			return fmt.Sprintf("inline comment %d: %s", i, reason)
		}
	}
	return ""
}

// inlineCommentsText joins comment bodies for content scans (canary).
func inlineCommentsText(comments []ReviewInlineComment) string {
	var b strings.Builder
	for _, rc := range comments {
		b.WriteString(rc.Body)
		b.WriteString("\n")
	}
	return b.String()
}

// diffAnchors is the set of commentable lines per path and side, grouped by
// hunk so a range (start_line..line) can be required to sit in one hunk.
type diffAnchors map[string]map[string]map[int]int // path -> side -> line -> hunk index

// parsePatchAnchors records every line a review comment can attach to in a
// unified-diff patch: added and context lines on RIGHT, deleted and context
// lines on LEFT.
func parsePatchAnchors(patch string) map[string]map[int]int {
	sides := map[string]map[int]int{reviewSideRight: {}, reviewSideLeft: {}}
	hunk := -1
	oldLine, newLine := 0, 0
	for _, raw := range strings.Split(patch, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, "@@") {
			o, n, ok := parseHunkHeader(line)
			if !ok {
				hunk = -1
				continue
			}
			hunk++
			oldLine, newLine = o, n
			continue
		}
		if hunk < 0 || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			sides[reviewSideRight][newLine] = hunk
			newLine++
		case '-':
			sides[reviewSideLeft][oldLine] = hunk
			oldLine++
		case ' ':
			sides[reviewSideRight][newLine] = hunk
			sides[reviewSideLeft][oldLine] = hunk
			newLine++
			oldLine++
		}
		// '\' ("\ No newline at end of file") carries no line.
	}
	return sides
}

// parseHunkHeader reads "@@ -a,b +c,d @@" and returns the first old and new
// line numbers.
func parseHunkHeader(h string) (oldStart, newStart int, ok bool) {
	fields := strings.Fields(h)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return 0, 0, false
	}
	start := func(s string) (int, bool) {
		s = s[1:]
		if i := strings.IndexByte(s, ','); i >= 0 {
			s = s[:i]
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	o, ok1 := start(fields[1])
	n, ok2 := start(fields[2])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return o, n, true
}

// anchorable reports whether rc can be posted on the diff.
func (a diffAnchors) anchorable(rc ReviewInlineComment) bool {
	lines := a[strings.TrimSpace(rc.Path)][rc.normalizedSide()]
	end, ok := lines[rc.Line]
	if !ok {
		return false
	}
	if rc.StartLine > 0 {
		start, ok := lines[rc.StartLine]
		if !ok || start != end {
			return false
		}
	}
	return true
}

// fetchDiffAnchors reads the PR's changed files. complete=false means the
// list could not be read in full, so absence from it proves nothing.
func (c *Client) fetchDiffAnchors(ctx context.Context, owner, repoName string, number int) (diffAnchors, bool) {
	if c == nil || c.client == nil {
		return nil, false
	}
	ctx = WithRESTCaller(ctx, "hive:review_inline_anchor")
	anchors := diffAnchors{}
	opts := &gh.ListOptions{PerPage: listPerPage}
	for page := 0; page < reviewInlineFilesMaxPages; page++ {
		files, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repoName, number, opts)
		if err != nil {
			return nil, false
		}
		for _, f := range files {
			if f == nil || f.GetFilename() == "" {
				continue
			}
			anchors[f.GetFilename()] = parsePatchAnchors(f.GetPatch())
		}
		if resp == nil || resp.NextPage == 0 {
			return anchors, true
		}
		opts.Page = resp.NextPage
	}
	return nil, false
}

// partitionInlineComments splits comments into those to post on lines and
// those to fold into the summary. With an unknown diff every comment is
// kept; the 422 fallback covers a bad anchor.
func (c *Client) partitionInlineComments(ctx context.Context, owner, repoName string, number int, comments []ReviewInlineComment) (anchored, folded []ReviewInlineComment) {
	if len(comments) == 0 {
		return nil, nil
	}
	anchors, complete := c.fetchDiffAnchors(ctx, owner, repoName, number)
	if !complete {
		return comments, nil
	}
	for _, rc := range comments {
		if anchors.anchorable(rc) {
			anchored = append(anchored, rc)
		} else {
			folded = append(folded, rc)
		}
	}
	return anchored, folded
}

// foldInlineComments appends comments to a summary body as a list, each
// naming its location, so a finding that cannot sit on a line is not lost.
func foldInlineComments(body string, comments []ReviewInlineComment) string {
	if len(comments) == 0 {
		return body
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, "\n"))
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(reviewInlineFoldHeading)
	b.WriteString("\n")
	for _, rc := range comments {
		b.WriteString("\n- `")
		b.WriteString(strings.TrimSpace(rc.Path))
		b.WriteString(":")
		if rc.StartLine > 0 {
			b.WriteString(strconv.Itoa(rc.StartLine))
			b.WriteString("-")
		}
		b.WriteString(strconv.Itoa(rc.Line))
		if rc.normalizedSide() == reviewSideLeft {
			b.WriteString(" (base)")
		}
		b.WriteString("`: ")
		b.WriteString(strings.ReplaceAll(strings.TrimSpace(rc.Body), "\n", "\n  "))
	}
	return b.String()
}

// draftReviewComments converts comments to the create-review payload.
// relay, when set, is applied to each body exactly as it is to the review
// body (mention neutralizing for agents that may not ping people).
func draftReviewComments(comments []ReviewInlineComment, relay func(string) string) []*gh.DraftReviewComment {
	if len(comments) == 0 {
		return nil
	}
	out := make([]*gh.DraftReviewComment, 0, len(comments))
	for _, rc := range comments {
		side := rc.normalizedSide()
		text := rc.Body
		if relay != nil {
			text = relay(text)
		}
		d := &gh.DraftReviewComment{
			Path: gh.Ptr(strings.TrimSpace(rc.Path)),
			Body: gh.Ptr(text),
			Line: gh.Ptr(rc.Line),
			Side: gh.Ptr(side),
		}
		if rc.StartLine > 0 {
			d.StartLine = gh.Ptr(rc.StartLine)
			d.StartSide = gh.Ptr(side)
		}
		out = append(out, d)
	}
	return out
}

// isInlineAnchorRefusal reports a 422 from create-review that is not the
// own-PR refusal: with comments attached, that is GitHub refusing a line
// that is not part of the diff.
func isInlineAnchorRefusal(err error) bool {
	return err != nil && githubStatusError(err, 422) && !IsOwnPullRequestReviewRefusal(err)
}

// logInlineFold records why comments went into the summary.
func (c *Client) logInlineFold(req ReviewRequest, n int, reason string) {
	if n == 0 || c == nil || c.logger == nil {
		return
	}
	c.logger.Info("review-request watcher: inline comments folded into the review summary",
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.Int("folded", n), slog.String("reason", reason))
}

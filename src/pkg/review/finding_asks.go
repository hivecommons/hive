package review

import (
	"fmt"
	"strings"
)

// findingAskMaxRunes bounds one rendered finding line, so a long reviewer
// title cannot turn the contributor nudge into a wall of text.
const findingAskMaxRunes = 200

// FindingAsks renders the aggregate's in-scope findings as one plain line
// each ("`path:line` title"), at most limit of them (limit <= 0 means all).
// When findings are left out, a last line says how many more there are. The
// stage lanes hand these to the contributor nudge, so a PR returned to its
// author on a changes_requested lists what the review asked for instead of
// only saying that it asked.
func (a Aggregate) FindingAsks(limit int) []string {
	var lines []string
	for _, f := range a.Findings {
		if !InScopeFinding(f.Finding) {
			continue
		}
		title := findingAskOneLine(f.Finding.Title)
		if title == "" {
			title = findingAskOneLine(f.Finding.Summary)
		}
		loc := strings.ReplaceAll(findingAskOneLine(f.Finding.File), "`", "")
		if loc != "" && f.Finding.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, f.Finding.Line)
		}
		switch {
		case loc != "" && title != "":
			lines = append(lines, "`"+loc+"` "+title)
		case loc != "":
			lines = append(lines, "`"+loc+"`")
		case title != "":
			lines = append(lines, title)
		}
	}
	if limit > 0 && len(lines) > limit {
		more := len(lines) - limit
		lines = append(lines[:limit:limit], fmt.Sprintf("and %d more finding(s) in the review", more))
	}
	return lines
}

func findingAskOneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > findingAskMaxRunes {
		s = string(r[:findingAskMaxRunes-1]) + "…"
	}
	return s
}

// blockSummaryReason is one blocking perspective's summary as the aggregate
// keeps it (Aggregate.BlockSummaries), or "" when the report has none.
func blockSummaryReason(r PerspectiveReport) string {
	summary := strings.TrimSpace(r.Summary)
	if summary == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", r.Perspective, summary)
}

// BlockSummaryAsks renders what the blocking reviewers said in their
// summaries, one plain line each, at most limit of them (limit <= 0 means
// all), trimmed like a finding line. The changes_requested / reject
// perspectives come first (BlockSummaries), then the perspectives that
// asked for a person (HumanSummaries): those block too when the aggregate
// routes as changes_requested. The contributor nudge lists these lines
// only when the verdict blocks and has no in-scope finding to list, so a
// PR returned to its author never says only that the review asked for
// something.
func (a Aggregate) BlockSummaryAsks(limit int) []string {
	var lines []string
	seen := map[string]bool{}
	for _, list := range [][]string{a.BlockSummaries, a.HumanSummaries} {
		for _, s := range list {
			if s = findingAskOneLine(s); s != "" && !seen[s] {
				seen[s] = true
				lines = append(lines, s)
			}
		}
	}
	if limit > 0 && len(lines) > limit {
		more := len(lines) - limit
		lines = append(lines[:limit:limit], fmt.Sprintf("and %d more reviewer summary line(s) in the review", more))
	}
	return lines
}

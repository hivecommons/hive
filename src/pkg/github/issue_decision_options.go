package github

import (
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// IssueDecisionOptions reads the options a parked issue asks its owner to
// choose between, for "Let hive decide" (projectbluefin/utah#920): the
// lettered options the "What to reply" notice offers (the issue body, or the
// newest hive comment that lists them), else a numbered list a cue line
// introduces as alternatives ("Pick one:", "three competing delivery
// mechanisms:"). recommended is the label the same text recommends, "" when
// it names none. Fewer than two options reads as none.
func (c *Client) IssueDecisionOptions(issueBody string, issueAuthor *gh.User, comments []*gh.IssueComment) (options []delegatedecision.Option, recommended string) {
	m := c.unparkMenuFor(issueBody, issueAuthor, comments)
	if len(m.options) >= 2 {
		for _, l := range m.options {
			options = append(options, delegatedecision.Option{Label: l, Summary: m.optionText[l]})
		}
		return options, m.recommended
	}
	// No lettered options: the newest park text with a numbered option list
	// wins, as for letters. The hive's own notices, replies and the
	// delegate's verdicts are not park text; a withdrawn ask takes its
	// options with it.
	source := ""
	if numberedDecisionOptions(issueBody) != nil {
		source = issueBody
	}
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		body := cm.GetBody()
		switch {
		case strings.Contains(body, parkWithdrawnMarkerPrefix):
			source = ""
		case strings.Contains(body, unparkNoticeMarker), strings.Contains(body, unparkReplyMarkerPrefix),
			strings.Contains(body, unparkAcceptedMarker), strings.Contains(body, parkRecheckMarker),
			delegatedecision.IsDelegatedComment(body):
		case numberedDecisionOptions(body) != nil:
			source = body
		}
	}
	if source == "" {
		return nil, ""
	}
	options = numberedDecisionOptions(source)
	return options, numberedRecommendedOption(source, options)
}

// numberedOptionItemRE is a numbered list item, "1. text" or "2) text",
// indented at most three spaces as Markdown allows.
var numberedOptionItemRE = regexp.MustCompile(`^ {0,3}([1-9])[.)][ \t]+(\S.*)$`)

// numberedOptionCueRE is a lead-in that makes the numbered list after it a
// set of alternatives rather than steps.
var numberedOptionCueRE = regexp.MustCompile(`(?i)\b(?:pick|choose|select)\s+(?:one|between|from|among)\b|\bone of\b|\b(?:options?|alternatives?|choices?)\b|\bcompeting\b|\bmutually exclusive\b|\beither\b`)

// numberedDecisionOptions reads the first numbered list in text that a cue
// line introduces as alternatives: the nearest non-blank line before item 1
// must read as one (numberedOptionCueRE). Items must run 1, 2, 3 … with only
// blank or indented continuation lines between them; a numbered list of
// steps ("What it cost: 1. eu-stack … 2. gdb …") has no such lead-in and is
// not read. Fenced code is skipped. Fewer than two items reads as none.
func numberedDecisionOptions(text string) []delegatedecision.Option {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	lead, fenced := "", false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		m := numberedOptionItemRE.FindStringSubmatch(line)
		if m == nil || m[1] != "1" || !numberedOptionCueRE.MatchString(lead) {
			if strings.TrimSpace(line) != "" {
				lead = line
			}
			continue
		}
		var opts []delegatedecision.Option
		j := i
		for ; j < len(lines); j++ {
			l := lines[j]
			if im := numberedOptionItemRE.FindStringSubmatch(l); im != nil {
				if im[1] != string(rune('1'+len(opts))) {
					break
				}
				opts = append(opts, delegatedecision.Option{Label: im[1], Summary: unparkCleanOptionSummary(strings.ReplaceAll(im[2], "**", ""))})
				continue
			}
			if strings.TrimSpace(l) == "" || strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") {
				continue
			}
			break
		}
		if len(opts) >= 2 {
			return opts
		}
		lead, i = line, j-1
	}
	return nil
}

// numberedOptionRefRE names a numbered option on a recommendation line:
// "option 2", "choice #3", or a bare number right after the word
// ("Recommendation: 2.", "I recommend **1**").
var numberedOptionRefRE = regexp.MustCompile(`(?i)\b(?:option|choice|alternative)\s*#?\s*([1-9])\b|\brecommend\w*(?:\s+is)?\s*[:—–-]?\s*\**\s*([1-9])\**(?:[.):,;]|\s|$)`)

// numberedRecommendedOption is the numbered option text recommends, read
// only from lines that mention a recommendation and only as an explicit
// option reference, so a stray number ("saves 3 hours") is never taken for
// a choice. "" when none of options is named.
func numberedRecommendedOption(text string, options []delegatedecision.Option) string {
	offered := map[string]bool{}
	for _, o := range options {
		offered[o.Label] = true
	}
	for _, line := range strings.Split(text, "\n") {
		if !unparkMentionsRecommendation(line) {
			continue
		}
		for _, m := range numberedOptionRefRE.FindAllStringSubmatch(line, -1) {
			for _, n := range m[1:] {
				if offered[n] {
					return n
				}
			}
		}
	}
	return ""
}

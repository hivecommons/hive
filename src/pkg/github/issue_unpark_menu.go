package github

import (
	"fmt"
	"strings"
	"unicode"

	gh "github.com/google/go-github/v72/github"
)

// unparkOwnerStepMarker tags a comment the hive posted with
// `hive-open-issue comment --needs-decision`: the remaining step is one only
// the owner can do (hivecommons/hive#12334). The "What to reply" notice offers
// `/hive done` while the newest such comment is unanswered.
const unparkOwnerStepMarker = "<!-- hive:owner-step -->"

// unparkDoneDecision is the recorded decision for `/hive done`.
const unparkDoneDecision = "the owner-only step is done; Hive re-checks it and closes the issue once the fix is verified"

// unparkMenu is what the "What to reply" notice and `/hive help` offer on one
// parked issue.
type unparkMenu struct {
	// options are the lettered options the issue offers, in order.
	options []string
	// recommended is the option the text recommends, or "" when none is named.
	recommended string
	// hasRecommendation is true when the issue states a recommendation.
	hasRecommendation bool
	// ownerStep is true when the issue is parked on a step only the owner can
	// do, so `/hive done` is offered.
	ownerStep bool
}

// unparkMenuFromBody reads the menu from the issue body alone.
func unparkMenuFromBody(issueBody string) unparkMenu {
	m := unparkMenu{options: unparkOptionLabels(issueBody)}
	m.hasRecommendation = unparkMentionsRecommendation(issueBody)
	if m.hasRecommendation {
		m.recommended = unparkRecommendedOption(issueBody, m.options)
	}
	return m
}

// unparkMenuFor reads the menu from the issue body and from the hive's own
// comments. The newest hive comment that lists options wins over the body,
// because agents often post the options in a comment after filing.
func (c *Client) unparkMenuFor(issueBody string, comments []*gh.IssueComment) unparkMenu {
	m := unparkMenuFromBody(issueBody)
	source := issueBody
	var ownerStepBody string
	ownerStep := false
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		body := cm.GetBody()
		if strings.Contains(body, unparkReplyMarkerPrefix) {
			// A /hive command was answered after any earlier owner step.
			ownerStep = false
			continue
		}
		if strings.Contains(body, unparkNoticeMarker) {
			continue
		}
		if strings.Contains(body, unparkOwnerStepMarker) {
			ownerStep, ownerStepBody = true, body
		}
		if opts := unparkOptionLabels(body); len(opts) > 0 {
			m.options, source = opts, body
		}
		if unparkMentionsRecommendation(body) {
			m.hasRecommendation = true
		}
	}
	m.recommended = ""
	if unparkMentionsRecommendation(source) {
		m.recommended = unparkRecommendedOption(source, m.options)
	}
	// A comment parked with --needs-decision that lists options is a choice,
	// not a manual step.
	m.ownerStep = ownerStep && len(unparkOptionLabels(ownerStepBody)) == 0
	return m
}

// bullets lists the commands for this menu.
func (m unparkMenu) bullets() string {
	var b strings.Builder
	if m.ownerStep {
		fmt.Fprintln(&b, "- `/hive done`: you did the step only you can do; Hive re-checks it and closes the issue once it is verified")
	}
	switch {
	case len(m.options) > 0:
		first := m.recommended
		if first == "" {
			first = m.options[0]
		}
		if m.recommended != "" || m.hasRecommendation {
			fmt.Fprintf(&b, "- `/hive approve`: go with the recommendation (option %s)\n", first)
		} else {
			fmt.Fprintf(&b, "- `/hive approve`: go with option %s\n", first)
		}
		for _, opt := range m.options {
			if opt == first {
				continue
			}
			fmt.Fprintf(&b, "- `/hive decision %s`: go with option %s\n", opt, opt)
		}
	case m.hasRecommendation:
		fmt.Fprintln(&b, "- `/hive approve`: go with the recommendation")
		fmt.Fprintln(&b, "- `/hive decision <your instructions>`: go ahead, following your instructions")
	default:
		fmt.Fprintln(&b, "- `/hive decision <your instructions>`: go ahead, following your instructions")
	}
	fmt.Fprintln(&b, "- `/hive help`: show these again")
	return b.String()
}

// unparkCommandDecisionText is the decision recorded for an accepted command.
func unparkCommandDecisionText(command unparkCommand) string {
	if command.Kind == unparkCommandDone {
		return unparkDoneDecision
	}
	if decision := strings.TrimSpace(command.Decision); decision != "" {
		return decision
	}
	return "go with the recommendation"
}

// unparkMentionsRecommendation reports whether text states a recommendation.
func unparkMentionsRecommendation(text string) bool {
	return strings.Contains(strings.ToLower(text), "recommend")
}

// unparkRecommendedOption returns the option letter named on the first line
// of text that mentions a recommendation, e.g. "Recommendation: option A." or
// "My recommendation is **B**". Only letters among options count.
func unparkRecommendedOption(text string, options []string) string {
	if len(options) == 0 {
		return ""
	}
	offered := map[string]bool{}
	for _, opt := range options {
		offered[opt] = true
	}
	for _, line := range strings.Split(text, "\n") {
		if !unparkMentionsRecommendation(line) {
			continue
		}
		for _, tok := range strings.FieldsFunc(line, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if offered[tok] {
				return tok
			}
		}
	}
	return ""
}

// unparkBoldOptionLetter reads a bold lettered option such as "**A.** text",
// "**B. Keep latest**", "- **C)** text" or "**D** — text".
func unparkBoldOptionLetter(line string) string {
	s := strings.TrimLeft(strings.TrimSpace(line), "-+ \t")
	if !strings.HasPrefix(s, "**") || len(s) < 4 {
		return ""
	}
	s = s[2:]
	letter := s[:1]
	if letter < "A" || letter > "F" {
		return ""
	}
	rest := s[1:]
	if strings.ContainsAny(rest[:1], ".):") {
		return letter
	}
	if strings.HasPrefix(rest, "**") {
		after := strings.TrimSpace(rest[2:])
		for _, sep := range []string{"—", "–", "-", ":", "."} {
			if strings.HasPrefix(after, sep) {
				return letter
			}
		}
	}
	return ""
}

// ownerStepCommentBody tags a comment that parks its issue with
// --needs-decision, so the "What to reply" notice can offer `/hive done`.
func ownerStepCommentBody(body string, needsDecision bool) string {
	if !needsDecision || strings.Contains(body, unparkOwnerStepMarker) {
		return body
	}
	return body + "\n\n" + unparkOwnerStepMarker
}

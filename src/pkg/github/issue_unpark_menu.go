package github

import (
	"fmt"
	"strings"
	"unicode"

	gh "github.com/google/go-github/v72/github"
)

// unparkOwnerStepMarker tags a body the hive posted with `hive-open-issue
// comment --needs-decision` or `hive-open-issue issue --needs-decision`: the
// remaining step is one only the owner can do (hivecommons/hive#12334). The
// "What to reply" notice offers `/hive done` while the newest such text is
// unanswered and lists no lettered options.
const unparkOwnerStepMarker = "<!-- hive:owner-step -->"

// unparkAcceptedMarker tags the hive's reply to an accepted un-park command
// (`/hive approve`, `/hive decision`, `/hive done`) or dashboard release.
// Only such a reply answers an owner step; `/hive help`, the prose hint and
// the ai_author refusal leave the step open.
const unparkAcceptedMarker = "<!-- hive:unpark-accepted -->"

// unparkDoneDecision is the recorded decision for `/hive done`. The reply
// comment that carries it is the whole hand-off: the lane that next picks up
// the issue reads it, re-checks the owner's step and closes the issue once
// the fix is verified. No label or queue state encodes the decision.
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
	// releaseStep is the typed release proposal. The repo policy decides
	// whether `/hive approve` may execute it; without that opt-in it remains
	// descriptive metadata and the legacy manual-step menu is unchanged.
	releaseStep *IssueOwnerStep
}

// unparkMenuFromBody reads the menu from the issue body alone. An issue
// filed with `hive-open-issue issue --needs-decision` carries the owner-step
// marker in its body; it is an owner step when it lists no options.
func unparkMenuFromBody(issueBody string) unparkMenu {
	m := unparkMenu{options: unparkOptionLabels(issueBody)}
	m.hasRecommendation = unparkMentionsRecommendation(issueBody)
	if m.hasRecommendation {
		m.recommended = unparkRecommendedOption(issueBody, m.options)
	}
	m.ownerStep = isUnparkOwnerStepText(issueBody)
	if m.ownerStep {
		m.releaseStep = parseIssueOwnerStep(issueBody)
	}
	return m
}

// isUnparkOwnerStepText reports whether text parks the issue on a step only
// the owner can do: it carries the marker and lists no lettered options (a
// comment parked with --needs-decision that lists options is a choice, not a
// manual step).
func isUnparkOwnerStepText(text string) bool {
	return hasOwnerStepMarker(text) && len(unparkOptionLabels(text)) == 0
}

// unparkMenuFor reads the menu from the issue body and from the hive's own
// comments. The body's owner-step marker counts only when the hive authored
// the issue. The newest hive comment that lists options wins over the body,
// because agents often post the options in a comment after filing, and the
// recommended option is read from the newest text that states a
// recommendation, so a body-level recommendation is never pinned onto
// options a later comment listed.
func (c *Client) unparkMenuFor(issueBody string, issueAuthor *gh.User, comments []*gh.IssueComment) unparkMenu {
	m := unparkMenuFromBody(issueBody)
	if !c.isHiveAccount(issueAuthor) {
		// Only a hive-authored body can park the issue on an owner step; the
		// author of any other issue could otherwise edit the marker in.
		m.ownerStep, m.releaseStep = false, nil
	}
	recommendationSource := ""
	if m.hasRecommendation {
		recommendationSource = issueBody
	}
	ownerStep := m.ownerStep
	for _, cm := range comments {
		if cm == nil || !c.isHiveAccount(cm.GetUser()) {
			continue
		}
		body := cm.GetBody()
		if strings.Contains(body, unparkAcceptedMarker) {
			// Accepted command replies and dashboard release replies both
			// answer the step. Only command replies carry a per-comment
			// unparkReplyMarkerPrefix.
			ownerStep = false
			m.releaseStep = nil
			continue
		}
		if strings.Contains(body, unparkReplyMarkerPrefix) {
			// `/hive help`, the prose hint and the ai_author refusal leave
			// the owner step open.
			continue
		}
		if strings.Contains(body, unparkNoticeMarker) {
			continue
		}
		if hasOwnerStepMarker(body) {
			ownerStep = isUnparkOwnerStepText(body)
			m.releaseStep = nil
			if ownerStep {
				m.releaseStep = parseIssueOwnerStep(body)
			}
		}
		if opts := unparkOptionLabels(body); len(opts) > 0 {
			m.options = opts
		}
		if unparkMentionsRecommendation(body) {
			m.hasRecommendation, recommendationSource = true, body
		}
	}
	m.recommended = unparkRecommendedOption(recommendationSource, m.options)
	m.ownerStep = ownerStep
	return m
}

// bullets lists the commands for this menu with release execution disabled,
// preserving the default-off behavior for callers without repository policy.
func (m unparkMenu) bullets() string {
	return m.bulletsFor(false)
}

// bulletsFor lists the commands after applying the repository's release policy.
func (m unparkMenu) bulletsFor(releaseOnApprove bool) string {
	var b strings.Builder
	if m.ownerStep {
		fmt.Fprintln(&b, "- `/hive done`: you did the step only you can do; Hive re-checks it and closes the issue once it is verified")
	}
	switch {
	case releaseOnApprove && m.releaseStep != nil:
		fmt.Fprintf(&b, "- `/hive approve`: Hive creates release `%s` from `%s`\n", m.releaseStep.Tag, m.releaseStep.Target)
		fmt.Fprintln(&b, "- `/hive decision <your instructions>`: go ahead, following your instructions")
	case len(m.options) > 0 && m.recommended != "":
		fmt.Fprintf(&b, "- `/hive approve`: go with the recommendation (option %s)\n", m.recommended)
		for _, opt := range m.options {
			if opt == m.recommended {
				continue
			}
			fmt.Fprintf(&b, "- `/hive decision %s`: go with option %s\n", opt, opt)
		}
	case len(m.options) > 0 && m.hasRecommendation:
		// A recommendation is stated but names none of the listed options,
		// so no option is attributed to it; every option stays an explicit
		// decision.
		fmt.Fprintln(&b, "- `/hive approve`: go with the recommendation")
		for _, opt := range m.options {
			fmt.Fprintf(&b, "- `/hive decision %s`: go with option %s\n", opt, opt)
		}
	case len(m.options) > 0:
		first := m.options[0]
		fmt.Fprintf(&b, "- `/hive approve`: go with option %s\n", first)
		for _, opt := range m.options[1:] {
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

// ownerStepCommentBody tags a body that parks its issue with
// --needs-decision (a new issue or a comment on an existing one), so the
// "What to reply" notice can offer `/hive done`.
func ownerStepCommentBody(body string, needsDecision bool) string {
	if !needsDecision || hasOwnerStepMarker(body) {
		return body
	}
	return body + "\n\n" + unparkOwnerStepMarker
}

package github

import (
	"regexp"
	"strings"
)

// Agent start signals (hivecommons/hive#10527).
//
// A governor kick lists many issues and the agent starts on few of them, so
// the hive no longer claims an issue for being listed. It claims it when the
// agent's own relay request first shows it working the issue: a comment,
// label or claim request on the issue (hive-open-issue), or a pull-request
// request naming it (hive-open-pr --issues). The relays below report those
// requests through AgentStartHook once the request is authorized, so the
// agent name is the verified requester, not text the agent wrote.

// Start signals reported through AgentStartHook.
const (
	AgentStartSignalComment   = "comment"
	AgentStartSignalLabel     = "label"
	AgentStartSignalClaim     = "claim"
	AgentStartSignalPRRequest = "pr_request"
)

// AgentStartHook is told that agent's own request shows it working issue
// number in repo. repo is spelled as the request spelled it (bare or
// owner/repo).
type AgentStartHook func(agent, repo string, number int, signal string)

// SetAgentStartHook installs (or with nil, removes) the start-signal hook.
// Safe to call before or after the watchers start.
func (c *Client) SetAgentStartHook(fn AgentStartHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.agentStartHook.Store(nil)
		return
	}
	c.agentStartHook.Store(&fn)
}

// notifyAgentStart reports a start signal to the installed hook, if any.
func (c *Client) notifyAgentStart(agent, repo string, number int, signal string) {
	if c == nil || agent == "" || repo == "" || number <= 0 {
		return
	}
	if hook := c.agentStartHook.Load(); hook != nil && *hook != nil {
		(*hook)(agent, repo, number, signal)
	}
}

// commentBlockerRefPattern is an issue or pull-request reference as an agent writes a
// blocker: "#N", "owner/repo#N", or a GitHub issue/pull URL.
const commentBlockerRefPattern = `(?:#\d+|[\w.-]+/[\w.-]+#\d+|https?://github\.com/[\w.-]+/[\w.-]+/(?:pull|issues)/\d+)`

// commentDeferral matches a comment that parks the issue instead of starting
// it (#12387): one that opens (after markdown and an optional "Status:" or
// "Update:" label) with "Deferred"/"Deferring", "Not starting",
// "[this is] [still] blocked by/on <ref>" or "waiting on/for <ref>"; or that
// says "deferred this cycle" or "pick(s) [this/it] up once/when …" anywhere,
// or "pick(s) [this/it] up after <ref> merges/lands/closes". A past "picking
// this up after #N merged" is a resume, not a deferral. The opening forms
// only look at how the comment starts, so a progress comment with a
// deferral aside later on still records the claim.
var commentDeferral = regexp.MustCompile(`(?i)` +
	`^\W*(?:(?:status|update)\s*:\W*)?(?:` +
	`deferr(?:ed|ing)\b` +
	`|(?:not|never|won't|can't|cannot)\s+start` +
	`|(?:(?:this|it)\s+is\s+)?(?:still\s+|currently\s+)?(?:blocked\s+(?:by|on)|waiting\s+(?:on|for))\s+` + commentBlockerRefPattern +
	`)` +
	`|\bdeferred\s+this\s+cycle\b` +
	`|\bpick(?:s|ing)?\s+(?:this\s+|it\s+)?up\s+(?:once|when)\b` +
	`|\bpick(?:s|ing)?\s+(?:this\s+|it\s+)?up\s+after\s+\S+(?:\s+\S+){0,3}?\s+(?:merges|lands|closes|ships|is\s+(?:merged|closed|fixed|resolved))\b`)

// commentStartNow matches an explicit statement that work is starting or
// resuming now. It overrides commentDeferral, so "Picking this up after
// #12322 merged, starting now." or "Deferred earlier; no longer blocked,
// working on it now." still record the claim that keeps a second agent off
// the issue (#10527). Group 1 captures a negation ("not starting now"),
// which does not count.
var commentStartNow = regexp.MustCompile(`(?i)\b((?:not|never|won't|can't|cannot)\s+)?(?:` +
	`(?:starting|started|resuming|resumed)\s+(?:work\s+|on\s+(?:this|it)\s+)?now` +
	`|(?:working\s+on|picking\s+up)\s+(?:this|it)\s+now` +
	`|picking\s+(?:this|it)\s+up\s+now` +
	`|no\s+longer\s+blocked|now\s+unblocked|unblocked\s+now` +
	`)\b`)

// commentIsDeferral reports whether a comment body is an explicit deferral
// with no statement that work is starting now.
func commentIsDeferral(body string) bool {
	if !commentDeferral.MatchString(body) {
		return false
	}
	for _, m := range commentStartNow.FindAllStringSubmatch(body, -1) {
		if m[1] == "" {
			return false
		}
	}
	return true
}

// commentEscalation matches a comment that parks the issue on a person
// instead of starting it: one that opens with an escalation verdict
// (`needs-decision (scanner): …`, `needs-direction …` — the explanation
// text that accompanies a stage/045 park, hivecommons/hive#12683 — or
// `Triage (scanner):
// not a … defect`) or with "parking"/"parked", or that says "parking this
// [issue/RFC] …", "out of the [agent] work queue" or "no PR to open"
// anywhere. The scanner posted exactly these on hivecommons/hive#12570 and
// #12481 and the hive answered with a 🔒 agent claim and
// stage/030-pr-in-progress: "PR in progress" on an RFC it had just declined
// to implement.
var commentEscalation = regexp.MustCompile(`(?i)` +
	`^\W*(?:(?:status|update|triage(?:\s*\([^)]*\))?)\s*:\W*)?(?:` +
	`needs-(?:decision|direction|spec|human|signal)\b` +
	`|park(?:ed|ing)\b` +
	`|not\s+a\s+(?:\S+\s+){0,2}?(?:code\s+)?(?:defect|bug)\b` +
	`)` +
	`|\bpark(?:s|ed|ing)?\s+(?:this|it)\b` +
	`|\bout\s+of\s+the\s+(?:agent\s+)?work\s+queue\b` +
	`|\bno\s+PR\s+to\s+open\b`)

// commentIsEscalation reports whether a comment body parks the issue on a
// person (see commentEscalation) with no statement that work is starting.
func commentIsEscalation(body string) bool {
	if !commentEscalation.MatchString(body) {
		return false
	}
	for _, m := range commentStartNow.FindAllStringSubmatch(body, -1) {
		if m[1] == "" {
			return false
		}
	}
	return true
}

// escalationLabels are the labels an agent puts on an issue to hand it to a
// person. A label request that adds nothing else is the agent stepping
// AWAY from the issue, so it must not record the claim that stages it
// "pr-in-progress" (hivecommons/hive#12481 carried needs-direction and
// stage/030 together for five hours).
var escalationLabels = map[string]bool{
	issueNeedsHumanLabel:                true,
	NeedsDecisionStageLabel:             true,
	legacyIssueNeedsDecisionStageLabel:  true,
	issueNeedsDirectionLabel:            true,
	issueNeedsDecisionLabel:             true,
	issueNeedsSpecLabel:                 true,
	NeedsSignalLabel:                    true,
	issueNeedsReporterConfirmationLabel: true,
}

// labelsOnlyEscalate reports whether every label in labels is an escalation
// label. An empty list (a remove-only request) is not a start either.
func labelsOnlyEscalate(labels []string) bool {
	for _, l := range labels {
		if !escalationLabels[strings.ToLower(strings.TrimSpace(l))] {
			return false
		}
	}
	return true
}

// issueRequestStartSignal maps a successful issue request to the start
// signal it is, or "" for requests that are not one: creating an issue,
// closing one, asking for a review, a deferral comment (#12387), an
// escalation comment or a label request that only parks the issue on a
// person do not show the agent working an existing issue. labels are the
// labels a "label" request adds. A claim request is a start signal
// whatever an accompanying comment said.
func issueRequestStartSignal(kind, body string, labels []string) string {
	switch kind {
	case "comment":
		if commentIsDeferral(body) || commentIsEscalation(body) {
			return ""
		}
		return AgentStartSignalComment
	case "label":
		if labelsOnlyEscalate(labels) {
			return ""
		}
		return AgentStartSignalLabel
	case "claim":
		return AgentStartSignalClaim
	}
	return ""
}

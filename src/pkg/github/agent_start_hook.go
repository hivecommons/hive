package github

import "regexp"

// Agent start signals (hivecommons/hive#10527).
//
// A governor kick lists many issues and the agent starts on few of them, so
// the hive no longer claims an issue for being listed. It claims it when the
// agent's own relay request first shows it STARTING WORK on the issue. A
// start signal is one of exactly these, reported through AgentStartHook once
// the request is authorized, so the agent name is the verified requester,
// not text the agent wrote:
//
//   - a `claim` request on the issue (hive-open-issue claim) — the explicit
//     "I am implementing this" the kick asks for;
//   - a pull-request request naming it (hive-open-pr --issues);
//   - a `comment` request whose body carries an explicit start statement:
//     the structured marker `<!-- hive:start -->` or a start phrase such as
//     "starting now" / "working on this now" / an opening "Starting …" /
//     "Implementing …" (commentIsStart), and no explicit deferral or
//     escalation that it does not override.
//
// Nothing else is. A comment that reports — a triage verdict, a
// verification result, a gate check, a status update, a deferral or an
// escalation — is posted and starts nothing, and a `label` request is never
// a start signal: the labels an agent may add through the relay are triage
// and verification verdicts (kind/*, area/*, hive/verified-open, the
// parking labels), none of which says work began. hivecommons/hive#13011
// was claimed and staged stage/030-pr-in-progress on the scanner's "Gate
// check … no implementation started" comment, and the epic #12999 on the
// scanner's hive/verified-open label, when any comment or label counted.

// Start signals reported through AgentStartHook. AgentStartSignalLabel is
// retired: a label request no longer starts anything (see the file comment);
// the constant stays so consumers that recorded it keep compiling and
// recognising historical journal entries.
const (
	AgentStartSignalComment   = "comment"
	AgentStartSignalLabel     = "label"
	AgentStartSignalClaim     = "claim"
	AgentStartSignalPRRequest = "pr_request"
	// AgentStartSignalBranch is a comment whose start marker names the
	// branch or pull request the work lives on (`<!-- hive:start
	// branch=<name> -->` / `pr=<n>`): PR evidence, like a PR request.
	AgentStartSignalBranch = "branch"
)

// StartSignalIsPREvidence reports whether signal says a pull request is
// under way — a PR request naming the issue, or a start marker naming a
// branch / PR. Only such a signal stages the issue
// stage/030-pr-in-progress; a bare claim or start comment is
// stage/020-claimed (hivecommons/hive#13069).
func StartSignalIsPREvidence(signal string) bool {
	return signal == AgentStartSignalPRRequest || signal == AgentStartSignalBranch
}

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
	`(?:starting|started|resuming|resumed|implementing)\s+(?:work\s+|on\s+(?:this|it)\s+)?now` +
	`|(?:working\s+on|picking\s+up)\s+(?:this|it)\s+now` +
	`|picking\s+(?:this|it)\s+up\s+now` +
	`|no\s+longer\s+blocked|now\s+unblocked|unblocked\s+now` +
	`)\b`)

// commentStartOpener matches a comment that OPENS (after markdown and an
// optional "Status:"/"Update:" label) with a verb of beginning work:
// "Starting …", "Started …", "Resuming …", "Implementing …", "Working on …",
// "Picking this up …", "Taking this …". Group 1 captures a negation ("Not
// starting"), which does not count.
var commentStartOpener = regexp.MustCompile(`(?i)` +
	`^\W*(?:(?:status|update)\s*:\W*)?((?:not|never|won't|can't|cannot)\s+)?(?:` +
	`start(?:ing|ed)\b` +
	`|resum(?:ing|ed)\b` +
	`|implementing\b` +
	`|working\s+on\b` +
	`|picking\s+(?:this|it)\s+up\b` +
	`|taking\s+(?:this|it)\b` +
	`)`)

// commentStartMarker is the structured start marker an agent (or the relay
// helper) may put in a comment to state unambiguously that it is beginning
// work on the issue: `<!-- hive:start -->`, optionally with attributes.
var commentStartMarker = regexp.MustCompile(`<!--\s*hive:start\b[^>]*-->`)

// commentStartBranchAttr matches a `branch=<name>` or `pr=<n>` attribute
// inside the start marker: the agent names where its pull request lives.
var commentStartBranchAttr = regexp.MustCompile(`(?i)\b(?:branch|pr)=["']?[^\s"'>]+`)

// commentStartNamesBranch reports whether body carries a start marker whose
// attributes name a branch or pull request.
func commentStartNamesBranch(body string) bool {
	for _, m := range commentStartMarker.FindAllString(body, -1) {
		if commentStartBranchAttr.MatchString(m) {
			return true
		}
	}
	return false
}

// CommentStartMarker is the canonical spelling of the start marker.
const CommentStartMarker = "<!-- hive:start -->"

// commentSaysStartNow reports whether body carries a non-negated
// commentStartNow phrase.
func commentSaysStartNow(body string) bool {
	for _, m := range commentStartNow.FindAllStringSubmatch(body, -1) {
		if m[1] == "" {
			return true
		}
	}
	return false
}

// commentIsStart reports whether a comment body is an explicit start
// statement: it carries the start marker, a start-now phrase, or opens with a
// verb of beginning work, and is not an explicit deferral or escalation that
// it fails to override. Anything else — a progress note, a triage verdict, a
// verification result, a gate check, a status report — is not a start.
func commentIsStart(body string) bool {
	if commentStartMarker.MatchString(body) {
		return true
	}
	if commentIsDeferral(body) || commentIsEscalation(body) {
		return false
	}
	if commentSaysStartNow(body) {
		return true
	}
	if m := commentStartOpener.FindStringSubmatch(body); m != nil && m[1] == "" {
		return true
	}
	return false
}

// commentIsDeferral reports whether a comment body is an explicit deferral
// with no statement that work is starting now.
func commentIsDeferral(body string) bool {
	return commentDeferral.MatchString(body) && !commentSaysStartNow(body)
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
	return commentEscalation.MatchString(body) && !commentSaysStartNow(body)
}

// issueRequestStartSignal maps a successful issue request to the start
// signal it is, or "" for requests that are not one (see the file comment):
// a claim request is a start signal whatever an accompanying comment said; a
// comment is one only when commentIsStart says it states a start; creating
// an issue, closing one, asking for a review and every label request are
// not.
func issueRequestStartSignal(kind, body string) string {
	switch kind {
	case "comment":
		if commentStartNamesBranch(body) {
			return AgentStartSignalBranch
		}
		if commentIsStart(body) {
			return AgentStartSignalComment
		}
		return ""
	case "claim":
		return AgentStartSignalClaim
	}
	return ""
}

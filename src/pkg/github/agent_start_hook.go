package github

import "regexp"

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

// commentDeferral matches the deferral forms from #12387: a comment that
// opens with "Deferred"/"Deferring" (markdown emphasis allowed), "deferred
// this cycle", "deferring … until/pending", "picks up once/after/when", a
// present tense "blocked by/on #N" (opening the comment or a line, or
// "is/still/currently blocked by #N") and "waiting on/for #N". Such a comment
// is not evidence of work starting, even when it names the issue or its
// blocker. A past-tense mention ("previously blocked by #N") or an aside
// ("cleanup deferred to #N") in an ordinary work comment does not match.
// Explicit label, claim and PR requests remain start signals independently
// of any accompanying comment.
var commentDeferral = regexp.MustCompile(`(?im)(?:^\W*deferr(?:ed|ing)\b` +
	`|\bdeferred\s+this\s+cycle\b` +
	`|\bdeferr(?:ed|ing)\s+(?:\w+\s+){0,2}?(?:until|pending)\b` +
	`|\bpicks?\s+up\s+(?:once|after|when)\b` +
	`|^\W*blocked\s+(?:by|on)\s+#\d+` +
	`|\b(?:is|am|are|still|currently|remains?)\s+blocked\s+(?:by|on)\s+#\d+` +
	`|\bwaiting\s+(?:on|for)\s+#\d+)`)

// commentStartOverride matches an explicit statement that work is starting
// or resuming. It wins over commentDeferral so a resume comment such as
// "Previously blocked by #N; that merged, starting now." still records the
// claim that keeps a second agent off the issue (#10527). Occurrences that
// commentNonStart matches are removed before this runs, so a start word
// inside the deferral clause does not override it.
var commentStartOverride = regexp.MustCompile(`(?i)\b(?:starting|started|resuming|resumed|picking\s+(?:this\s+|it\s+)?up|picked\s+(?:this\s+|it\s+)?up|back\s+on\s+(?:this|it)\b|no\s+longer\s+blocked|unblocked)\b`)

// commentNonStart matches start words that do not describe work starting
// now: a negated start ("not starting until #N merges"), a start conditional
// on something else within the same clause ("once #N merges and this is
// unblocked", "resuming after that", "will resume once it merges" — the
// rest of that clause is consumed with it), and a third party's past start
// ("#N was started by scanner", "#N already started this refactor"). Each
// match is blanked before commentStartOverride is evaluated.
var commentNonStart = regexp.MustCompile(`(?i)(?:\b(?:not|never|won't|cannot|can't|before|without)\s+(?:be\s+)?(?:start|resum|pick|unblock)\w*` +
	`|\b(?:once|after|when|until|while|unless|if|pending)\b[^.;!?\n]*?\b(?:start|resum|pick|unblock)\w*` +
	`|\b(?:start|resum|unblock)\w*\s+(?:once|after|when|until|unless|if|pending)\b[^.;!?\n]*` +
	`|\bpick\w*\s+(?:this\s+|it\s+)?up\s+(?:once|after|when|until|unless|if|pending)\b[^.;!?\n]*` +
	`|\b(?:was|were|been|already|has|had|#\d+)\s+(?:started|resumed|picked\s+(?:this\s+|it\s+)?up)\b)`)

// commentIsDeferral reports whether a comment body is an explicit deferral
// that carries no overriding statement of work starting.
func commentIsDeferral(body string) bool {
	if !commentDeferral.MatchString(body) {
		return false
	}
	return !commentStartOverride.MatchString(commentNonStart.ReplaceAllString(body, " "))
}

// issueRequestStartSignal maps a successful issue request to its start
// signal, or "" when it does not show work starting. Creating an issue,
// closing one, asking for a review, and deferral comments are not starts.
func issueRequestStartSignal(kind, body string) string {
	switch kind {
	case "comment":
		if commentIsDeferral(body) {
			return ""
		}
		return AgentStartSignalComment
	case "label":
		return AgentStartSignalLabel
	case "claim":
		return AgentStartSignalClaim
	}
	return ""
}

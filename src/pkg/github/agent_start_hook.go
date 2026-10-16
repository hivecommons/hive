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

// commentDeferral matches explicit statements that work is being postponed.
// Such a comment is not evidence of work starting, even when it names the
// issue or its blocker. Explicit label, claim and PR requests remain start
// signals independently of any accompanying deferral comment (#12387).
var commentDeferral = regexp.MustCompile(`(?i)\b(?:deferred|deferring|picks?\s+up\s+(?:once|after)|blocked\s+by\s+#\d+|waiting\s+(?:on|for)\s+#\d+)\b`)

// issueRequestStartSignal maps a successful issue request to its start
// signal, or "" when it does not show work starting. Creating an issue,
// closing one, asking for a review, and deferral comments are not starts.
func issueRequestStartSignal(kind, body string) string {
	switch kind {
	case "comment":
		if commentDeferral.MatchString(body) {
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

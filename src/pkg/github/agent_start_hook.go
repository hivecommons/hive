package github

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

// issueRequestStartSignal maps a successful issue-request kind to the start
// signal it is, or "" for kinds that are not one: creating an issue, closing
// one, and asking for a review do not show the agent working an existing
// issue.
func issueRequestStartSignal(kind string) string {
	switch kind {
	case "comment":
		return AgentStartSignalComment
	case "label":
		return AgentStartSignalLabel
	case "claim":
		return AgentStartSignalClaim
	}
	return ""
}

package github

import "time"

// Agent actions the relay performed on an agent's behalf (hivecommons/hive#18916).
//
// The per-kick outcome record needs to know what a kick came to. The relay is
// where an agent's GitHub writes happen, so it tells one observer, with ids
// only: which agent, what kind of write, where. The hook is told after the
// write succeeded; a failed or denied request is never reported.

// Agent action kinds.
const (
	AgentActionPush    = "push"
	AgentActionComment = "comment"
)

// AgentAction is one write the relay made for an agent.
type AgentAction struct {
	Agent  string
	Kind   string
	Repo   string
	Number int
	Branch string
	SHA    string
	At     time.Time
}

// AgentActionHook is told of each AgentAction.
type AgentActionHook func(AgentAction)

// SetAgentActionHook installs (or with nil, removes) the hook. Safe to call
// before or after the watchers start.
func (c *Client) SetAgentActionHook(fn AgentActionHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.agentActionHook.Store(nil)
		return
	}
	c.agentActionHook.Store(&fn)
}

// notifyAgentAction reports a completed write to the installed hook, if any.
func (c *Client) notifyAgentAction(a AgentAction) {
	if c == nil || a.Agent == "" {
		return
	}
	if hook := c.agentActionHook.Load(); hook != nil && *hook != nil {
		(*hook)(a)
	}
}

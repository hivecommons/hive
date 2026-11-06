package main

import "strings"

// Task MCP kick-pointer wiring. A kick tells the agent that the `hive-task`
// MCP server is connected for its launch, and with
// task_mcp.drop_stuffed_context it also cuts the work lists down to refs
// (pkg/scheduler). Both are only right for a launch that was handed the
// connection and a token the endpoint accepts, so the scheduler is given the
// two facts it cannot read from an agent's config.

// wireSchedulerTaskMCP hands the scheduler the task MCP endpoint its kicks may
// point at and the runtime's backend lookup.
func (b *boot) wireSchedulerTaskMCP() {
	b.sched.SetTaskMCPURL(b.taskMCPURLForKicks())
	b.sched.SetAgentBackendLookup(b.agentLaunchBackend)
}

// taskMCPURLForKicks is taskMCPURLForAgents as the scheduler should see it:
// empty when no launch token can be minted. Without a dashboard token
// taskMCPLaunchTokenForAgents returns nothing, the launch carries the bare URL
// and the endpoint refuses every call, so no kick may point at it. The token
// is read once at boot, like the dashboard server's own copy of it.
func (b *boot) taskMCPURLForKicks() string {
	if b == nil || b.cfg == nil || strings.TrimSpace(b.cfg.Dashboard.AuthToken) == "" {
		return ""
	}
	return b.taskMCPURLForAgents()
}

// agentLaunchBackend is the backend the runtime launches an agent on, with a
// backend override applied (provider rotation sets one without touching the
// agent's config). ok is false before the agent manager exists and for an
// agent it does not know; the scheduler then reads the configured backend.
func (b *boot) agentLaunchBackend(name string) (string, bool) {
	if b == nil || b.agentMgr == nil {
		return "", false
	}
	return b.agentMgr.EffectiveBackend(name)
}

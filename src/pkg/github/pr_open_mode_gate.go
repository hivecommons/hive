package github

import "strconv"

// The per-repo open-PRs gate (project.repo_policies[].open_prs).
//
// An agent opens a PR on GitHub only through this client: the pr-request
// relay (hive-open-pr) and the sandbox path (CreateAgentPR). The proxy
// hard-denies a direct POST /pulls. Both paths ask the gate first and refuse,
// with an agent_write_refused audit entry, when the repo's mode forbids the
// PR. The gate is the hive's: it reads the live config and, for
// workflow_only, the hive's own record of workflow hand-offs.

// PROpenModeGate decides whether agent may open a PR in repo. issues are
// the issue numbers the request names (hive-open-pr --issues); nil when the
// path has none. It returns the repo's mode and, when the PR is refused, the
// reason ("" allows).
type PROpenModeGate func(agent, repo string, issues []int) (mode, reason string)

// GateOpenPRs is the gate name a refused pr-request result and its log line
// carry.
const GateOpenPRs = "open-prs"

// SetPROpenModeGate installs (or with nil, removes) the open-PRs gate. Safe
// to call before or after the watcher starts. With no gate every repo is on.
func (c *Client) SetPROpenModeGate(fn PROpenModeGate) {
	if c == nil {
		return
	}
	if fn == nil {
		c.prOpenModeGate.Store(nil)
		return
	}
	c.prOpenModeGate.Store(&fn)
}

// refuseOpenPRs runs the open-PRs gate and, on a refusal, records an
// agent_write_refused audit entry for the open. It returns the reason and
// whether the PR is refused.
func (c *Client) refuseOpenPRs(agent, repo string, issues []int, via string) (string, bool) {
	if c == nil {
		return "", false
	}
	gate := c.prOpenModeGate.Load()
	if gate == nil || *gate == nil {
		return "", false
	}
	mode, reason := (*gate)(agent, repo, issues)
	if reason == "" {
		return "", false
	}
	target := 0
	if len(issues) > 0 {
		target = issues[0]
	}
	c.recordWriteAudit(AuditActionAgentWriteRefused, InvocationMeta{Agent: agent},
		WriteTarget{Repo: repo, Number: target},
		"op", WriteOpOpenPR, "gate", GateOpenPRs, "mode", mode, "via", via,
		"issues", strconv.Itoa(len(issues)), "outcome", "refused")
	return reason, true
}

package config

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// What a workflow hand-off needs the agent to be allowed to do
// (hivecommons/hive#18749).
//
// A model profile names a role; the hive names the agent that fills it. The
// model's grant (its tools and outputs) describes the role and grants the
// session nothing: an agent acts with the mode and capabilities its own
// config gives it, which the agent manager writes to the session's mode and
// capability files and the write gates enforce (hive-open-pr and the push
// relay need a push-capable mode and the repository's ACMM level to allow
// the agent; a comment needs ISSUES_ONLY or the converse capability). So a
// profile resolves only to an agent that holds what its deliverable needs.
// The checks below are those gates, read from the same config.

// AgentInteractionMode is the GitHub interaction mode the agent's sessions
// run in: its configured `mode:`, else the default for its name at the
// hive's ACMM level. It is the agent manager's own rule (the value written
// to the session's mode file).
func (c *Config) AgentInteractionMode(name string) crux.AgentMode {
	if c == nil {
		return crux.ModeAdvisory
	}
	if modeStr := c.Agents[name].Mode; modeStr != "" {
		if parsed, ok := crux.ParseAgentMode(modeStr); ok {
			return parsed
		}
	}
	return crux.DefaultAgentMode(name, c.ACMMLevelOrZero())
}

// PRRepoPolicyRefusal is the per-repository gate the pull-request relay
// applies before it opens anything for an agent: the repository's effective
// ACMM level must let the agent push. Nil when it does.
func (c *Config) PRRepoPolicyRefusal(agentName, repo string) error {
	level := c.EffectiveACMMLevelForRepo(repo)
	if level <= 0 {
		level = c.ACMMLevelOrZero()
	}
	if !crux.DefaultAgentMode(agentName, level).CanPush() {
		return fmt.Errorf("repo %s effective ACMM L%d does not allow %s to open PRs", repo, level, agentName)
	}
	return nil
}

// action is what the need asks the agent to be allowed to do, as the
// reasons word it.
func (n WorkflowDispatchNeed) action() string {
	if n.Reviews {
		return "comment"
	}
	return "open pull requests"
}

// place is where, as the reasons word it.
func (n WorkflowDispatchNeed) place() string {
	if strings.TrimSpace(n.Repo) == "" {
		return "the bound repository"
	}
	return n.Repo
}

func (n WorkflowDispatchNeed) excludes(name string) bool {
	for _, x := range n.Exclude {
		if x == name {
			return true
		}
	}
	return false
}

// workflowDispatchLack says why the agent may not do what the need asks, or
// "" when it may. A profile that opens a pull request needs a push-capable
// mode and, when the repository is known, the repository's ACMM level to
// allow the agent; a reviewing profile needs comment rights (ISSUES_ONLY or
// above, or the converse capability).
func (c *Config) workflowDispatchLack(name string, need WorkflowDispatchNeed) string {
	mode := c.AgentInteractionMode(name)
	if need.Reviews {
		a := c.Agents[name]
		if mode.CanComment() || (a.Converse != nil && *a.Converse) {
			return ""
		}
		return fmt.Sprintf("may not comment in %s (mode %s, no converse capability)", need.place(), mode)
	}
	if !mode.CanPush() {
		return fmt.Sprintf("may not open pull requests in %s (mode %s)", need.place(), mode)
	}
	if strings.TrimSpace(need.Repo) != "" {
		if err := c.PRRepoPolicyRefusal(name, need.Repo); err != nil {
			return fmt.Sprintf("may not open pull requests in %s (the repository's ACMM level does not allow it)", need.place())
		}
		// A workflow hand-off is what workflow_only admits, so only off
		// (the repo's or the hive-wide project.open_prs) stops it.
		if c.RepoOpenPRsMode(need.Repo) == RepoOpenPRsOff {
			return fmt.Sprintf("may not open pull requests in %s (open PRs is off for the repository)", need.place())
		}
	}
	return ""
}

// WorkflowDispatchAgentQualifies reports whether the agent may do what the
// need asks, and the reason when it may not. An agent that is unknown,
// disabled or a replica overlay never qualifies.
func (c *Config) WorkflowDispatchAgentQualifies(agent string, need WorkflowDispatchNeed) (bool, string) {
	if c == nil {
		return false, "no config"
	}
	a, exists := c.Agents[agent]
	if !exists || !a.Enabled || a.ReplicaOf != "" {
		return false, "is not an enabled agent of this hive"
	}
	if lack := c.workflowDispatchLack(agent, need); lack != "" {
		return false, lack
	}
	return true, ""
}

// WorkflowDispatchAgentChoice is one agent a profile could be pinned to,
// with whether it may do what the profile needs.
type WorkflowDispatchAgentChoice struct {
	Agent string `json:"agent"`
	// Mode is the agent's interaction mode (ADVISORY, ISSUES_AND_PRS, ...).
	Mode string `json:"mode"`
	// Qualifies is false when the agent may not do what the profile needs;
	// Reason says why.
	Qualifies bool   `json:"qualifies"`
	Reason    string `json:"reason,omitempty"`
}

// WorkflowDispatchCandidates lists every pinnable agent
// (WorkflowDispatchAgentChoices) with whether it qualifies for the need, so
// a picker can mark the ones that do not.
func (c *Config) WorkflowDispatchCandidates(need WorkflowDispatchNeed) []WorkflowDispatchAgentChoice {
	names := c.WorkflowDispatchAgentChoices()
	out := make([]WorkflowDispatchAgentChoice, 0, len(names))
	for _, name := range names {
		lack := c.workflowDispatchLack(name, need)
		out = append(out, WorkflowDispatchAgentChoice{Agent: name, Mode: c.AgentInteractionMode(name).String(), Qualifies: lack == "", Reason: lack})
	}
	return out
}

// workflowDispatchUnfilled is the reason nothing fills the profile once
// every step has been tried. names are the pinnable agents.
func (c *Config) workflowDispatchUnfilled(profile string, need WorkflowDispatchNeed, names []string) string {
	if len(names) == 0 {
		return fmt.Sprintf("no agent fills %s: this hive has no enabled agent", profile)
	}
	var capable, untried []string
	advisoryOnly := true
	for _, name := range names {
		if c.AgentInteractionMode(name) != crux.ModeAdvisory {
			advisoryOnly = false
		}
		if c.workflowDispatchLack(name, need) != "" {
			continue
		}
		capable = append(capable, name)
		if !need.excludes(name) {
			untried = append(untried, name)
		}
	}
	switch {
	case len(capable) == 0 && advisoryOnly:
		return fmt.Sprintf("no agent on this hive may %s in %s; agents are advisory-only", need.action(), need.place())
	case len(capable) == 0:
		return fmt.Sprintf("no agent on this hive may %s in %s; no agent's mode allows it", need.action(), need.place())
	case len(untried) == 0:
		return fmt.Sprintf("no agent other than %s may %s in %s", strings.Join(capable, ", "), need.action(), need.place())
	}
	return fmt.Sprintf("no agent fills %s: %s does not work on %s", profile, untried[0], need.place())
}

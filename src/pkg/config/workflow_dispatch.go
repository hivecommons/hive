package config

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// WorkflowDispatchConfig is the `workflow.dispatch:` block: which hive agent
// fills each agent profile a bound workflow model hands items to
// (workflowmodel.DispatchTarget), and the prompt it is kicked with.
//
//	workflow:
//	  dispatch:
//	    enabled: true                 # default
//	    profiles:
//	      techdocs-drafter:
//	        agent: writer             # default: an agent named after the profile, else writer/docs/feature,
//	                                  # else the hive's issue worker (scanner) or, for a reviewing profile, the reviewer,
//	                                  # else any agent that may do what the profile needs
//	        prompt_template: ""       # default: the built-in dispatch prompt
//	      techdocs-verifier:
//	        disabled: true            # never dispatch this profile automatically
type WorkflowDispatchConfig struct {
	// Enabled turns automatic dispatch on. Nil means true: a model that
	// declares a dispatch target and a fleet with an agent to fill it
	// dispatches out of the box. Dispatch also needs workflow.enabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Profiles maps a model profile name to its hive-side settings.
	Profiles map[string]WorkflowDispatchProfile `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// WorkflowDispatchProfile is one profile's hive-side dispatch settings.
type WorkflowDispatchProfile struct {
	// Agent is the hive agent the profile is handed to. Empty resolves by
	// default (WorkflowDispatchAgent).
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// PromptTemplate replaces the built-in dispatch prompt. ${VAR}
	// placeholders are expanded (docs/design/workflow-models.md lists them).
	PromptTemplate string `yaml:"prompt_template,omitempty" json:"prompt_template,omitempty"`
	// Disabled keeps the profile from being dispatched automatically; the
	// dashboard's owner-only "Send to <profile>" still works.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// DefaultWorkflowDispatchRoles are the agent roles a profile with no
// configured agent falls back to, in preference order.
var DefaultWorkflowDispatchRoles = []string{"writer", "docs", "feature"}

// WorkflowDispatchIssueWorkerRole is the role of the hive's standard
// issue-working agent: the lane every issue with no lane of its own is
// classified into (classify.DefaultLane, which a test keeps equal to this)
// and the default owner of a red pull request (review.DefaultFixerAgent). It
// opens the pull requests for issues on a default hive, so it is the last
// resort for a profile that opens one when the hive has no writer, docs or
// feature agent (hivecommons/hive#18690), provided its mode lets it open
// one (hivecommons/hive#18749).
const WorkflowDispatchIssueWorkerRole = "scanner"

// WorkflowDispatchReviewerRole is the last resort for a reviewing profile
// (one that reports on the item and opens no pull request).
const WorkflowDispatchReviewerRole = "reviewer"

// How a profile's agent was resolved (WorkflowDispatchResolution.Source).
const (
	WorkflowDispatchSourcePinned   = "pinned"
	WorkflowDispatchSourceNamed    = "named"
	WorkflowDispatchSourceRole     = "role"
	WorkflowDispatchSourceFallback = "fallback"
)

// WorkflowDispatchNeed is what a hand-off asks of the agent, for the
// last-resort pick. The zero value asks for an agent that opens a pull
// request, in any repository.
type WorkflowDispatchNeed struct {
	// Repo is the bound repository the item lives in ("" when unknown).
	Repo string
	// Reviews is true for a profile that reports on the item instead of
	// opening a pull request (TechDocs' techdocs-verifier). It needs
	// comment rights; any other profile needs to open pull requests.
	Reviews bool
	// Exclude names agents that must not be picked: the ones an earlier
	// hand-off of the same item went to without starting it, so the
	// automatic retry goes to a different agent (hivecommons/hive#18749).
	Exclude []string
}

// WorkflowDispatchResolution is the outcome of resolving a profile's agent.
type WorkflowDispatchResolution struct {
	Profile string `json:"profile"`
	// Agent is the agent to kick; empty when nothing fills the profile.
	Agent string `json:"agent,omitempty"`
	// Source is one of the WorkflowDispatchSource* values when Agent is set.
	Source string `json:"source,omitempty"`
	// Mode is the interaction mode of Agent (ISSUES_AND_PRS, ...): what its
	// session actually holds, which the hand-off prompt states.
	Mode string `json:"mode,omitempty"`
	// Needs is what the profile needs its agent to be allowed to do
	// ("open pull requests in cncf/techdocs").
	Needs string `json:"needs,omitempty"`
	// Pinned is workflow.dispatch.profiles.<profile>.agent as configured.
	Pinned string `json:"pinned,omitempty"`
	// Disabled is true when the profile is turned off for automatic
	// dispatch and the resolution honored that.
	Disabled bool `json:"disabled,omitempty"`
	// Problem says, in one short sentence, why nothing fills the profile.
	// Empty when Agent is set or the profile is Disabled.
	Problem string `json:"problem,omitempty"`
}

// OK reports whether an agent fills the profile.
func (r WorkflowDispatchResolution) OK() bool { return r.Agent != "" }

// workflowDispatchMu guards Workflow.Dispatch.Profiles: the scheduler reads
// it on every pass while the dashboard pins an agent
// (SetWorkflowDispatchAgent).
var workflowDispatchMu sync.RWMutex

// DispatchEnabled is the effective workflow.dispatch.enabled (default true).
func (c WorkflowDispatchConfig) DispatchEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Profile returns the settings for a model profile (zero when unset).
func (c WorkflowDispatchConfig) Profile(name string) WorkflowDispatchProfile {
	workflowDispatchMu.RLock()
	defer workflowDispatchMu.RUnlock()
	if c.Profiles == nil {
		return WorkflowDispatchProfile{}
	}
	return c.Profiles[strings.TrimSpace(name)]
}

// WorkflowDispatchAgent resolves the hive agent that fills a model profile
// with no knowledge of the item (ResolveWorkflowDispatch with a zero need).
// ok is false when nothing fills the profile, the profile is disabled, or a
// configured agent does not exist (a typo never falls back to another agent).
func (c *Config) WorkflowDispatchAgent(profile string) (agent string, ok bool) {
	r := c.ResolveWorkflowDispatch(profile, WorkflowDispatchNeed{}, false)
	return r.resolved()
}

// WorkflowDispatchAgentForManual is WorkflowDispatchAgent for the owner's
// "Send to <profile>": a profile disabled for automatic dispatch still
// resolves, since a person chose to hand the item off.
func (c *Config) WorkflowDispatchAgentForManual(profile string) (agent string, ok bool) {
	r := c.ResolveWorkflowDispatch(profile, WorkflowDispatchNeed{}, true)
	return r.resolved()
}

// resolved is the (agent, ok) form: a pinned agent that is not usable is
// still named, so a caller can say which one.
func (r WorkflowDispatchResolution) resolved() (string, bool) {
	if r.OK() {
		return r.Agent, true
	}
	if r.Disabled {
		return "", false
	}
	return r.Pinned, false
}

// ResolveWorkflowDispatch resolves the hive agent that fills a model
// profile. Every step accepts only an enabled agent that may do what the
// profile needs in the bound repository (workflowDispatchLack: open pull
// requests, or comment for a reviewing profile), in this order:
//
//  1. workflow.dispatch.profiles.<profile>.agent. A pin that names no
//     enabled agent, or an agent that may not do what the profile needs, is
//     a Problem, never a fallback;
//  2. an agent named after the profile;
//  3. the first agent (by name) whose role is the earliest of
//     DefaultWorkflowDispatchRoles;
//  4. the last resort, so a standard fleet works out of the box, among the
//     agents that serve need.Repo: the first whose role is
//     WorkflowDispatchReviewerRole for a reviewing profile, else
//     WorkflowDispatchIssueWorkerRole; failing that, the first agent (by
//     name) that qualifies at all.
//
// Replica overlays are never chosen: the base agent is the one kicked.
// need.Exclude removes agents from every step. When nothing qualifies the
// Problem says so. manual is the owner's "Send to <profile>": a profile
// disabled for automatic dispatch still resolves.
func (c *Config) ResolveWorkflowDispatch(profile string, need WorkflowDispatchNeed, manual bool) WorkflowDispatchResolution {
	profile = strings.TrimSpace(profile)
	out := WorkflowDispatchResolution{Profile: profile}
	if c == nil || profile == "" {
		out.Problem = "no profile to fill"
		return out
	}
	out.Needs = need.action() + " in " + need.place()
	set := c.Workflow.Dispatch.Profile(profile)
	out.Pinned = strings.TrimSpace(set.Agent)
	if set.Disabled && !manual {
		out.Disabled = true
		return out
	}
	usable := func(name string) bool {
		a, exists := c.Agents[name]
		return exists && a.Enabled && a.ReplicaOf == ""
	}
	pick := func(name, source string) WorkflowDispatchResolution {
		out.Agent, out.Source, out.Mode = name, source, c.AgentInteractionMode(name).String()
		return out
	}
	if out.Pinned != "" {
		if !usable(out.Pinned) {
			out.Problem = fmt.Sprintf("the pinned agent %q is not an enabled agent of this hive", out.Pinned)
			return out
		}
		if lack := c.workflowDispatchLack(out.Pinned, need); lack != "" {
			out.Problem = fmt.Sprintf("the pinned agent %q %s", out.Pinned, lack)
			return out
		}
		if need.excludes(out.Pinned) {
			out.Problem = fmt.Sprintf("the pinned agent %q did not start the hand-off, and a pin is never replaced by another agent", out.Pinned)
			return out
		}
		return pick(out.Pinned, WorkflowDispatchSourcePinned)
	}
	fits := func(name string) bool {
		return usable(name) && !need.excludes(name) && c.workflowDispatchLack(name, need) == ""
	}
	if fits(profile) {
		return pick(profile, WorkflowDispatchSourceNamed)
	}
	names := c.WorkflowDispatchAgentChoices()
	withRole := func(role string, keep func(name string) bool) string {
		for _, name := range names {
			r := c.Agents[name].Role
			if r == "" {
				r = name
			}
			if strings.EqualFold(r, role) && keep(name) {
				return name
			}
		}
		return ""
	}
	for _, role := range DefaultWorkflowDispatchRoles {
		if name := withRole(role, fits); name != "" {
			return pick(name, WorkflowDispatchSourceRole)
		}
	}
	lastResort := WorkflowDispatchIssueWorkerRole
	if need.Reviews {
		lastResort = WorkflowDispatchReviewerRole
	}
	serves := func(name string) bool { return fits(name) && c.AgentServesRepo(name, need.Repo) }
	if name := withRole(lastResort, serves); name != "" {
		return pick(name, WorkflowDispatchSourceFallback)
	}
	for _, name := range names {
		if serves(name) {
			return pick(name, WorkflowDispatchSourceFallback)
		}
	}
	out.Problem = c.workflowDispatchUnfilled(profile, need, names)
	return out
}

// WorkflowDispatchAgentChoices are the agents a profile can be handed to:
// every enabled agent that is not a replica overlay, by name.
func (c *Config) WorkflowDispatchAgentChoices() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Agents))
	for name, a := range c.Agents {
		if a.Enabled && a.ReplicaOf == "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// SetWorkflowDispatchAgent pins workflow.dispatch.profiles.<profile>.agent,
// or clears the pin when agent is empty (the profile resolves by default
// again). The profile's other settings are kept. A non-empty agent must be
// one of WorkflowDispatchAgentChoices. It reports whether anything changed;
// the caller saves the config.
func (c *Config) SetWorkflowDispatchAgent(profile, agent string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("nil config")
	}
	profile, agent = strings.TrimSpace(profile), strings.TrimSpace(agent)
	if profile == "" {
		return false, fmt.Errorf("workflow dispatch: empty profile")
	}
	if agent != "" {
		a, exists := c.Agents[agent]
		if !exists || !a.Enabled || a.ReplicaOf != "" {
			return false, fmt.Errorf("workflow dispatch: %q is not an enabled agent of this hive", agent)
		}
	}
	workflowDispatchMu.Lock()
	defer workflowDispatchMu.Unlock()
	cur := c.Workflow.Dispatch.Profiles[profile]
	if strings.TrimSpace(cur.Agent) == agent {
		return false, nil
	}
	// Copy on write: a config save marshals the map without this lock.
	next := make(map[string]WorkflowDispatchProfile, len(c.Workflow.Dispatch.Profiles)+1)
	for k, v := range c.Workflow.Dispatch.Profiles {
		next[k] = v
	}
	cur.Agent = agent
	if cur == (WorkflowDispatchProfile{}) {
		delete(next, profile)
	} else {
		next[profile] = cur
	}
	if len(next) == 0 {
		next = nil
	}
	c.Workflow.Dispatch.Profiles = next
	return true, nil
}

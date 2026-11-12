package config

import (
	"fmt"
	"sort"
	"strings"
)

// reviewerAutoscaleNoLaneReason is the reason ReviewerAutoscaleLane gives
// when no agent is named and none qualifies.
const reviewerAutoscaleNoLaneReason = "no enabled agent takes the review swarm's reviews; list one in review.reviewer_agents or name one in review.reviewers.autoscale.agent"

// ReviewerAutoscaleLane resolves the base agent the reviewer autoscaler
// manages and, when there is none, says why in plain words.
//
// The backlog the autoscaler sizes the lane against is the review swarm's
// (the PRs the dispatch planner left waiting for a free reviewer), so the
// lane has to be one the swarm's dispatcher hands reviews to: a replica of
// any other agent could never take one of those PRs. The role alone does not
// decide that. A pack can ship two agents with role "reviewer", the swarm's
// reviewer and a lane that only works escalated PRs, and with
// review.reviewer_agents naming the first, the second receives no review.
//
// Order:
//  1. review.reviewers.autoscale.agent when it qualifies; a name that does
//     not qualify resolves to nothing rather than to some other agent.
//  2. The first entry of review.reviewer_agents that qualifies.
//  3. With no reviewer list, the first name-sorted agent that qualifies
//     under the swarm's keyword rule.
//
// An agent qualifies when reviewerAutoscaleIneligible finds nothing against
// it.
func (c *Config) ReviewerAutoscaleLane() (agent, reason string) {
	if c == nil {
		return "", reviewerAutoscaleNoLaneReason
	}
	if name := strings.TrimSpace(c.Review.Reviewers.Autoscale.Agent); name != "" {
		if why := c.reviewerAutoscaleIneligible(name); why != "" {
			return "", fmt.Sprintf("review.reviewers.autoscale.agent %q %s", name, why)
		}
		return name, ""
	}
	for _, name := range c.Review.ReviewerAgents {
		if name = strings.TrimSpace(name); name != "" && c.reviewerAutoscaleIneligible(name) == "" {
			return name, ""
		}
	}
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c.reviewerAutoscaleIneligible(name) == "" {
			return name, ""
		}
	}
	return "", reviewerAutoscaleNoLaneReason
}

// ReviewerAutoscaleAgent is the agent half of ReviewerAutoscaleLane: the
// base agent the autoscaler manages, or empty when nothing qualifies.
func (c *Config) ReviewerAutoscaleAgent() string {
	agent, _ := c.ReviewerAutoscaleLane()
	return agent
}

// reviewerAutoscaleIneligible says why name cannot be the autoscaled lane,
// or returns empty when it can. The lane must be a declared base agent that
// is enabled and that the review dispatcher would hand a review to: admitted
// by review.reviewer_agents (or, with no list, by the keyword rule), kick
// driven and not on-demand. These are the config-level conditions of
// review.reviewCapableAgents. A pause is left out on purpose: it is a
// temporary state, and resolving another lane for as long as it lasts would
// retire the paused lane's replicas.
func (c *Config) reviewerAutoscaleIneligible(name string) string {
	const neverDispatched = "so the review swarm never dispatches a review to it"
	ac, ok := c.Agents[name]
	switch {
	case !ok:
		return "is not a declared agent"
	case ac.ReplicaOf != "":
		return fmt.Sprintf("is derived from %s; name the base agent", ac.ReplicaOf)
	case !ac.Enabled:
		return "is disabled"
	case ac.OnDemand:
		return "is on-demand, " + neverDispatched
	case !ac.UsesGovernorKick():
		return "is not kick-driven, " + neverDispatched
	case c.reviewerAdmitted(name, ac):
		return ""
	case len(c.Review.ReviewerAgents) > 0:
		return "is not in review.reviewer_agents, " + neverDispatched
	default:
		return `has no "review" in its name, role, aliases or keywords, ` + neverDispatched
	}
}

// ValidateReviewerAutoscaleAgent refuses an enabled autoscale block that
// names a lane agent the review swarm cannot dispatch to. Writers of the
// setting (the review settings PUT) call it before saving. Config load does
// not: a hive.yaml that already names such an agent still loads, autoscale
// stays off for it (ReviewerAutoscaleEnabled) and ReviewerAutoscaleWarnings
// reports why.
func (c *Config) ValidateReviewerAutoscaleAgent() error {
	if c == nil {
		return nil
	}
	a := c.Review.Reviewers.Autoscale
	name := strings.TrimSpace(a.Agent)
	if !a.Enabled || name == "" {
		return nil
	}
	if why := c.reviewerAutoscaleIneligible(name); why != "" {
		return fmt.Errorf("review.reviewers.autoscale.agent %q %s", name, why)
	}
	return nil
}

// ReviewerAutoscaleWarnings reports an autoscale block that is switched on
// but resolves to no lane, so the reviewer count is not scaling: at most one
// warning, carrying the reason. Empty when autoscale is off or has a lane.
func ReviewerAutoscaleWarnings(cfg *Config) []string {
	if cfg == nil || !cfg.Review.Reviewers.Autoscale.Enabled {
		return nil
	}
	if _, reason := cfg.ReviewerAutoscaleLane(); reason != "" {
		return []string{"review.reviewers.autoscale is on but is not scaling any agent: " + reason}
	}
	return nil
}

package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// reviewerAutoscaleResolved is reviewSection.ReviewerAutoscale: which agent
// review.reviewers.autoscale scales. The stored block names an agent only
// when the operator set one, so without this the Settings page (and
// settings_read review) could not tell which lane was growing.
type reviewerAutoscaleResolved struct {
	Enabled bool `json:"enabled"`
	// Agent is review.reviewers.autoscale.agent as stored; empty means the
	// hive chooses.
	Agent string `json:"agent"`
	// EffectiveAgent is the base agent the setting resolves to
	// (config.ReviewerAutoscaleLane): the one whose replicas scale while
	// Enabled is true. Empty when no agent qualifies.
	EffectiveAgent string `json:"effective_agent"`
	// Reason says why EffectiveAgent is empty. While Enabled is true it is
	// the reason the reviewer count is not scaling.
	Reason string `json:"reason,omitempty"`
}

// reviewerAutoscaleResolution builds reviewSection.ReviewerAutoscale.
func reviewerAutoscaleResolution(cfg *config.Config) reviewerAutoscaleResolved {
	if cfg == nil {
		return reviewerAutoscaleResolved{}
	}
	a := cfg.Review.Reviewers.Autoscale
	agent, reason := cfg.ReviewerAutoscaleLane()
	return reviewerAutoscaleResolved{
		Enabled:        a.Enabled,
		Agent:          strings.TrimSpace(a.Agent),
		EffectiveAgent: agent,
		Reason:         reason,
	}
}

// reviewerAutoscaleAgentError checks the autoscale block a review settings
// PUT is about to save against the reviewer list the same request leaves in
// place (reviewerAgents is the body's reviewer_agents, nil when absent). A
// named lane agent the review swarm cannot dispatch to is refused, with the
// reason config.ValidateReviewerAutoscaleAgent gives.
func reviewerAutoscaleAgentError(cfg *config.Config, staged config.ReviewerAutoscaleConfig, reviewerAgents *[]string) error {
	if cfg == nil {
		return nil
	}
	next := *cfg
	next.Review.Reviewers.Autoscale = staged
	if reviewerAgents != nil {
		next.Review.ReviewerAgents = *reviewerAgents
	}
	return next.ValidateReviewerAutoscaleAgent()
}

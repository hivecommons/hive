package review

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// IndependentReviewHeading opens the independent-review instruction in both
// the cadence kick overlay and the swarm perspective prompt. The scheduler
// checks for it before inserting the overlay, so it must stay stable.
const IndependentReviewHeading = "## Independent review"

// independentReviewBody is the shared half of the instruction: what to hand
// the sub-agent, what to have it hunt for, and where to record its model. The
// two callers differ only in how the model is named — per PR line on the
// cadence kick, pinned on a swarm perspective kick — so that clause is the one
// parameter, and the rest cannot drift between the two paths.
func independentReviewBody(scope, modelClause string) string {
	return fmt.Sprintf("%s\n\n%s, launch a sub-agent (Agent tool) with `model` set to %s. Give the sub-agent the PR URL plus the review checklist, and have it probe author-model failure modes: invented APIs, tests that assert the implementation rather than the requirement, and swallowed errors. Record the sub-agent's verdict and model in the verdict artifact as `review_model`. Never review a PR yourself with the author's model.", IndependentReviewHeading, scope, modelClause)
}

// QueueIndependentReviewSection is the cadence-kick overlay: every PR line
// that carries `review_with=` names its own model.
func QueueIndependentReviewSection() string {
	return independentReviewBody("For each PR whose list line includes `review_with=<backend>/<model>`", "the PR's `review_with` model value")
}

// PerspectiveIndependentReviewSection is the swarm form of the same
// instruction for one perspective kick, whose model the planner already chose.
// Empty when no model was selected, so a hive without a pool sees no change.
func PerspectiveIndependentReviewSection(backend, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	target := model
	if b := strings.TrimSpace(backend); b != "" {
		target = b + "/" + model
	}
	return independentReviewBody(fmt.Sprintf("Run this review with `%s`: do not review it yourself", target), fmt.Sprintf("`%s`", model))
}

// ReviewModelChoice is what the planner decided for one perspective kick:
// the model the review must run with (empty when nothing is configured) and
// whether that came from the agent's fallback rather than the pool.
type ReviewModelChoice struct {
	Backend string
	Model   string
	// Fallback is true when no pool entry was eligible and the agent's
	// fallback decided. Skip and RequiresHuman refine it.
	Fallback      bool
	Skip          bool
	RequiresHuman bool
	// Configured is false when the agent has no review_models block and no
	// perspective_models entry applies, in which case Model is empty and the
	// kick carries no independent-review instruction.
	Configured bool
}

// chooseReviewModel applies agents.<name>.review_models — with a
// review.perspective_models entry substituted for the pool when one names
// this perspective — against the PR's author model. It is the single place
// the swarm decides a model, and it defers to config.ReviewModelsConfig.Select
// so the cadence kick and the swarm cannot disagree.
func chooseReviewModel(agent AgentCapability, perspective Perspective, authorModel string, overrides map[string]config.ReviewModelPoolEntry) ReviewModelChoice {
	cfg := agent.ReviewModels
	configured := cfg.Configured()
	if entry, ok := overrides[string(perspective)]; ok && strings.TrimSpace(entry.Model) != "" {
		cfg = cfg.WithPool([]config.ReviewModelPoolEntry{entry})
		configured = true
	}
	if !configured {
		return ReviewModelChoice{}
	}
	sel := cfg.Select(authorModel, agent.Backend, agent.Model)
	return ReviewModelChoice{Backend: sel.Backend, Model: sel.Model, Fallback: sel.Fallback, Skip: sel.Skip, RequiresHuman: sel.RequiresHuman, Configured: true}
}

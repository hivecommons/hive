package config

import (
	"strings"
)

// ReviewHead is one model "head" of a base reviewer agent
// (agents.<name>.review_heads, hivecommons/hive#13542). Each head is
// materialized as Replicas derived agents named <base>-<alias>-<n> that copy
// the base agent's config with only Backend and Model replaced. With split
// perspectives (review.combined_perspectives: false) the swarm reviews every
// perspective once per head, so two models judge the same perspective
// independently instead of each taking a different one.
type ReviewHead struct {
	Backend string `yaml:"backend" json:"backend"`
	Model   string `yaml:"model" json:"model"`
	// Alias names the head in agent names and verdicts. Empty means a slug
	// of <backend>-<model> (the backend prefix is not repeated when the
	// model already starts with it: claude/claude-sonnet-5-5 is
	// "claude-sonnet-5-5", not "claude-claude-sonnet-5-5").
	Alias string `yaml:"alias,omitempty" json:"alias,omitempty"`
	// Replicas is how many agents serve this head (1..MaxAgentReplicas, 0
	// means 1). The swarm picks the least-loaded one per kick.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty"`
}

// reviewHeadSlug lowercases s and turns every run of characters outside
// [a-z0-9] into one '-', trimming dashes at either end. The entrypoint's
// hive_collect_agent_names mirrors this exactly so it provisions the same
// names the manager launches.
func reviewHeadSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// EffectiveAlias is the head's name segment: Alias when set, else the
// <backend>-<model> slug.
func (h ReviewHead) EffectiveAlias() string {
	if a := reviewHeadSlug(h.Alias); a != "" {
		return a
	}
	backend := reviewHeadSlug(h.Backend)
	model := reviewHeadSlug(h.Model)
	if backend == "" {
		return model
	}
	if model == backend || strings.HasPrefix(model, backend+"-") {
		return model
	}
	if model == "" {
		return backend
	}
	return backend + "-" + model
}

// ReviewHeadKey is the identity one head's replicas share: <base>-<alias>.
// It is what verdicts and dispatch records carry as "head".
func ReviewHeadKey(base string, h ReviewHead) string {
	return base + "-" + h.EffectiveAlias()
}

// IsReviewHead reports whether this agent was derived from a base's
// review_heads.
func (a AgentConfig) IsReviewHead() bool { return a.ReviewHead != "" }

// ReviewHeadKeys lists the head keys a reviewer base produces, in config
// order. Empty when the agent declares no review_heads.
func (a AgentConfig) ReviewHeadKeys(base string) []string {
	keys := make([]string, 0, len(a.ReviewHeads))
	for _, h := range a.ReviewHeads {
		keys = append(keys, ReviewHeadKey(base, h))
	}
	return keys
}

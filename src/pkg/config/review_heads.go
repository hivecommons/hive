package config

import (
	"fmt"
	"sort"
	"strings"
)

// maxAgentNameLen is the longest agent name whose Unix user and private token
// group "hive-<name>" fit groupadd's 32-character group-name limit.
const maxAgentNameLen = 32 - len("hive-")

// ReviewHeadAgentName is the derived agent for one replica of a head.
func ReviewHeadAgentName(base string, h ReviewHead, index int) string {
	return fmt.Sprintf("%s-%d", ReviewHeadKey(base, h), index)
}

// reviewHeadAgentNames validates every base's review_heads and returns the
// derived agent names they produce, mapped to their base.
func reviewHeadAgentNames(baseAgents map[string]AgentConfig) (map[string]string, error) {
	out := map[string]string{}
	bases := make([]string, 0, len(baseAgents))
	for name := range baseAgents {
		bases = append(bases, name)
	}
	sort.Strings(bases)
	for _, base := range bases {
		heads := baseAgents[base].ReviewHeads
		seen := map[string]bool{}
		for i, h := range heads {
			if strings.TrimSpace(h.Backend) == "" || strings.TrimSpace(h.Model) == "" {
				return nil, fmt.Errorf("agent %s: review_heads[%d]: backend and model are required", base, i)
			}
			alias := h.EffectiveAlias()
			if alias == "" {
				return nil, fmt.Errorf("agent %s: review_heads[%d]: alias is empty after slugging", base, i)
			}
			if seen[alias] {
				return nil, fmt.Errorf("agent %s: review_heads: alias %q is used twice", base, alias)
			}
			seen[alias] = true
			replicas := normalizeReplicaCount(h.Replicas)
			if replicas < 1 || replicas > MaxAgentReplicas {
				return nil, fmt.Errorf("agent %s: review_heads[%d]: replicas must be between 1 and %d", base, i, MaxAgentReplicas)
			}
			for n := 1; n <= replicas; n++ {
				name := ReviewHeadAgentName(base, h, n)
				if len(name) > maxAgentNameLen {
					return nil, fmt.Errorf("agent %s: review_heads[%d]: derived agent name %q is %d characters; hive-<name> must fit the 32-character group limit (max %d), set a shorter alias", base, i, name, len(name), maxAgentNameLen)
				}
				if other, ok := out[name]; ok {
					return nil, fmt.Errorf("agent %s: review_heads would create %s, which agent %s's review_heads also create", base, name, other)
				}
				out[name] = base
			}
		}
	}
	return out, nil
}

// expandReviewHeads materializes base's review_heads. Each derived agent is a
// copy of the base with the head's backend and model, marked ReplicaOf the
// base (so it shares the base's cadence, prompt and scope, and is stripped on
// save) and ReviewHead with its head key. Settings that only make sense for
// the base's own backend (launch command, reasoning effort) are dropped when
// the head runs a different backend, and the head carries no review_models
// pool or review_heads of its own: the head IS the model.
func expandReviewHeads(base string, agent AgentConfig) []AgentConfig {
	var out []AgentConfig
	for _, h := range agent.ReviewHeads {
		replicas := normalizeReplicaCount(h.Replicas)
		for n := 1; n <= replicas; n++ {
			name := ReviewHeadAgentName(base, h, n)
			derived := agent
			derived.SetLoadState(name, derived.SourceFile())
			derived.ID = name
			derived.BeadsDir = fmt.Sprintf("/data/beads/%s", name)
			if !strings.EqualFold(strings.TrimSpace(h.Backend), strings.TrimSpace(agent.Backend)) {
				derived.LaunchCmd = ""
				derived.ReasoningEffort = ""
			}
			derived.Backend = strings.TrimSpace(h.Backend)
			derived.Model = strings.TrimSpace(h.Model)
			derived.ReviewModels = ReviewModelsConfig{}
			derived.ReviewHeads = nil
			derived.Replicas = replicas
			derived.ReplicaOf = base
			derived.ReplicaIndex = n
			derived.ReplicaCount = replicas
			derived.ReviewHead = ReviewHeadKey(base, h)
			out = append(out, derived)
		}
	}
	return out
}

// reviewRoleToken is the substring that marks an agent as a reviewer when
// review.reviewer_agents is empty. It mirrors review.hasReviewCapability
// (src/pkg/review/dispatch.go), which the dispatcher applies to the same
// fields; keep the two in step.
const reviewRoleToken = "review"

// reviewerAdmitted reports whether the dispatcher would hand review kicks to
// the enabled agent name under this config's review.reviewer_agents: a
// listed name, a replica or review head of a listed base, or — with no list
// — an agent whose name, role, aliases or keywords carry the review token.
// It mirrors review.ReviewCapable so config-time checks on review_heads see
// the same reviewer set the planner does.
func (c *Config) reviewerAdmitted(name string, ac AgentConfig) bool {
	if len(c.Review.ReviewerAgents) > 0 {
		listed := map[string]bool{}
		for _, n := range c.Review.ReviewerAgents {
			if n = strings.TrimSpace(n); n != "" {
				listed[n] = true
			}
		}
		return listed[name] || (ac.ReplicaOf != "" && listed[ac.ReplicaOf])
	}
	fields := append([]string{name, ac.Role}, ac.Aliases...)
	fields = append(fields, ac.LaneKeywords...)
	fields = append(fields, ac.DetectKeywords...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), reviewRoleToken) {
			return true
		}
	}
	return false
}

// dispatchedReviewerBases lists the enabled base agents the swarm hands
// review kicks to: admitted by reviewerAdmitted, not paused or on-demand,
// and kick-driven (the same filter as review.reviewCapableAgents), sorted.
func (c *Config) dispatchedReviewerBases() []string {
	var bases []string
	for name, ac := range c.EnabledAgents() {
		if ac.ReplicaOf != "" || ac.Paused || ac.OnDemand || !ac.UsesGovernorKick() {
			continue
		}
		if c.reviewerAdmitted(name, ac) {
			bases = append(bases, name)
		}
	}
	sort.Strings(bases)
	return bases
}

// ExpectedReviewHeads lists every head key the swarm dispatches each
// perspective to: the review_heads of every enabled base agent the
// dispatcher admits as a reviewer, sorted, with split and combined
// perspectives alike (hivecommons/hive#15058). A base review.reviewer_agents
// does not name is not dispatched, so its heads are not expected either, and
// an all-approve aggregate never waits on them. Empty when no admitted agent
// declares heads.
func (c *Config) ExpectedReviewHeads() []string {
	if c == nil {
		return nil
	}
	var keys []string
	for _, name := range c.dispatchedReviewerBases() {
		keys = append(keys, c.Agents[name].ReviewHeadKeys(name)...)
	}
	sort.Strings(keys)
	return keys
}

// EffectiveHeadMaxRetries resolves review.head_max_retries: 0 means the
// head SHA's max_redispatch budget applies per head as well.
func (r ReviewConfig) EffectiveHeadMaxRetries() int {
	if r.HeadMaxRetries <= 0 {
		return r.EffectiveMaxRedispatch()
	}
	return r.HeadMaxRetries
}

// validateReviewHeads checks each head's backend is one the governor can
// launch, review.head_max_retries is not negative, and that with split
// perspectives the admitted reviewers either all declare review_heads or
// none do. Head dispatch asks every perspective of every head and leaves
// nothing for a plain reviewer lane alongside them, so a mixed set would
// silently idle the lanes without heads (hivecommons/hive#13542). Under
// combined perspectives that mix loaded before heads were dispatched there
// (hivecommons/hive#15058), so it stays loadable and ReviewHeadsWarnings
// reports it instead. Structural checks (required fields, aliases, replicas,
// name collisions) run where the heads are expanded, so a bad head fails
// config load the same way a bad replicas value does.
func (c *Config) validateReviewHeads() error {
	if c.Review.HeadMaxRetries < 0 {
		return fmt.Errorf("review.head_max_retries must be >= 0")
	}
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, h := range c.Agents[name].ReviewHeads {
			if err := c.Governor.ValidateBackend(strings.TrimSpace(h.Backend)); err != nil {
				return fmt.Errorf("agent %s: review_heads[%d]: %w", name, i, err)
			}
		}
	}
	if c.Review.CombinedPerspectives {
		return nil
	}
	if withHeads, without := c.mixedReviewHeadBases(); len(withHeads) > 0 {
		return fmt.Errorf("review_heads: with review.combined_perspectives: false every reviewer must declare review_heads: %s do, %s do not; add review_heads to them or leave them out of review.reviewer_agents",
			strings.Join(withHeads, ", "), strings.Join(without, ", "))
	}
	return nil
}

// ReviewHeadsWarnings reports, under combined perspectives, an admitted
// reviewer set that mixes bases with and without review_heads. Combined head
// dispatch (hivecommons/hive#15058) asks every perspective of every head, so
// the lanes without heads receive no combined perspectives while the heads
// cover them. Split perspectives reject the same mix at load
// (validateReviewHeads); here it is a warning because the mix loaded fine
// before heads were dispatched under combined perspectives.
func ReviewHeadsWarnings(cfg *Config) []string {
	if cfg == nil || !cfg.Review.CombinedPerspectives {
		return nil
	}
	withHeads, without := cfg.mixedReviewHeadBases()
	if len(withHeads) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"review_heads under review.combined_perspectives: %s declare review_heads, %s do not — while the heads cover every perspective, %s will receive no combined perspectives; add review_heads to them or leave them out of review.reviewer_agents",
		strings.Join(withHeads, ", "), strings.Join(without, ", "), strings.Join(without, ", "))}
}

// mixedReviewHeadBases splits the dispatched reviewer bases into those that
// declare review_heads and those that do not, returning both only when the
// set mixes them.
func (c *Config) mixedReviewHeadBases() (withHeads, without []string) {
	for _, name := range c.dispatchedReviewerBases() {
		if len(c.Agents[name].ReviewHeads) > 0 {
			withHeads = append(withHeads, name)
		} else {
			without = append(without, name)
		}
	}
	if len(withHeads) == 0 || len(without) == 0 {
		return nil, nil
	}
	return withHeads, without
}

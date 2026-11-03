package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Reviewer autoscale defaults (hivecommons/hive#14685). Every threshold the
// controller consults is either one of these or a config key below, so there
// is one place to read when asking "why did the lane grow?".
const (
	// DefaultReviewerAutoscaleMin is the floor of active reviewer replicas.
	DefaultReviewerAutoscaleMin = 1
	// DefaultReviewerAutoscaleMax is the ceiling of active reviewer replicas.
	// It is below MaxAgentReplicas so a hive that opts in gets a safe band
	// without also raising the fleet-wide replica cap.
	DefaultReviewerAutoscaleMax = 4
	// DefaultReviewerAutoscaleTargetBacklog is how many PRs may wait per
	// active reviewer before the lane is considered behind.
	DefaultReviewerAutoscaleTargetBacklog = 2
	// DefaultReviewerAutoscaleScaleUpCycles is how many consecutive cycles
	// the lane must be behind before a reviewer is added. Small on purpose:
	// a backlog costs merge latency every cycle it persists.
	DefaultReviewerAutoscaleScaleUpCycles = 2
	// DefaultReviewerAutoscaleScaleDownCycles is how many consecutive idle
	// cycles precede retiring a reviewer. Larger than scale-up so a lull
	// between pushes does not thrash the lane.
	DefaultReviewerAutoscaleScaleDownCycles = 5
	// DefaultReviewerAutoscaleCooldown is the minimum gap between two
	// scaling actions in either direction.
	DefaultReviewerAutoscaleCooldown = 5 * time.Minute
	// DefaultReviewerAutoscaleCPUPerReviewer is the CPU (cores) one reviewer
	// is assumed to need; scale-up is refused when the cgroup cannot carry it.
	DefaultReviewerAutoscaleCPUPerReviewer = 1.0
	// DefaultReviewerAutoscaleMemPerReviewerMB is the memory one reviewer is
	// assumed to need, in MiB.
	DefaultReviewerAutoscaleMemPerReviewerMB = 2048
)

// ReviewersConfig groups settings about the reviewer agents themselves (as
// opposed to what they review). Lives under review.reviewers.
type ReviewersConfig struct {
	// Autoscale lets the reviewer lane grow and shrink with the review
	// backlog instead of running a fixed replica count.
	Autoscale ReviewerAutoscaleConfig `yaml:"autoscale,omitempty" json:"autoscale,omitzero"`
}

// ReviewerAutoscaleConfig is review.reviewers.autoscale
// (hivecommons/hive#14685). Opt-in: the zero value keeps the static
// agents.<reviewer>.replicas behaviour.
//
// When enabled, the lane's base agent (Agent, or the first reviewer-role
// agent) is expanded to the controller's current target instead of its
// declared replicas; the declared value only seeds the initial count,
// clamped into [Min, Max]. Dynamic replicas are ordinary replicas —
// reviewer-3, reviewer-4 — with the same role, prompt, beads dir, scoped
// token and cadence key family as reviewer-2, so nothing downstream can
// tell them apart.
type ReviewerAutoscaleConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Agent names the base reviewer agent that scales. Empty picks the
	// first (name-sorted) enabled agent with role "reviewer", falling back
	// to review.reviewer_agents[0].
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	Min   int    `yaml:"min,omitempty" json:"min,omitempty"`
	Max   int    `yaml:"max,omitempty" json:"max,omitempty"`
	// TargetBacklogPerReviewer is the waiting-PRs-per-reviewer ratio above
	// which the lane is behind.
	TargetBacklogPerReviewer int `yaml:"target_backlog_per_reviewer,omitempty" json:"target_backlog_per_reviewer,omitempty"`
	// ScaleUpCycles / ScaleDownCycles are the sustained-signal lengths.
	ScaleUpCycles   int `yaml:"scale_up_cycles,omitempty" json:"scale_up_cycles,omitempty"`
	ScaleDownCycles int `yaml:"scale_down_cycles,omitempty" json:"scale_down_cycles,omitempty"`
	// Cooldown is a Go duration string; the minimum gap between actions.
	Cooldown string `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
	// CPUPerReviewer / MemPerReviewerMB are the per-reviewer resource cost
	// the cgroup guard reserves before adding one.
	CPUPerReviewer   float64 `yaml:"cpu_per_reviewer,omitempty" json:"cpu_per_reviewer,omitempty"`
	MemPerReviewerMB int     `yaml:"mem_per_reviewer_mb,omitempty" json:"mem_per_reviewer_mb,omitempty"`
}

// EffectiveMin returns Min or its default.
func (a ReviewerAutoscaleConfig) EffectiveMin() int {
	if a.Min > 0 {
		return a.Min
	}
	return DefaultReviewerAutoscaleMin
}

// EffectiveMax returns Max or its default, never above MaxAgentReplicas.
func (a ReviewerAutoscaleConfig) EffectiveMax() int {
	max := a.Max
	if max <= 0 {
		max = DefaultReviewerAutoscaleMax
	}
	if max > MaxAgentReplicas {
		max = MaxAgentReplicas
	}
	if max < a.EffectiveMin() {
		max = a.EffectiveMin()
	}
	return max
}

// EffectiveTargetBacklogPerReviewer returns the ratio or its default.
func (a ReviewerAutoscaleConfig) EffectiveTargetBacklogPerReviewer() int {
	if a.TargetBacklogPerReviewer > 0 {
		return a.TargetBacklogPerReviewer
	}
	return DefaultReviewerAutoscaleTargetBacklog
}

// EffectiveScaleUpCycles returns the sustained scale-up length or its default.
func (a ReviewerAutoscaleConfig) EffectiveScaleUpCycles() int {
	if a.ScaleUpCycles > 0 {
		return a.ScaleUpCycles
	}
	return DefaultReviewerAutoscaleScaleUpCycles
}

// EffectiveScaleDownCycles returns the sustained scale-down length or its default.
func (a ReviewerAutoscaleConfig) EffectiveScaleDownCycles() int {
	if a.ScaleDownCycles > 0 {
		return a.ScaleDownCycles
	}
	return DefaultReviewerAutoscaleScaleDownCycles
}

// EffectiveCooldown parses Cooldown, falling back to the default when unset
// or malformed (Validate rejects malformed values at load).
func (a ReviewerAutoscaleConfig) EffectiveCooldown() time.Duration {
	raw := strings.TrimSpace(a.Cooldown)
	if raw == "" {
		return DefaultReviewerAutoscaleCooldown
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultReviewerAutoscaleCooldown
	}
	return d
}

// EffectiveCPUPerReviewer returns the per-reviewer CPU reservation.
func (a ReviewerAutoscaleConfig) EffectiveCPUPerReviewer() float64 {
	if a.CPUPerReviewer > 0 {
		return a.CPUPerReviewer
	}
	return DefaultReviewerAutoscaleCPUPerReviewer
}

// EffectiveMemPerReviewerMB returns the per-reviewer memory reservation.
func (a ReviewerAutoscaleConfig) EffectiveMemPerReviewerMB() int {
	if a.MemPerReviewerMB > 0 {
		return a.MemPerReviewerMB
	}
	return DefaultReviewerAutoscaleMemPerReviewerMB
}

// Clamp bounds n into [EffectiveMin, EffectiveMax].
func (a ReviewerAutoscaleConfig) Clamp(n int) int {
	if n < a.EffectiveMin() {
		return a.EffectiveMin()
	}
	if n > a.EffectiveMax() {
		return a.EffectiveMax()
	}
	return n
}

// Validate rejects a band that cannot be honoured.
func (a ReviewerAutoscaleConfig) Validate() error {
	if !a.Enabled {
		return nil
	}
	if a.Min < 0 || a.Max < 0 {
		return fmt.Errorf("review.reviewers.autoscale: min and max must not be negative")
	}
	if a.Max > MaxAgentReplicas {
		return fmt.Errorf("review.reviewers.autoscale: max %d exceeds the replica cap %d", a.Max, MaxAgentReplicas)
	}
	if a.Min > 0 && a.Max > 0 && a.Min > a.Max {
		return fmt.Errorf("review.reviewers.autoscale: min %d is greater than max %d", a.Min, a.Max)
	}
	if a.TargetBacklogPerReviewer < 0 || a.ScaleUpCycles < 0 || a.ScaleDownCycles < 0 || a.MemPerReviewerMB < 0 || a.CPUPerReviewer < 0 {
		return fmt.Errorf("review.reviewers.autoscale: thresholds must not be negative")
	}
	if raw := strings.TrimSpace(a.Cooldown); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return fmt.Errorf("review.reviewers.autoscale: cooldown %q is not a positive duration (e.g. 5m)", a.Cooldown)
		}
	}
	return nil
}

// ReviewerAutoscaleAgent resolves the base agent the autoscaler manages:
// the configured name when it is a declared base agent, else the first
// name-sorted enabled base agent with role "reviewer", else
// review.reviewer_agents[0]. Empty when nothing qualifies.
func (c *Config) ReviewerAutoscaleAgent() string {
	if c == nil {
		return ""
	}
	a := c.Review.Reviewers.Autoscale
	if name := strings.TrimSpace(a.Agent); name != "" {
		if agent, ok := c.Agents[name]; ok && agent.ReplicaOf == "" {
			return name
		}
		return ""
	}
	names := make([]string, 0, len(c.Agents))
	for name, agent := range c.Agents {
		if agent.ReplicaOf != "" || !agent.Enabled {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(agent.Role), "reviewer") {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return names[0]
	}
	for _, name := range c.Review.ReviewerAgents {
		if agent, ok := c.Agents[name]; ok && agent.ReplicaOf == "" {
			return name
		}
	}
	return ""
}

// ReviewerAutoscaleEnabled reports whether the lane is under autoscale
// control and resolves to a real base agent.
func (c *Config) ReviewerAutoscaleEnabled() bool {
	return c != nil && c.Review.Reviewers.Autoscale.Enabled && c.ReviewerAutoscaleAgent() != ""
}

// reviewerAutoscaleReplicaCount is the replica count ExpandAgentReplicas
// materialises for the autoscaled lane: the controller's current target when
// it has set one, else the declared replicas clamped into the band.
func (c *Config) reviewerAutoscaleReplicaCount(declared int) int {
	a := c.Review.Reviewers.Autoscale
	if c.ReviewerAutoscaleActive > 0 {
		return a.Clamp(c.ReviewerAutoscaleActive)
	}
	return a.Clamp(normalizeReplicaCount(declared))
}

// SetReviewerAutoscaleActive records the controller's target replica count
// and re-expands agents so the lane materialises it. It is the single write
// path for ReviewerAutoscaleActive.
func (c *Config) SetReviewerAutoscaleActive(n int) error {
	if c == nil {
		return nil
	}
	c.ReviewerAutoscaleActive = c.Review.Reviewers.Autoscale.Clamp(n)
	return c.ExpandAgentReplicas()
}

// ReviewerAutoscaleReplicas lists the lane's current agent names in replica
// order (base first), or nil when autoscale is off.
func (c *Config) ReviewerAutoscaleReplicas() []string {
	if !c.ReviewerAutoscaleEnabled() {
		return nil
	}
	base := c.ReviewerAutoscaleAgent()
	out := []string{base}
	count := c.Agents[base].ReplicaCount
	for i := 2; i <= count; i++ {
		name := replicaAgentName(base, i)
		if _, ok := c.Agents[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

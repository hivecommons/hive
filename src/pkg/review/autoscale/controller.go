// Package autoscale sizes the reviewer lane to its backlog
// (hivecommons/hive#14685).
//
// The Controller is a pure decision function over one Observation per
// governor cycle. It never touches agents itself: the caller materialises a
// scale-up by raising the lane's replica count, honours a drain by
// withholding kicks from the named reviewer, and retires a drained reviewer
// only when the controller says its turn has ended. Time comes in on the
// Observation, so tests drive it with a fake clock and no sleeps.
package autoscale

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Action is what the caller should do after an Observe.
type Action string

const (
	// ActionHold means no change this cycle.
	ActionHold Action = "hold"
	// ActionScaleUp means raise the lane to Decision.To replicas.
	ActionScaleUp Action = "scale-up"
	// ActionDrain means stop kicking Decision.Agent; it retires when idle.
	ActionDrain Action = "drain"
	// ActionUndrain means Decision.Agent may be kicked again: demand returned
	// before its turn ended, so keeping it is cheaper than a fresh replica.
	ActionUndrain Action = "undrain"
	// ActionRetire means Decision.Agent is idle and may now be removed,
	// lowering the lane to Decision.To replicas.
	ActionRetire Action = "retire"
	// ActionBlocked means a scale-up was wanted and a guard refused it;
	// Decision.Reason names the guard.
	ActionBlocked Action = "blocked"
)

// Guard names recorded in Decision.Reason when a wanted scale-up is refused.
const (
	GuardAtMax              = "at-max"
	GuardCooldown           = "cooldown"
	GuardSaturated          = "saturated"
	GuardProviderBudget     = "provider-budget"
	GuardProviderHeadroom   = "provider-headroom"
	GuardMaxParallelReviews = "max-parallel-reviews"
	GuardCPU                = "cpu"
	GuardMemory             = "memory"
)

const (
	// SaturationFraction is the share of active reviewers whose last kick
	// failed to reach the CLI prompt above which the lane is treated as
	// saturated: more reviewers would fail the same way, so demand is not the
	// problem.
	SaturationFraction = 0.5
	// MinProviderHeadroomPct is the provider quota remaining below which a
	// scale-up is refused; a new reviewer would only burn the last of it.
	MinProviderHeadroomPct = 10.0
	// mibBytes converts the cgroup's byte counters to the MiB config unit.
	mibBytes = 1024 * 1024
)

// Reviewer is one active reviewer replica as the caller sees it this cycle.
type Reviewer struct {
	Name string
	// Index is the replica index (base agent is 1). Drains pick the highest.
	Index int
	// Busy is true while the agent is mid-turn or has a review kick in
	// flight; a draining reviewer is never retired while Busy.
	Busy bool
	// Saturated is true when the agent's most recent kicks were undeliverable
	// ("CLI did not reach input prompt").
	Saturated bool
}

// Resources is the pod's cgroup budget as of this cycle. Known is false when
// no cgroup limits could be read, which disables the resource guard rather
// than blocking (a dev laptop has no cgroup, a pod always does).
type Resources struct {
	Known       bool
	CPULimit    float64
	CPUUsed     float64
	MemLimitMB  int
	MemUsedMB   int
	MemLimitSet bool
}

// FreeCPU is the unused share of the CPU quota in cores.
func (r Resources) FreeCPU() float64 { return r.CPULimit - r.CPUUsed }

// FreeMemMB is the unused memory in MiB; unbounded when no limit is set.
func (r Resources) FreeMemMB() int {
	if !r.MemLimitSet {
		return int(^uint(0) >> 1)
	}
	return r.MemLimitMB - r.MemUsedMB
}

// Observation is everything the controller looks at for one cycle.
type Observation struct {
	Now       time.Time
	Reviewers []Reviewer
	// WaitingKeys identify PRs that needed a reviewer this cycle and got
	// none (repo#number@head). The controller ages them across cycles.
	WaitingKeys []string
	// InFlight is the number of review kicks currently pending on the lane.
	InFlight int
	// DwellThreshold is the stage dwell after which a waiting PR counts as
	// late regardless of the backlog ratio; zero disables the age signal.
	DwellThreshold time.Duration
	// MaxParallelReviews is review.max_parallel_reviews; zero disables the
	// slot guard.
	MaxParallelReviews int
	Resources          Resources
	// ProviderBudgetExceeded is the inference budget latch.
	ProviderBudgetExceeded bool
	// ProviderHeadroomPct is the lowest remaining quota across providers;
	// negative when unknown.
	ProviderHeadroomPct float64
}

// Decision is the controller's answer for one cycle.
type Decision struct {
	Action Action
	From   int
	To     int
	Agent  string
	Reason string
	At     time.Time
}

// String renders the one-line audit form, e.g.
// "reviewer autoscale: 2→3 reason=backlog=7 oldest=41m".
func (d Decision) String() string {
	switch d.Action {
	case ActionHold:
		return fmt.Sprintf("reviewer autoscale: hold at %d reason=%s", d.From, d.Reason)
	case ActionDrain, ActionUndrain, ActionRetire:
		return fmt.Sprintf("reviewer autoscale: %d→%d %s %s reason=%s", d.From, d.To, d.Action, d.Agent, d.Reason)
	case ActionBlocked:
		return fmt.Sprintf("reviewer autoscale: %d→%d blocked by %s", d.From, d.To, d.Reason)
	default:
		return fmt.Sprintf("reviewer autoscale: %d→%d reason=%s", d.From, d.To, d.Reason)
	}
}

// Status is the snapshot the dashboard shows.
type Status struct {
	Enabled      bool      `json:"enabled"`
	Agent        string    `json:"agent,omitempty"`
	Min          int       `json:"min"`
	Max          int       `json:"max"`
	Current      int       `json:"current"`
	Target       int       `json:"target"`
	Draining     []string  `json:"draining,omitempty"`
	Backlog      int       `json:"backlog"`
	OldestWait   string    `json:"oldestWait,omitempty"`
	UpStreak     int       `json:"upStreak"`
	DownStreak   int       `json:"downStreak"`
	LastDecision *Decision `json:"lastDecision,omitempty"`
}

// Controller holds the sustained-signal state between cycles.
type Controller struct {
	cfg        config.ReviewerAutoscaleConfig
	agent      string
	firstSeen  map[string]time.Time
	draining   map[string]bool
	upStreak   int
	downStreak int
	lastAction time.Time
	last       *Decision
	backlog    int
	oldest     time.Duration
	current    int
	target     int
}

// New builds a controller for the named base agent.
func New(cfg config.ReviewerAutoscaleConfig, agent string) *Controller {
	return &Controller{
		cfg:       cfg,
		agent:     agent,
		firstSeen: map[string]time.Time{},
		draining:  map[string]bool{},
	}
}

// Reconfigure swaps the band and thresholds without losing streaks.
func (c *Controller) Reconfigure(cfg config.ReviewerAutoscaleConfig, agent string) {
	c.cfg = cfg
	c.agent = agent
}

// Draining reports whether kicks to name should be withheld.
func (c *Controller) Draining(name string) bool {
	return c != nil && c.draining[name]
}

// DrainingNames lists draining reviewers, sorted.
func (c *Controller) DrainingNames() []string {
	if c == nil || len(c.draining) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.draining))
	for n := range c.draining {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Status returns the dashboard snapshot.
func (c *Controller) Status() Status {
	if c == nil {
		return Status{}
	}
	st := Status{
		Enabled:    true,
		Agent:      c.agent,
		Min:        c.cfg.EffectiveMin(),
		Max:        c.cfg.EffectiveMax(),
		Current:    c.current,
		Target:     c.target,
		Draining:   c.DrainingNames(),
		Backlog:    c.backlog,
		UpStreak:   c.upStreak,
		DownStreak: c.downStreak,
	}
	if c.oldest > 0 {
		st.OldestWait = c.oldest.Round(time.Second).String()
	}
	if c.last != nil {
		d := *c.last
		st.LastDecision = &d
	}
	return st
}

// Observe folds one cycle into the controller and returns what to do.
func (c *Controller) Observe(obs Observation) Decision {
	d := c.observe(obs)
	d.At = obs.Now
	if d.Action != ActionHold {
		c.last = &d
	}
	return d
}

func (c *Controller) observe(obs Observation) Decision {
	c.ageBacklog(obs)
	active := len(obs.Reviewers)
	c.current = active
	c.pruneDraining(obs.Reviewers)
	serving := active - len(c.draining)
	if serving < 0 {
		serving = 0
	}
	c.target = serving
	min, max := c.cfg.EffectiveMin(), c.cfg.EffectiveMax()

	behind := c.behind(serving, obs.DwellThreshold)
	idle := c.backlog == 0 && obs.InFlight < active
	if behind {
		c.upStreak++
		c.downStreak = 0
	} else if idle {
		c.downStreak++
		c.upStreak = 0
	} else {
		c.upStreak = 0
		c.downStreak = 0
	}

	// A drained reviewer whose turn ended is retired before anything else:
	// it completes the earlier decision rather than being a new one.
	if name, ok := c.retirable(obs.Reviewers); ok {
		delete(c.draining, name)
		c.lastAction = obs.Now
		c.target = active - 1
		return Decision{Action: ActionRetire, From: active, To: active - 1, Agent: name, Reason: "idle"}
	}

	reason := c.reason()
	if behind && c.upStreak >= c.cfg.EffectiveScaleUpCycles() {
		// Demand came back while a reviewer was draining: keep it.
		if name, ok := c.highestDraining(obs.Reviewers); ok {
			delete(c.draining, name)
			c.upStreak = 0
			c.lastAction = obs.Now
			c.target = serving + 1
			return Decision{Action: ActionUndrain, From: serving, To: serving + 1, Agent: name, Reason: reason}
		}
		if guard := c.scaleUpGuard(obs, active, serving, max); guard != "" {
			return Decision{Action: ActionBlocked, From: serving, To: serving + 1, Reason: guard}
		}
		c.upStreak = 0
		c.lastAction = obs.Now
		c.target = active + 1
		return Decision{Action: ActionScaleUp, From: active, To: active + 1, Reason: reason}
	}

	if idle && c.downStreak >= c.cfg.EffectiveScaleDownCycles() && serving > min && len(c.draining) == 0 {
		if !obs.Now.Before(c.lastAction.Add(c.cfg.EffectiveCooldown())) {
			if name, ok := c.highestActive(obs.Reviewers); ok {
				c.draining[name] = true
				c.downStreak = 0
				c.lastAction = obs.Now
				c.target = serving - 1
				return Decision{Action: ActionDrain, From: serving, To: serving - 1, Agent: name, Reason: fmt.Sprintf("idle for %d cycles", c.cfg.EffectiveScaleDownCycles())}
			}
		}
	}
	return Decision{Action: ActionHold, From: active, To: active, Reason: reason}
}

func (c *Controller) ageBacklog(obs Observation) {
	seen := make(map[string]bool, len(obs.WaitingKeys))
	for _, k := range obs.WaitingKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		seen[k] = true
		if _, ok := c.firstSeen[k]; !ok {
			c.firstSeen[k] = obs.Now
		}
	}
	for k := range c.firstSeen {
		if !seen[k] {
			delete(c.firstSeen, k)
		}
	}
	c.backlog = len(seen)
	c.oldest = 0
	for _, t := range c.firstSeen {
		if age := obs.Now.Sub(t); age > c.oldest {
			c.oldest = age
		}
	}
}

func (c *Controller) behind(serving int, dwell time.Duration) bool {
	if c.backlog == 0 {
		return false
	}
	if serving == 0 {
		return true
	}
	if c.backlog > c.cfg.EffectiveTargetBacklogPerReviewer()*serving {
		return true
	}
	return dwell > 0 && c.oldest > dwell
}

func (c *Controller) reason() string {
	if c.oldest > 0 {
		return fmt.Sprintf("backlog=%d oldest=%s", c.backlog, c.oldest.Round(time.Second))
	}
	return fmt.Sprintf("backlog=%d", c.backlog)
}

func (c *Controller) scaleUpGuard(obs Observation, active, serving, max int) string {
	if serving >= max || active >= max {
		return GuardAtMax
	}
	if obs.Now.Before(c.lastAction.Add(c.cfg.EffectiveCooldown())) {
		return GuardCooldown
	}
	if saturated(obs.Reviewers) {
		return GuardSaturated
	}
	if obs.ProviderBudgetExceeded {
		return GuardProviderBudget
	}
	if obs.ProviderHeadroomPct >= 0 && obs.ProviderHeadroomPct < MinProviderHeadroomPct {
		return GuardProviderHeadroom
	}
	if obs.MaxParallelReviews > 0 && active >= obs.MaxParallelReviews {
		return GuardMaxParallelReviews
	}
	if obs.Resources.Known {
		if obs.Resources.CPULimit > 0 && obs.Resources.FreeCPU() < c.cfg.EffectiveCPUPerReviewer() {
			return GuardCPU
		}
		if obs.Resources.FreeMemMB() < c.cfg.EffectiveMemPerReviewerMB() {
			return GuardMemory
		}
	}
	return ""
}

func saturated(reviewers []Reviewer) bool {
	if len(reviewers) == 0 {
		return false
	}
	n := 0
	for _, r := range reviewers {
		if r.Saturated {
			n++
		}
	}
	return float64(n) >= SaturationFraction*float64(len(reviewers))
}

func (c *Controller) pruneDraining(reviewers []Reviewer) {
	present := make(map[string]bool, len(reviewers))
	for _, r := range reviewers {
		present[r.Name] = true
	}
	for n := range c.draining {
		if !present[n] {
			delete(c.draining, n)
		}
	}
}

func (c *Controller) retirable(reviewers []Reviewer) (string, bool) {
	var best *Reviewer
	for i := range reviewers {
		r := &reviewers[i]
		if !c.draining[r.Name] || r.Busy {
			continue
		}
		if best == nil || r.Index > best.Index {
			best = r
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

func (c *Controller) highestDraining(reviewers []Reviewer) (string, bool) {
	return c.highest(reviewers, true)
}

func (c *Controller) highestActive(reviewers []Reviewer) (string, bool) {
	return c.highest(reviewers, false)
}

// highest picks the highest-index reviewer in the draining (or not) set,
// never the base replica (index 1), which is the lane's floor.
func (c *Controller) highest(reviewers []Reviewer, draining bool) (string, bool) {
	var best *Reviewer
	for i := range reviewers {
		r := &reviewers[i]
		if c.draining[r.Name] != draining || r.Index <= 1 {
			continue
		}
		if best == nil || r.Index > best.Index {
			best = r
		}
	}
	if best == nil {
		return "", false
	}
	return best.Name, true
}

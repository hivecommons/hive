package governor

import (
	"sort"

	"github.com/hivecommons/hive/pkg/config"
)

// Mode-unscheduled detection (#7474), the weaker sibling of NoCadenceAgents.
//
// NoCadenceAgents names an agent no mode schedules. This names an agent SOME
// mode schedules but the current one does not: enabled, governor-kickable,
// with a cadence in at least one mode, and nothing for the governor to resolve
// in the mode the fleet is in right now (its own entry, or the idle fallback
// every mode inherits). The live case: a reviewer whose only cadence is in
// surge. It is kicked every 30 minutes while the backlog holds the fleet in
// surge — and the reviewer's job is to shrink that backlog, so the moment it
// succeeds the fleet drops to busy, where it has no cadence, and it goes
// silent with the queue still far above where the operator wants it. The
// configuration is self-limiting, and nothing on the dashboard says so: the
// agent is enabled, running, has a cadence, is not on-demand, so every
// existing signal reports it healthy.
//
// An explicit pause/off entry in the current mode is NOT this: the governor
// resolved something, the operator chose it, and offByCadence already shows
// it. Nor is an agent no mode names at all — that is NoCadenceAgents' class.

// ModeUnscheduledAgent is one agent the current governor mode does not
// schedule, with the modes that do, so the operator is told which mode to
// add a cadence to rather than just that something is missing.
type ModeUnscheduledAgent struct {
	Agent string `json:"agent"`
	// Mode is the config key of the mode the fleet is in (lowercase, as the
	// cadence map spells it).
	Mode string `json:"mode"`
	// CadenceModes are the modes whose cadence map names the agent, in
	// threshold order — "surge" alone is the self-limiting shape.
	CadenceModes []string `json:"cadence_modes"`
}

// ModeUnscheduledAgents returns the enabled, governor-kickable agents that
// have a cadence in at least one mode but none the governor can resolve in
// the CURRENT mode (for a repo-scoped agent, in any repo's current mode).
// Sorted by agent name; non-nil even when empty, so it is always a
// measurement, never "not measured".
func (g *Governor) ModeUnscheduledAgents() []ModeUnscheduledAgent {
	g.mu.RLock()
	defer g.mu.RUnlock()

	out := []ModeUnscheduledAgent{}
	aggregateMode := modeToConfigKey(g.state.Mode)
	for name, ac := range g.agents {
		if !ac.Enabled || ac.OnDemand || !ac.UsesGovernorKick() {
			continue
		}
		if !g.agentHasAnyCadenceLocked(name) {
			continue // NoCadenceAgents' class, not this one
		}
		if g.agentScheduledNowLocked(name, aggregateMode) {
			continue
		}
		baseName := name
		if ac.ReplicaOf != "" {
			baseName = ac.ReplicaOf
		}
		out = append(out, ModeUnscheduledAgent{
			Agent:        name,
			Mode:         aggregateMode,
			CadenceModes: config.CadenceModesIn(g.cfg.Modes, name, baseName),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// agentScheduledNowLocked reports whether resolveCadence — the chain the
// governor actually kicks on — finds an entry for the agent in the mode the
// fleet is in. A repo-scoped agent follows each repo's own mode, so it is
// scheduled if ANY repo's mode resolves. Caller must hold g.mu.
func (g *Governor) agentScheduledNowLocked(agentName, aggregateMode string) bool {
	if !g.agentUsesRepoScope(agentName) {
		_, ok := g.resolveCadence(aggregateMode, agentName)
		return ok
	}
	for _, repoMode := range g.state.RepoModes {
		if _, ok := g.resolveCadence(modeToConfigKey(repoMode), agentName); ok {
			return true
		}
	}
	// No repo has a mode yet (first ticks after start): updateCadences gives
	// the agent nothing until repo modes exist, which is a startup transient,
	// not a configuration gap. Answer for the aggregate mode so the first
	// evaluation does not flag every repo-scoped agent.
	if len(g.state.RepoModes) == 0 {
		_, ok := g.resolveCadence(aggregateMode, agentName)
		return ok
	}
	return false
}

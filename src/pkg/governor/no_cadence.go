package governor

import (
	"sort"

	"github.com/hivecommons/hive/pkg/config"
)

// No-cadence detection (#5577). An agent that is enabled and governor-kickable
// but appears in NO mode's cadence map is never timer-kicked; if it has also
// never been kicked by any other path (LastKick empty forever), it will sit
// green and idle indefinitely while the dashboard's "not producing output"
// warnings name the symptom without the cause. This names the cause: the
// operator never configured a cadence.

// NoCadenceAgents returns the enabled, governor-kickable agents that have no
// cadence entry in ANY configured mode (an explicit "off"/"pause" entry counts
// as configured — that is operator choice, not omission) and have never been
// kicked at all. Sorted; empty when every agent is either scheduled, kicked,
// on-demand, event-driven, or disabled.
func (g *Governor) NoCadenceAgents() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// Non-nil even when empty: the governor's config is always loaded, so
	// this is always a measurement — the heartbeat must send [] (clearing the
	// hub's carried-forward value), never null ("not measured").
	out := []string{}
	for name, ac := range g.agents {
		if !ac.Enabled || ac.OnDemand || !ac.UsesGovernorKick() {
			continue
		}
		if g.agentHasAnyCadenceLocked(name) {
			continue
		}
		if lk, ok := g.state.LastKick[name]; ok && !lk.IsZero() {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// agentHasAnyCadenceLocked reports whether ANY mode's cadence map names this
// agent (directly or via its replica base, mirroring resolveCadence's
// fallback). Caller must hold g.mu.
//
// The predicate lives in config.HasAnyCadenceIn so the spoke dashboard's
// per-agent card signal (#5594) evaluates the SAME rule: a second copy here
// is how the fleet banner and the agent card would come to disagree.
func (g *Governor) agentHasAnyCadenceLocked(agentName string) bool {
	baseName := agentName
	if ac, ok := g.agents[agentName]; ok && ac.ReplicaOf != "" {
		baseName = ac.ReplicaOf
	}
	return config.HasAnyCadenceIn(g.cfg.Modes, agentName, baseName)
}

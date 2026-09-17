package config

import (
	"sort"
	"strings"
)

// CadenceValueForMode resolves an agent's Cadence for a governor mode, applying
// the same base-agent fallback the dashboard uses: an exact per-agent cadence
// wins, otherwise the cadence keyed on the agent's base name (rotation members
// share a base). Returns "" when the mode or agent has no cadence entry.
//
// This is the single source of truth shared by the spoke dashboard's
// offByCadence detection and the heartbeat's ExpectedActive computation, so the
// two can never disagree about whether the governor will kick an agent.
func (c *Config) CadenceValueForMode(agentName, modeName string) Cadence {
	if mode, ok := c.Governor.Modes[modeName]; ok {
		if cad, ok := mode.Cadences[agentName]; ok {
			return cad
		}
		if base := c.BaseAgentName(agentName); base != agentName {
			if cad, ok := mode.Cadences[base]; ok {
				return cad
			}
		}
	}
	return ""
}

// HasAnyCadenceIn reports whether ANY mode in modes carries a cadence entry for
// agentName — directly, or under baseName (pass the agent's replica base, or
// agentName itself when there is none), mirroring CadenceValueForMode's
// fallback. An explicit "off"/"pause" entry COUNTS as configured: that is an
// operator choice, not an omission. False therefore means "no mode names this
// agent at all", i.e. the governor will never timer-kick it in any mode.
//
// Shared by the governor's NoCadenceAgents fleet detector (#5577) and the
// dashboard's per-agent noCadence card signal (#5594) so the fleet banner and
// the agent card can never disagree about which agents are unscheduled.
func HasAnyCadenceIn(modes map[string]ModeConfig, agentName, baseName string) bool {
	for _, mode := range modes {
		if _, ok := mode.Cadences[agentName]; ok {
			return true
		}
		if baseName != "" && baseName != agentName {
			if _, ok := mode.Cadences[baseName]; ok {
				return true
			}
		}
	}
	return false
}

// HasAnyCadence is HasAnyCadenceIn for this config's governor modes, resolving
// the agent's replica base itself.
func (c *Config) HasAnyCadence(agentName string) bool {
	if c == nil {
		// "cannot tell", not "unscheduled" — the dashboard turns a false here
		// into an operator-facing warning, and a missing config must not
		// accuse every agent of never being scheduled.
		return true
	}
	return HasAnyCadenceIn(c.Governor.Modes, agentName, c.BaseAgentName(agentName))
}

// idleModeKey is the mode whose cadence entries serve as every other mode's
// fallback — the governor's resolveCadence chain is current-mode entry →
// idle-mode entry → none. Kept in step with the governor's own constant.
const idleModeKey = "idle"

// ModeSchedulesIn reports whether the governor would find a cadence entry for
// agentName in modeName, following the SAME chain the governor's
// resolveCadence follows: the mode's own entry (agent, then baseName), else
// the idle mode's entry (agent, then baseName). An explicit pause/off entry
// counts — the governor found something, and offByCadence is the signal for
// that — so false means exactly "nothing schedules this agent while the fleet
// is in modeName", however many OTHER modes name it.
//
// Shared by the governor's ModeUnscheduledAgents detector and the dashboard's
// per-agent card signal (#7474) for the same reason HasAnyCadenceIn is: a
// second copy is how the fleet banner and the agent card would come to
// disagree. modeName is matched case-insensitively (mode keys are lowercase;
// the governor's Mode strings are not).
func ModeSchedulesIn(modes map[string]ModeConfig, modeName, agentName, baseName string) bool {
	modeName = strings.ToLower(strings.TrimSpace(modeName))
	has := func(key string) bool {
		mode, ok := modes[key]
		if !ok {
			return false
		}
		if _, ok := mode.Cadences[agentName]; ok {
			return true
		}
		if baseName != "" && baseName != agentName {
			if _, ok := mode.Cadences[baseName]; ok {
				return true
			}
		}
		return false
	}
	if has(modeName) {
		return true
	}
	return modeName != idleModeKey && has(idleModeKey)
}

// CadenceModesIn lists the modes whose cadence map names agentName (directly
// or under baseName), ordered by threshold ascending — idle, quiet, busy,
// surge in a stock config — so a message can say "only in surge" or
// "in busy and surge". Empty when HasAnyCadenceIn would be false.
func CadenceModesIn(modes map[string]ModeConfig, agentName, baseName string) []string {
	out := []string{}
	for key, mode := range modes {
		if _, ok := mode.Cadences[agentName]; ok {
			out = append(out, key)
			continue
		}
		if baseName != "" && baseName != agentName {
			if _, ok := mode.Cadences[baseName]; ok {
				out = append(out, key)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := modes[out[i]].Threshold, modes[out[j]].Threshold
		if ti != tj {
			return ti < tj
		}
		return out[i] < out[j]
	})
	return out
}

// ModeSchedules is ModeSchedulesIn for this config's governor modes, resolving
// the agent's replica base itself. A nil config reports true: "cannot tell"
// must not become "unscheduled" on the operator's screen.
func (c *Config) ModeSchedules(agentName, modeName string) bool {
	if c == nil {
		return true
	}
	return ModeSchedulesIn(c.Governor.Modes, modeName, agentName, c.BaseAgentName(agentName))
}

// CadenceModes is CadenceModesIn for this config's governor modes.
func (c *Config) CadenceModes(agentName string) []string {
	if c == nil {
		return []string{}
	}
	return CadenceModesIn(c.Governor.Modes, agentName, c.BaseAgentName(agentName))
}

// ExpectedActive reports whether the governor's CURRENT mode schedules this
// agent on a kicking cadence right now — i.e. the governor is expected to be
// driving it. It is the inverse of the dashboard's offByCadence: false when the
// mode's cadence is a non-kicking value (pause/off/0/"on demand"), when the
// agent is on-demand (never on a schedule), or when the agent is on-demand by
// pack default. onDemandAgent is the agent's own OnDemand config flag;
// onDemandFromPack is the ACMM-pack on-demand set (see OnDemandAgentsFromPacks).
//
// modeName is matched case-insensitively against the config's mode keys, which
// are lowercase (idle/quiet/busy/surge); callers holding an upper-cased
// governor mode string need not pre-lowercase.
func (c *Config) ExpectedActive(agentName, modeName string, onDemandAgent bool, onDemandFromPack map[string]bool) bool {
	if onDemandAgent || onDemandFromPack[agentName] {
		return false
	}
	cad := c.CadenceValueForMode(agentName, strings.ToLower(strings.TrimSpace(modeName)))
	if cad == "" {
		// No cadence entry for this mode: the agent is not scheduled to kick in
		// this mode, so it is not expected active.
		return false
	}
	return !cad.IsPaused()
}

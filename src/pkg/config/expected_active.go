package config

import "strings"

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

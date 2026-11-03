package config

import (
	"os"
	"strings"
)

// ValidExplainModes are the accepted explain_mode values. "" means "inherit the
// hive-wide default"; it is valid on an agent but is not a mode in itself.
var ValidExplainModes = map[string]bool{
	"":               true,
	ExplainModeOff:   true,
	ExplainModeBrief: true,
	ExplainModeFull:  true,
}

// ValidateExplainMode reports whether v is an accepted explain_mode value.
func ValidateExplainMode(v string) bool { return ValidExplainModes[v] }

// Explain-mode default sources reported by ExplainModeDefaultSource. Both are
// safe to show in the dashboard and to log.
const (
	// ExplainModeSourceConfig means governor.explain_mode set the default.
	ExplainModeSourceConfig = "config"
	// ExplainModeSourceEnv means the ExplainModeEnvVar environment variable
	// set the default because governor.explain_mode is unset.
	ExplainModeSourceEnv = "env:" + ExplainModeEnvVar
)

// ResolveExplainModeDefault returns the hive-wide default explain mode applied
// to agents that leave explain_mode unset.
//
// Governor config wins over the environment, with the env var kept as the
// fallback for hives that already set it — the same precedence, and for the
// same reason, as the backup encryption key (see BackupConfig.ResolveKey).
// HIVE_EXPLAIN_MODE is set on the DEPLOYMENT, and a hosted spoke owner has no
// deployment-env access, so a knob that lives only in the environment is
// unreachable for exactly the operators who go looking for it and find nothing
// (#4712). Putting it in governor config puts it in the dashboard's Settings →
// Governor form, next to the other hive-wide agent defaults.
//
// An unrecognized value in either place resolves to off, matching
// resolveExplainMode's rule that a typo degrades to today's behaviour rather
// than to a mode nobody asked for.
func (g GovernorConfig) ResolveExplainModeDefault() string {
	mode, _ := g.resolveExplainModeDefault()
	return mode
}

// ExplainModeDefaultSource reports WHERE the hive-wide default came from —
// ExplainModeSourceConfig, ExplainModeSourceEnv, or "" when neither sets one
// (so agents fall through to off). The dashboard shows this so an operator can
// tell a default they set from one the deployment set for them, which is the
// question that produced #4712 in the first place.
func (g GovernorConfig) ExplainModeDefaultSource() string {
	_, source := g.resolveExplainModeDefault()
	return source
}

func (g GovernorConfig) resolveExplainModeDefault() (mode, source string) {
	if v := strings.TrimSpace(g.ExplainMode); v != "" {
		return normalizeExplainMode(v), ExplainModeSourceConfig
	}
	if v := strings.TrimSpace(os.Getenv(ExplainModeEnvVar)); v != "" {
		return normalizeExplainMode(v), ExplainModeSourceEnv
	}
	return ExplainModeOff, ""
}

// normalizeExplainMode maps any value to one the kick path can act on,
// collapsing typos and unknown modes to off.
func normalizeExplainMode(v string) string {
	switch v {
	case ExplainModeBrief, ExplainModeFull:
		return v
	default:
		return ExplainModeOff
	}
}

// ValidCavemanModes are the accepted caveman_mode values. "" means "caveman
// disabled"; it is valid on an agent but is not a mode in itself.
var ValidCavemanModes = map[string]bool{
	"":       true,
	"lite":   true,
	"full":   true,
	"ultra":  true,
	"wenyan": true,
}

// ValidateCavemanMode reports whether v is an accepted caveman_mode value.
func ValidateCavemanMode(v string) bool { return ValidCavemanModes[v] }

// ValidJevModes are the accepted jev_mode values. "" is the same as "off".
var ValidJevModes = map[string]bool{
	"":            true,
	JevModeOff:    true,
	JevModeAssist: true,
}

// ValidateJevMode reports whether v is an accepted jev_mode value.
func ValidateJevMode(v string) bool { return ValidJevModes[v] }

// JevAssistEnabled reports whether the named agent has jev_mode: assist. It
// reads the live config so a dashboard toggle takes effect on the agent's
// next decision call, mirroring IsRepoPaused / AgentServesRepo.
func (c *Config) JevAssistEnabled(agent string) bool {
	if c == nil {
		return false
	}
	a, ok := c.Agents[agent]
	return ok && a.JevEnabled()
}

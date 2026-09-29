package config

import (
	"os"
	"strings"
)

// Agent explain modes (AgentConfig.ExplainMode / HIVE_EXPLAIN_MODE).
//
// Agents are told to act rather than narrate — see the "Output Rules — Terse
// Mode" block in every agent policy and the EXECUTE, DO NOT NARRATE suffix the
// agent manager appends on inference backends. That instruction exists to stop
// weak models answering a kick with a plan for someone else to run instead of
// running it, which is a real, observed failure and must be preserved. The cost
// of preserving it is that an operator debugging an agent has no visibility
// into WHY it chose what it chose (#3887, split from #3878).
//
// Explain mode buys that visibility back without relaxing the rule: the agent
// still acts first, and the explanation rides ALONGSIDE the tool calls on
// EXPLAIN-prefixed lines rather than replacing them.
const (
	// ExplainModeOff disables explanation. As an explicit per-agent value it
	// also overrides a hive-wide default, which "" (unset) does not.
	ExplainModeOff = "off"
	// ExplainModeBrief asks for one short EXPLAIN line per tool call.
	ExplainModeBrief = "brief"
	// ExplainModeFull adds an end-of-turn EXPLAIN block covering the goal as
	// understood, alternatives rejected, and what would change the decision.
	ExplainModeFull = "full"
)

// ValidExplainModes are the accepted explain_mode values. "" means "inherit the
// hive-wide default"; it is valid on an agent but is not a mode in itself.
var ValidExplainModes = map[string]bool{
	"":               true,
	ExplainModeOff:   true,
	ExplainModeBrief: true,
	ExplainModeFull:  true,
}

// ExplainModeEnvVar is the deployment environment variable holding the
// hive-wide default for agents that leave explain_mode unset. It lets an
// operator debugging a misbehaving fleet turn explanation on for every agent at
// once without editing (and later having to unedit) each agent's config — the
// same shape as HIVE_TMUX_HISTORY_LIMIT and HIVE_TMUX_PANE_WIDTH.
//
// It remains supported, but it is no longer the ONLY place the default lives:
// see GovernorConfig.ExplainMode. Hive also injects the RESOLVED mode into
// every agent process under this same name.
const ExplainModeEnvVar = "HIVE_EXPLAIN_MODE"

// ExplainLinePrefix marks a line the agent emitted as debugging explanation
// rather than as ordinary output. It is the whole reason explanation can be a
// SEPARATE stream without new capture infrastructure: agent logs are tmux pane
// scrapes, so there is no second channel to write to, but a stable, greppable
// prefix lets the full-log endpoint (and `grep`) split explanation from work
// after the fact. Chosen to be ASCII, unlikely to collide with tool output, and
// stable — the filter and the prompt must agree on it forever.
const ExplainLinePrefix = "EXPLAIN:"

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

// Jev per-agent modes (hivecommons/hive#8939). "" and "off" both mean "no Jev
// tool installed, no Jev network calls" — the default. "assist" installs the
// jev_decide skill into the agent's CLI home and lets the agent call the
// hive's Jev decision endpoint for quick typed judgments.
const (
	JevModeOff    = "off"
	JevModeAssist = "assist"
)

// ValidJevModes are the accepted jev_mode values. "" is the same as "off".
var ValidJevModes = map[string]bool{
	"":            true,
	JevModeOff:    true,
	JevModeAssist: true,
}

// ValidateJevMode reports whether v is an accepted jev_mode value.
func ValidateJevMode(v string) bool { return ValidJevModes[v] }

// JevEnabled reports whether this agent may call the Jev decision tool.
func (a AgentConfig) JevEnabled() bool { return a.JevMode == JevModeAssist }

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

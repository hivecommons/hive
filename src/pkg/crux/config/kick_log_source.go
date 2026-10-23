package config

import (
	"os"
	"strings"
)

const (
	// KickLogSourceScrollback is the tmux capture-pane path (default).
	KickLogSourceScrollback = "scrollback"
	// KickLogSourceTranscript renders the backend CLI's own transcript,
	// falling back to scrollback (with a warning) when none is usable.
	KickLogSourceTranscript = "transcript"
	// KickLogSourceAuto renders the transcript when the backend has a reader
	// and the transcript exists and parses, else scrollback, silently.
	KickLogSourceAuto = "auto"
	// KickLogSourceEnvVar overrides governor.logging.kick_log_source for one
	// process. The name predates the config key (#11799) and is kept.
	KickLogSourceEnvVar = "HIVE_KICK_LOG_SOURCE"
)

// NormalizeKickLogSource maps any value to one the kick path can act on:
// a known source lower-cased, anything else KickLogSourceScrollback.
func NormalizeKickLogSource(s string) string {
	v, _ := normalizeKickLogSource(s)
	return v
}

func normalizeKickLogSource(s string) (string, bool) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case KickLogSourceScrollback, KickLogSourceTranscript, KickLogSourceAuto:
		return v, true
	default:
		return KickLogSourceScrollback, false
	}
}

// Kick log source (hivecommons/hive#11762, #9579).
//
// A kick log is normally the tmux scrollback the backend CLI painted; #11799
// added the option to render the CLI's own transcript instead, selected by
// HIVE_KICK_LOG_SOURCE. That env var is set on the deployment, which a hosted
// spoke owner cannot reach, so the same choice is also governor.logging.
// kick_log_source, editable under Settings → Logging → Kick logs.
//
// Precedence: a valid HIVE_KICK_LOG_SOURCE wins (the one-step rollback, and
// the dashboard shows the select locked with a note when it does), then the
// config key, then KickLogSourceScrollback. An unknown value in either place
// is ignored rather than failing load, so a typo degrades to today's
// behaviour instead of to a mode nobody asked for.
const (

	// KickLogSourceOriginEnv, KickLogSourceOriginConfig and
	// KickLogSourceOriginDefault name where the effective source came from.
	KickLogSourceOriginEnv     = "env"
	KickLogSourceOriginConfig  = "config"
	KickLogSourceOriginDefault = "default"
)

// KickLogSources lists the accepted kick_log_source values in display order.
func KickLogSources() []string {
	return []string{KickLogSourceScrollback, KickLogSourceTranscript, KickLogSourceAuto}
}

// ValidKickLogSource reports whether s (case-insensitive, trimmed) is one of
// KickLogSources.
func ValidKickLogSource(s string) bool {
	_, ok := normalizeKickLogSource(s)
	return ok
}

// KickLogSourceEnvOverride returns the valid HIVE_KICK_LOG_SOURCE value in
// force, or "" when the variable is unset or unrecognised.
func KickLogSourceEnvOverride() string {
	if v, ok := normalizeKickLogSource(os.Getenv(KickLogSourceEnvVar)); ok {
		return v
	}
	return ""
}

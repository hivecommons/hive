package config

// SessionTaintConfig gates session taint (hivecommons/hive#18987,
// docs/design/session-taint.md, phase 0). Absent or `enabled: false` means the
// feature is not built at all: nothing is recorded, no lookup is made and every
// request and action behaves exactly as before.
//
// Phase 0 is OBSERVE-ONLY. With `enabled: true` the hive keeps a bounded,
// in-memory record of which coarse labels each agent session has accrued and
// which would-be verdicts a small fixed rule table gives to the agent's later
// relay writes. It writes those to the log and the audit log and never blocks,
// holds, restricts or alters anything. No credential or content value is ever
// stored: labels, counts and ids only. A change takes effect at the next
// restart, and a restart begins every session clean.
type SessionTaintConfig struct {
	// Enabled turns the observer on. The default (false) is off.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxSessions bounds how many agent sessions are tracked at once; the
	// least recently touched is evicted past it. Zero means
	// DefaultSessionTaintMaxSessions.
	MaxSessions int `yaml:"max_sessions,omitempty" json:"max_sessions,omitempty"`
	// MaxLabelsPerSession bounds the labels one session holds. Zero means
	// DefaultSessionTaintMaxLabelsPerSession.
	MaxLabelsPerSession int `yaml:"max_labels_per_session,omitempty" json:"max_labels_per_session,omitempty"`
	// MaxDecisions bounds the ring of recent would-act decisions kept in
	// memory. Zero means DefaultSessionTaintMaxDecisions.
	MaxDecisions int `yaml:"max_decisions,omitempty" json:"max_decisions,omitempty"`
}

// The defaults mirror the observer's own bounds (crux/sessiontaint) so a hive
// that sets only `enabled: true` gets the documented limits.
const (
	DefaultSessionTaintMaxSessions         = 256
	DefaultSessionTaintMaxLabelsPerSession = 32
	DefaultSessionTaintMaxDecisions        = 128
)

// MaxSessionsOrDefault resolves MaxSessions; a non-positive value means the default.
func (c SessionTaintConfig) MaxSessionsOrDefault() int {
	if c.MaxSessions <= 0 {
		return DefaultSessionTaintMaxSessions
	}
	return c.MaxSessions
}

// MaxLabelsPerSessionOrDefault resolves MaxLabelsPerSession.
func (c SessionTaintConfig) MaxLabelsPerSessionOrDefault() int {
	if c.MaxLabelsPerSession <= 0 {
		return DefaultSessionTaintMaxLabelsPerSession
	}
	return c.MaxLabelsPerSession
}

// MaxDecisionsOrDefault resolves MaxDecisions.
func (c SessionTaintConfig) MaxDecisionsOrDefault() int {
	if c.MaxDecisions <= 0 {
		return DefaultSessionTaintMaxDecisions
	}
	return c.MaxDecisions
}

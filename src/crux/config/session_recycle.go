package config

import "time"

// Session-recycle defaults (#14842). A CLI session's V8 heap grows with
// every turn and never shrinks on idle, so an agent that keeps working
// climbs past a gigabyte within hours and the pod eventually OOMs. The
// recycle policy ends an idle session once it is over an RSS or age budget
// and relaunches it through the ordinary restart path.
const (
	DefaultSessionRecycleMaxRSSMB = 1536
	DefaultSessionRecycleMaxAge   = 6 * time.Hour
	DefaultSessionRecycleMinIdle  = 2 * time.Minute
	// SessionRecycleBackoff is how long after a recycle the same agent is
	// left alone, even if it is immediately over budget again.
	SessionRecycleBackoff = 10 * time.Minute
)

// SessionRecycleConfig bounds the resident memory and age of an agent's CLI
// session. It is written fleet-wide under agents.session_recycle and may be
// overridden per agent under agents.<name>.session_recycle. Zero fields take
// the defaults; the policy is on unless Enabled is explicitly false.
type SessionRecycleConfig struct {
	// Enabled turns the policy off when explicitly false. Nil means on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxRSSMB is the resident-set budget for the whole CLI process tree
	// under the agent's tmux pane, in MiB. 0 means the default (1536).
	MaxRSSMB int `yaml:"max_rss_mb,omitempty" json:"max_rss_mb,omitempty"`
	// MaxAge recycles a session older than this even if under the RSS
	// budget. 0 means the default (6h).
	MaxAge time.Duration `yaml:"max_age,omitempty" json:"max_age,omitempty"`
	// MinIdle is how long the pane and transcript must have been silent at
	// the input prompt before a recycle is allowed. 0 means the default (2m).
	MinIdle time.Duration `yaml:"min_idle,omitempty" json:"min_idle,omitempty"`
}

// IsZero reports whether no field was set, so an absent block encodes as
// nothing and a per-agent nil pointer inherits the fleet value.
func (c SessionRecycleConfig) IsZero() bool {
	return c.Enabled == nil && c.MaxRSSMB == 0 && c.MaxAge == 0 && c.MinIdle == 0
}

// Effective returns the policy with defaults filled in. The receiver may be
// nil (an agent with no override and no fleet block), which yields the
// defaults with the policy enabled.
func (c *SessionRecycleConfig) Effective() SessionRecycleConfig {
	out := SessionRecycleConfig{}
	if c != nil {
		out = *c
	}
	if out.MaxRSSMB <= 0 {
		out.MaxRSSMB = DefaultSessionRecycleMaxRSSMB
	}
	if out.MaxAge <= 0 {
		out.MaxAge = DefaultSessionRecycleMaxAge
	}
	if out.MinIdle <= 0 {
		out.MinIdle = DefaultSessionRecycleMinIdle
	}
	if out.Enabled == nil {
		on := true
		out.Enabled = &on
	}
	return out
}

// IsEnabled is Effective().Enabled as a plain bool.
func (c *SessionRecycleConfig) IsEnabled() bool {
	e := c.Effective()
	return e.Enabled != nil && *e.Enabled
}

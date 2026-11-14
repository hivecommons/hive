package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Defaults and bounds for the dashboard Sessions feature (interactive, named,
// shared CLI terminals). Every default is overridable by an env var so an
// operator can tune a spoke without editing hive.yaml.
const (
	// EnvSessionsEnabled overrides dashboard.sessions.enabled when set to a
	// recognised boolean ("1/true/yes/on" or "0/false/no/off").
	EnvSessionsEnabled = "HIVE_SESSIONS_ENABLED"
	// EnvSessionsMax, EnvSessionsMaxPerUser and EnvSessionsIdleTTLHours
	// override the corresponding dashboard.sessions limits.
	EnvSessionsMax          = "HIVE_SESSIONS_MAX"
	EnvSessionsMaxPerUser   = "HIVE_SESSIONS_MAX_PER_USER"
	EnvSessionsIdleTTLHours = "HIVE_SESSIONS_IDLE_TTL_HOURS"

	DefaultSessionsMax          = 12
	DefaultSessionsMaxPerUser   = 4
	DefaultSessionsIdleTTLHours = 72
)

// DashboardSessionsConfig is dashboard.sessions: named, shared CLI-agent
// terminal sessions inside the dashboard. Off by default.
type DashboardSessionsConfig struct {
	// Enabled turns the Sessions feature on. HIVE_SESSIONS_ENABLED overrides it.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxSessions caps live (running) sessions per spoke. 0 = default (12).
	MaxSessions int `yaml:"max_sessions,omitempty" json:"max_sessions,omitempty"`
	// MaxPerUser caps live sessions one user may create. 0 = default (4).
	MaxPerUser int `yaml:"max_per_user,omitempty" json:"max_per_user,omitempty"`
	// IdleTTLHours closes a session with no input and no attached client for
	// this many hours. 0 = default (72).
	IdleTTLHours int `yaml:"idle_ttl_hours,omitempty" json:"idle_ttl_hours,omitempty"`
}

// Validate rejects negative limits.
func (s DashboardSessionsConfig) Validate() error {
	for name, v := range map[string]int{
		"max_sessions":   s.MaxSessions,
		"max_per_user":   s.MaxPerUser,
		"idle_ttl_hours": s.IdleTTLHours,
	} {
		if v < 0 {
			return fmt.Errorf("dashboard.sessions.%s must not be negative (got %d)", name, v)
		}
	}
	return nil
}

// SessionsEnabled reports whether the Sessions feature is on: the
// HIVE_SESSIONS_ENABLED env wins over dashboard.sessions.enabled when it holds
// a recognised boolean.
func (c *Config) SessionsEnabled() bool {
	if v, ok := parseBoolEnv(EnvSessionsEnabled); ok {
		return v
	}
	return c != nil && c.Dashboard.Sessions.Enabled
}

// SessionsMax returns the per-spoke live-session cap.
func (c *Config) SessionsMax() int {
	var configured int
	if c != nil {
		configured = c.Dashboard.Sessions.MaxSessions
	}
	return sessionsLimit(EnvSessionsMax, configured, DefaultSessionsMax)
}

// SessionsMaxPerUser returns the per-user live-session cap.
func (c *Config) SessionsMaxPerUser() int {
	var configured int
	if c != nil {
		configured = c.Dashboard.Sessions.MaxPerUser
	}
	return sessionsLimit(EnvSessionsMaxPerUser, configured, DefaultSessionsMaxPerUser)
}

// SessionsIdleTTLHours returns the idle-close threshold in hours.
func (c *Config) SessionsIdleTTLHours() int {
	var configured int
	if c != nil {
		configured = c.Dashboard.Sessions.IdleTTLHours
	}
	return sessionsLimit(EnvSessionsIdleTTLHours, configured, DefaultSessionsIdleTTLHours)
}

// sessionsLimit resolves env > config > default; non-positive values fall
// through to the next source.
func sessionsLimit(env string, configured, def int) int {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if configured > 0 {
		return configured
	}
	return def
}

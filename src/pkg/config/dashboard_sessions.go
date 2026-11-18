package config

import (
	"fmt"
	"os"
	"regexp"
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
	// EnvSessionsWorkspaceRepo overrides dashboard.sessions.workspace_repo.
	EnvSessionsWorkspaceRepo = "HIVE_SESSIONS_WORKSPACE_REPO"
	// EnvSessionsAutostart overrides dashboard.sessions.autostart when set to
	// a recognised boolean.
	EnvSessionsAutostart = "HIVE_SESSIONS_AUTOSTART"

	// DefaultSessionsEnabled is dashboard.sessions.enabled when hive.yaml
	// leaves it unset: Inception (spek) sessions are on unless an operator
	// turns them off (enabled: false or HIVE_SESSIONS_ENABLED=false).
	DefaultSessionsEnabled = true

	// DefaultSessionsAutostart is dashboard.sessions.autostart when hive.yaml
	// leaves it unset: a new session's CLI is taken through its startup
	// screens to its input prompt without a person.
	DefaultSessionsAutostart = true

	DefaultSessionsMax          = 12
	DefaultSessionsMaxPerUser   = 4
	DefaultSessionsIdleTTLHours = 72
)

// sessionsWorkspaceRepoPattern is the "org/repo" shape the workspace repo
// must have (one GitHub owner, one repository name).
var sessionsWorkspaceRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*/[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// DashboardSessionsConfig is dashboard.sessions: named, shared CLI-agent
// terminal sessions inside the dashboard, shown as "Inception (spek)". On by
// default.
type DashboardSessionsConfig struct {
	// Enabled turns the Sessions feature on or off. Nil (unset) means
	// DefaultSessionsEnabled; a pointer so an explicit false survives a save.
	// HIVE_SESSIONS_ENABLED overrides it.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxSessions caps live (running) sessions per spoke. 0 = default (12).
	MaxSessions int `yaml:"max_sessions,omitempty" json:"max_sessions,omitempty"`
	// MaxPerUser caps live sessions one user may create. 0 = default (4).
	MaxPerUser int `yaml:"max_per_user,omitempty" json:"max_per_user,omitempty"`
	// IdleTTLHours closes a session with no input and no attached client for
	// this many hours. 0 = default (72).
	IdleTTLHours int `yaml:"idle_ttl_hours,omitempty" json:"idle_ttl_hours,omitempty"`
	// WorkspaceRepo is the "org/repo" every session opens in: a persistent
	// per-hive clone, shared by all sessions, that `spektacular init` has run
	// in. Empty means the hive's own project repo (project.org/primary_repo).
	// HIVE_SESSIONS_WORKSPACE_REPO overrides it.
	WorkspaceRepo string `yaml:"workspace_repo,omitempty" json:"workspace_repo,omitempty"`
	// Autostart answers the vendor CLI's known startup screens (first-run
	// notice, trust for the session's own directory, onboarding) when a
	// session starts, so its first request runs without a person. It never
	// answers a permission prompt or a login screen. Nil (unset) means
	// DefaultSessionsAutostart; HIVE_SESSIONS_AUTOSTART overrides it.
	Autostart *bool `yaml:"autostart,omitempty" json:"autostart,omitempty"`
}

// Validate rejects negative limits and a workspace repo that is not org/repo.
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
	if repo := strings.TrimSpace(s.WorkspaceRepo); repo != "" && !sessionsWorkspaceRepoPattern.MatchString(repo) {
		return fmt.Errorf("dashboard.sessions.workspace_repo must be org/repo (got %q)", s.WorkspaceRepo)
	}
	return nil
}

// SessionsWorkspaceRepo returns the "org/repo" sessions open in: the env
// override, then dashboard.sessions.workspace_repo, then the hive's project
// repo (project.org + project.primary_repo). "" when none is set or the value
// is not org/repo, in which case sessions open in their own empty work dir.
func (c *Config) SessionsWorkspaceRepo() string {
	candidates := []string{strings.TrimSpace(os.Getenv(EnvSessionsWorkspaceRepo))}
	if c != nil {
		candidates = append(candidates, strings.TrimSpace(c.Dashboard.Sessions.WorkspaceRepo))
		if org, repo := strings.TrimSpace(c.Project.Org), strings.TrimSpace(c.Project.PrimaryRepo); org != "" && repo != "" {
			candidates = append(candidates, org+"/"+repo)
		}
	}
	for _, repo := range candidates {
		if repo == "" {
			continue
		}
		if sessionsWorkspaceRepoPattern.MatchString(repo) {
			return repo
		}
		return ""
	}
	return ""
}

// SessionsEnabled reports whether the Sessions feature is on: the
// HIVE_SESSIONS_ENABLED env wins over dashboard.sessions.enabled when it holds
// a recognised boolean.
func (c *Config) SessionsEnabled() bool {
	if v, ok := parseBoolEnv(EnvSessionsEnabled); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.Dashboard.Sessions.EnabledOrDefault()
}

// SessionsAutostart reports whether a starting session's CLI is taken through
// its startup screens automatically: HIVE_SESSIONS_AUTOSTART wins over
// dashboard.sessions.autostart when it holds a recognised boolean.
func (c *Config) SessionsAutostart() bool {
	if v, ok := parseBoolEnv(EnvSessionsAutostart); ok {
		return v
	}
	if c == nil {
		return DefaultSessionsAutostart
	}
	return c.Dashboard.Sessions.AutostartOrDefault()
}

// AutostartOrDefault is the saved dashboard.sessions.autostart value, or
// DefaultSessionsAutostart when hive.yaml leaves it unset. It ignores
// HIVE_SESSIONS_AUTOSTART (see Config.SessionsAutostart).
func (s DashboardSessionsConfig) AutostartOrDefault() bool {
	if s.Autostart == nil {
		return DefaultSessionsAutostart
	}
	return *s.Autostart
}

// EnabledOrDefault is the saved dashboard.sessions.enabled value, or
// DefaultSessionsEnabled when hive.yaml leaves it unset. It ignores
// HIVE_SESSIONS_ENABLED (see Config.SessionsEnabled).
func (s DashboardSessionsConfig) EnabledOrDefault() bool {
	if s.Enabled == nil {
		return DefaultSessionsEnabled
	}
	return *s.Enabled
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

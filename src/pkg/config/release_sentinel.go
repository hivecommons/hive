package config

import (
	"strings"
	"time"
)

// ReleaseSentinelEnabledEnvVar overrides release_sentinel.enabled for one
// process ("true"/"false", "1"/"0", "on"/"off", "yes"/"no"). Unset or
// unparseable leaves the config value in charge.
const ReleaseSentinelEnabledEnvVar = "HIVE_RELEASE_SENTINEL_ENABLED"

// ReleaseSentinelConfig is the operator opt-in for the release sentinel
// (hivecommons/hive#9585): a bounded repair loop that watches the CI of the
// current v<version> release tag and dispatches repair rounds to an agent
// when it fails. The zero value keeps it OFF: nothing is watched, dispatched
// or persisted.
//
// Duration fields accept Go duration strings ("2h", "90m"); empty, invalid
// or non-positive values fall back to the package defaults in
// pkg/releasesentinel.
type ReleaseSentinelConfig struct {
	// Enabled turns the sentinel on. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repo is the "owner/name" whose release tags are watched. Empty means
	// the project's primary repo.
	Repo string `yaml:"repo,omitempty" json:"repo,omitempty"`
	// Agent is the lane repair rounds are dispatched to. Empty means
	// ci-maintainer.
	Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
	// MaxRounds caps repair rounds per release. 0 means 5.
	MaxRounds int `yaml:"max_rounds,omitempty" json:"max_rounds,omitempty"`
	// RoundTimeout bounds one repair round. "" means 2h.
	RoundTimeout string `yaml:"round_timeout,omitempty" json:"round_timeout,omitempty"`
	// PollInterval is the minimum time between sentinel passes; it rides the
	// governor eval tick and self-gates to this. "" means 5m.
	PollInterval string `yaml:"poll_interval,omitempty" json:"poll_interval,omitempty"`
	// IgnoreWorkflows names workflows whose runs never block a release
	// (advisory bots and similar noise).
	IgnoreWorkflows []string `yaml:"ignore_workflows,omitempty" json:"ignore_workflows,omitempty"`
}

// ReleaseSentinelEnabled resolves the opt-in: the env override wins, then the
// config field. Nil-safe; a nil config is off.
func (c *Config) ReleaseSentinelEnabled() bool {
	if v, ok := parseBoolEnv(ReleaseSentinelEnabledEnvVar); ok {
		return v
	}
	if c == nil {
		return false
	}
	return c.ReleaseSentinel.Enabled
}

// ReleaseSentinelRepo is the "owner/name" the sentinel watches: the explicit
// release_sentinel.repo, else project.primary_repo (qualified with
// project.org when it is a bare name). Empty when neither is usable.
func (c *Config) ReleaseSentinelRepo() string {
	if c == nil {
		return ""
	}
	if r := strings.TrimSpace(c.ReleaseSentinel.Repo); r != "" {
		return r
	}
	primary := strings.TrimSpace(c.Project.PrimaryRepo)
	if primary == "" || strings.Contains(primary, "/") {
		return primary
	}
	if org := strings.TrimSpace(c.Project.Org); org != "" {
		return org + "/" + primary
	}
	return ""
}

// RoundTimeoutDuration parses RoundTimeout; 0 means "use the default".
func (r ReleaseSentinelConfig) RoundTimeoutDuration() time.Duration {
	return positiveDuration(r.RoundTimeout)
}

// PollIntervalDuration parses PollInterval; 0 means "use the default".
func (r ReleaseSentinelConfig) PollIntervalDuration() time.Duration {
	return positiveDuration(r.PollInterval)
}

func positiveDuration(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

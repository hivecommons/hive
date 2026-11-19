package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Release-feed watcher (hivecommons/hive#18494). governor.release_feeds lists,
// per agent, the upstream feeds whose new versions should kick that agent at
// once instead of waiting for its timer: GitHub releases or tags of an
// owner/repo, a PyPI package, or a generic index URL. The watcher polls each
// feed on ReleaseFeedInterval with conditional requests, persists the last
// version it saw, and kicks the subscribed agent through the kick queue with a
// ${NEW_RELEASES} block. A kick per agent is bounded by its cooldown.

// Release-feed source kinds.
const (
	ReleaseFeedKindGitHubRelease = "github_release"
	ReleaseFeedKindGitHubTag     = "github_tag"
	ReleaseFeedKindPyPI          = "pypi"
	ReleaseFeedKindIndex         = "index"
)

// Release-feed timing defaults. Each is env-overridable (a Go duration such as
// "10m"); an explicit hive.yaml value wins over the env, and the env over the
// constant. The interval is never allowed below MinReleaseFeedInterval so a
// typo cannot turn the watcher into a hammer on a public index.
const (
	ReleaseFeedIntervalEnv     = "HIVE_RELEASE_FEED_INTERVAL"
	DefaultReleaseFeedInterval = 15 * time.Minute
	MinReleaseFeedInterval     = time.Minute

	ReleaseFeedCooldownEnv     = "HIVE_RELEASE_FEED_COOLDOWN"
	DefaultReleaseFeedCooldown = time.Hour
)

// ReleaseFeedsConfig is governor.release_feeds.
type ReleaseFeedsConfig struct {
	// Enabled turns the watcher off when explicitly false. Absent means on
	// whenever at least one agent lists a source.
	Enabled *bool `yaml:"enabled,omitempty"`
	// IntervalS is the poll interval in seconds (0 = env or default).
	IntervalS int `yaml:"interval_s,omitempty"`
	// CooldownS is the default per-agent minimum gap between two
	// release-feed kicks, in seconds (0 = env or default).
	CooldownS int `yaml:"cooldown_s,omitempty"`
	// Agents maps an agent name to the feeds it subscribes to.
	Agents map[string]ReleaseFeedAgentConfig `yaml:"agents,omitempty"`
}

// ReleaseFeedAgentConfig is one agent's subscription.
type ReleaseFeedAgentConfig struct {
	Sources []ReleaseFeedSource `yaml:"sources,omitempty"`
	// CooldownS overrides the block-level cooldown for this agent.
	CooldownS int `yaml:"cooldown_s,omitempty"`
}

// ReleaseFeedSource is one upstream feed.
type ReleaseFeedSource struct {
	// Kind is github_release, github_tag, pypi or index.
	Kind string `yaml:"kind"`
	// Repo is owner/repo for the GitHub kinds.
	Repo string `yaml:"repo,omitempty"`
	// Package is the PyPI project name for kind pypi.
	Package string `yaml:"package,omitempty"`
	// URL is the index endpoint for kind index. It returns either JSON
	// carrying the version under VersionField, or plain text whose first
	// non-empty line is the version.
	URL string `yaml:"url,omitempty"`
	// VersionField is the top-level JSON key holding the version for kind
	// index (default "version").
	VersionField string `yaml:"version_field,omitempty"`
	// Name labels the feed in the kick block (default: repo, package or URL).
	Name string `yaml:"name,omitempty"`
	// AuthVar names an existing per-agent (or hive-level) variable whose
	// resolved value is sent as the bearer token. The value is never logged.
	AuthVar string `yaml:"auth_var,omitempty"`
}

// ReleaseFeedsEnabled reports whether the watcher should run: not explicitly
// disabled and at least one agent lists a source.
func (c *Config) ReleaseFeedsEnabled() bool {
	if c == nil || c.Governor.ReleaseFeeds == nil {
		return false
	}
	rf := c.Governor.ReleaseFeeds
	if rf.Enabled != nil && !*rf.Enabled {
		return false
	}
	for _, a := range rf.Agents {
		if len(a.Sources) > 0 {
			return true
		}
	}
	return false
}

// ReleaseFeedInterval is the effective poll interval.
func (c *Config) ReleaseFeedInterval() time.Duration {
	return releaseFeedInterval(c, os.Getenv)
}

func releaseFeedInterval(c *Config, getenv func(string) string) time.Duration {
	d := DefaultReleaseFeedInterval
	if v, ok := envDuration(getenv, ReleaseFeedIntervalEnv); ok {
		d = v
	}
	if c != nil && c.Governor.ReleaseFeeds != nil && c.Governor.ReleaseFeeds.IntervalS > 0 {
		d = time.Duration(c.Governor.ReleaseFeeds.IntervalS) * time.Second
	}
	return max(d, MinReleaseFeedInterval)
}

// ReleaseFeedCooldown is the effective cooldown for agent.
func (c *Config) ReleaseFeedCooldown(agent string) time.Duration {
	return releaseFeedCooldown(c, agent, os.Getenv)
}

func releaseFeedCooldown(c *Config, agent string, getenv func(string) string) time.Duration {
	d := DefaultReleaseFeedCooldown
	if v, ok := envDuration(getenv, ReleaseFeedCooldownEnv); ok {
		d = v
	}
	if c == nil || c.Governor.ReleaseFeeds == nil {
		return d
	}
	rf := c.Governor.ReleaseFeeds
	if rf.CooldownS > 0 {
		d = time.Duration(rf.CooldownS) * time.Second
	}
	if a, ok := rf.Agents[agent]; ok && a.CooldownS > 0 {
		d = time.Duration(a.CooldownS) * time.Second
	}
	return d
}

func envDuration(getenv func(string) string, name string) (time.Duration, bool) {
	if getenv == nil {
		return 0, false
	}
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// validateReleaseFeeds checks governor.release_feeds.
func validateReleaseFeeds(rf *ReleaseFeedsConfig) error {
	if rf == nil {
		return nil
	}
	if rf.IntervalS < 0 {
		return fmt.Errorf("governor.release_feeds.interval_s must be at least 0")
	}
	if rf.CooldownS < 0 {
		return fmt.Errorf("governor.release_feeds.cooldown_s must be at least 0")
	}
	for agent, a := range rf.Agents {
		if a.CooldownS < 0 {
			return fmt.Errorf("governor.release_feeds.agents.%s.cooldown_s must be at least 0", agent)
		}
		for i, src := range a.Sources {
			if err := validateReleaseFeedSource(src); err != nil {
				return fmt.Errorf("governor.release_feeds.agents.%s.sources[%d]: %w", agent, i, err)
			}
		}
	}
	return nil
}

func validateReleaseFeedSource(src ReleaseFeedSource) error {
	switch src.Kind {
	case ReleaseFeedKindGitHubRelease, ReleaseFeedKindGitHubTag:
		owner, repo, ok := strings.Cut(strings.TrimSpace(src.Repo), "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
			return fmt.Errorf("kind %s needs repo as owner/repo, got %q", src.Kind, src.Repo)
		}
	case ReleaseFeedKindPyPI:
		if strings.TrimSpace(src.Package) == "" {
			return fmt.Errorf("kind pypi needs package")
		}
	case ReleaseFeedKindIndex:
		u := strings.TrimSpace(src.URL)
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			return fmt.Errorf("kind index needs an http(s) url, got %q", src.URL)
		}
	default:
		return fmt.Errorf("kind must be one of %s, %s, %s, %s; got %q",
			ReleaseFeedKindGitHubRelease, ReleaseFeedKindGitHubTag, ReleaseFeedKindPyPI, ReleaseFeedKindIndex, src.Kind)
	}
	if src.AuthVar != "" && !ValidVariableName(src.AuthVar) {
		return fmt.Errorf("auth_var %q is not a valid variable name", src.AuthVar)
	}
	return nil
}

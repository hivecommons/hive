package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// repoSettingsMu guards Config.Repos. The dashboard edits a repo's settings
// while the scheduler and the issue-stage pass read them from other
// goroutines, so the map is never touched without it.
var repoSettingsMu sync.RWMutex

// RepoSettings is one managed repository's per-repo knobs under the top-level
// `repos:` map (hivecommons/hive#18495). It is additive: a repo with no entry
// inherits the hive-wide value of every knob, so an absent map is exactly the
// previous behaviour.
type RepoSettings struct {
	// AgentsMdMaxBytes caps this repo's rendered AGENTS.md block, overriding
	// project.agents_md_max_bytes. Zero inherits it. The agentsmd floor and
	// hard ceiling still apply (agentsmd.EffectiveMaxBytes).
	AgentsMdMaxBytes int `yaml:"agents_md_max_bytes,omitempty" json:"agents_md_max_bytes,omitempty"`
	// ParentAutoClose is the decomposed-parent auto-close policy for this
	// repo (hivecommons/hive#18869): "true" (the default when empty) closes a
	// decomposed parent once every child is closed, except an issue labelled
	// epic or hold; "false" never auto-closes a parent; "epics_too" also
	// closes epic/hold-labelled parents; "trackers_too" does that and also
	// closes tracker or umbrella issues that have no sub-issue children once
	// every PR and issue they reference is finished (hivecommons/hive#19071).
	// A parent a person reopened after a hive auto-close is never re-closed
	// under any value. YAML booleans
	// true/false are accepted and read as the strings.
	ParentAutoClose string `yaml:"parent_autoclose,omitempty" json:"parent_autoclose,omitempty"`
	// ReviewPreflight overrides review.preflight for this repo
	// (hivecommons/hive#19069). Unset fields inherit the hive-wide value.
	ReviewPreflight ReviewPreflightConfig `yaml:"review_preflight,omitempty" json:"review_preflight,omitempty"`
}

// Parent auto-close policy values for RepoSettings.ParentAutoClose.
const (
	ParentAutoCloseOn       = "true"
	ParentAutoCloseOff      = "false"
	ParentAutoCloseEpicsToo = "epics_too"
	// ParentAutoCloseTrackersToo implies ParentAutoCloseEpicsToo and also
	// closes tracker/umbrella issues whose referenced work is all finished.
	ParentAutoCloseTrackersToo = "trackers_too"
)

// ParentAutoCloseFor is the normalised parent auto-close policy for repo:
// one of ParentAutoCloseOn (default), ParentAutoCloseOff,
// ParentAutoCloseEpicsToo or ParentAutoCloseTrackersToo. An unrecognised value falls back to the default.
func (c *Config) ParentAutoCloseFor(repo string) string {
	rs, ok := c.RepoSettingsFor(repo)
	if !ok {
		return ParentAutoCloseOn
	}
	switch v := strings.ToLower(strings.TrimSpace(rs.ParentAutoClose)); v {
	case ParentAutoCloseOff, ParentAutoCloseEpicsToo, ParentAutoCloseTrackersToo:
		return v
	}
	return ParentAutoCloseOn
}

// RepoSettingsFor returns repo's entry from the `repos:` map, and whether one
// exists. repo may be given bare or org-qualified, and a key may be written
// either way too: both sides are org-qualified and case-folded before they are
// compared, since GitHub repository names are case-insensitive.
func (c *Config) RepoSettingsFor(repo string) (RepoSettings, bool) {
	if c == nil || len(c.Repos) == 0 || strings.TrimSpace(repo) == "" {
		return RepoSettings{}, false
	}
	repoSettingsMu.RLock()
	defer repoSettingsMu.RUnlock()
	if key, ok := c.repoSettingsKeyLocked(repo); ok {
		return c.Repos[key], true
	}
	return RepoSettings{}, false
}

// repoSettingsKeyLocked is the key of repo's entry in the `repos:` map. The
// caller holds repoSettingsMu.
func (c *Config) repoSettingsKeyLocked(repo string) (string, bool) {
	if _, ok := c.Repos[repo]; ok {
		return repo, true
	}
	want := repoPauseKey(c.Project.Org, strings.TrimSpace(repo))
	for key := range c.Repos {
		if repoPauseKey(c.Project.Org, strings.TrimSpace(key)) == want {
			return key, true
		}
	}
	return "", false
}

// NormalizeParentAutoClose maps a dashboard or YAML value onto one of the
// three policy constants, and reports false for anything else (an empty value
// included: the dashboard always sends an explicit choice).
func NormalizeParentAutoClose(v string) (string, bool) {
	switch n := strings.ToLower(strings.TrimSpace(v)); n {
	case ParentAutoCloseOn, ParentAutoCloseOff, ParentAutoCloseEpicsToo, ParentAutoCloseTrackersToo:
		return n, true
	}
	return "", false
}

// ParentAutoCloseExplicit reports whether repo carries its own
// repos.<repo>.parent_autoclose value, as opposed to inheriting the default.
func (c *Config) ParentAutoCloseExplicit(repo string) bool {
	rs, ok := c.RepoSettingsFor(repo)
	if !ok {
		return false
	}
	_, valid := NormalizeParentAutoClose(rs.ParentAutoClose)
	return valid
}

// ErrRepoNotMonitored is returned by SetParentAutoClose for a repo that is not
// in project.repos: the policy only governs repos the hive manages.
var ErrRepoNotMonitored = errors.New("repo is not a monitored repo")

// SetParentAutoClose sets repos.<repo>.parent_autoclose for a monitored repo
// (hivecommons/hive#19070). value must be one of "true", "false" or
// "epics_too". Choosing the default ("true") clears the override, and an entry
// left with no other knob is dropped, so the config file only carries values
// that differ from the default. The change is visible to every reader at once:
// ParentAutoCloseFor reads the live map, so no restart is needed.
func (c *Config) SetParentAutoClose(repo, value string) error {
	if c == nil {
		return errors.New("config unavailable")
	}
	norm, ok := NormalizeParentAutoClose(value)
	if !ok {
		return fmt.Errorf("parent_autoclose must be %q, %q, %q or %q", ParentAutoCloseOn, ParentAutoCloseEpicsToo, ParentAutoCloseTrackersToo, ParentAutoCloseOff)
	}
	name, ok := c.monitoredRepoName(repo)
	if !ok {
		return fmt.Errorf("%w: %s", ErrRepoNotMonitored, strings.TrimSpace(repo))
	}
	repoSettingsMu.Lock()
	defer repoSettingsMu.Unlock()
	key, exists := c.repoSettingsKeyLocked(name)
	if !exists {
		key = name
	}
	rs := c.Repos[key]
	if norm == ParentAutoCloseOn {
		rs.ParentAutoClose = ""
	} else {
		rs.ParentAutoClose = norm
	}
	// RepoSettings holds slices (review_preflight), so it is not comparable
	// with ==; IsZero reports an entry with no knob set.
	if reflect.ValueOf(rs).IsZero() {
		delete(c.Repos, key)
		return nil
	}
	if c.Repos == nil {
		c.Repos = map[string]RepoSettings{}
	}
	c.Repos[key] = rs
	return nil
}

// monitoredRepoName is the project.repos entry (as written) that repo names,
// compared org-qualified and case-folded.
func (c *Config) monitoredRepoName(repo string) (string, bool) {
	want := repoPauseKey(c.Project.Org, strings.TrimSpace(repo))
	if want == "" {
		return "", false
	}
	for _, r := range c.Project.Repos {
		if repoPauseKey(c.Project.Org, strings.TrimSpace(r)) == want {
			return strings.TrimSpace(r), true
		}
	}
	return "", false
}

// AgentsMdMaxBytesFor is the configured AGENTS.md injection cap for repo:
// repos.<repo>.agents_md_max_bytes when set, else
// project.agents_md_max_bytes. Zero means "use the agentsmd default"; the
// floor and ceiling are applied by the agentsmd package, not here.
func (c *Config) AgentsMdMaxBytesFor(repo string) int {
	if c == nil {
		return 0
	}
	if rs, ok := c.RepoSettingsFor(repo); ok && rs.AgentsMdMaxBytes > 0 {
		return rs.AgentsMdMaxBytes
	}
	return c.Project.AgentsMdMaxBytes
}

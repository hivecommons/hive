package config

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// RepoPause records that one repository is currently quiet — no agent writes
// against it, no agent work assembled for it — together with the provenance of
// the decision.
//
// Pause is a RUN-STATE, not a policy tier and not an edit to the hive's
// identity. project.repos still lists a paused repo, so it keeps its dashboard
// card, its ACMM eval and its place in the operator's picture of the fleet;
// only agent activity stops. That is the distinction #6203 asked for. The
// pre-existing workaround — deleting the repo from project.repos — reads as a
// permanent decision, drops the repo from the GitHub client's scope wholesale,
// and records nothing about who removed it or why.
//
// Pause is also orthogonal to the ACMM level. No level means "off": L1 still
// schedules agents and brainstorm is advisory at every level, so lowering a
// repo's autonomy cannot silence it. Level answers "how much may agents do
// here"; pause answers "should anything run here at all".
//
// Provenance is not decoration. #4041/#4042 established that an unexplained
// pause becomes its own support burden — days later nobody can distinguish a
// deliberate operator pause from a malfunction. #4055 answered that for agents
// by recording who/when/why on the pause itself; this is the same answer for
// repos.
type RepoPause struct {
	// Repo names the paused repository in whichever form project.repos uses
	// for it: a bare name ("console") or an explicit cross-org reference
	// ("laredo/cuga-agent"). Matching is done on the org-qualified, lower-cased
	// form, so both spellings of the same repo resolve alike and a config
	// written with either one behaves the same.
	Repo string `yaml:"repo" json:"repo"`

	// By is the acting operator behind the pause, when one is known. Empty for
	// a pause written by hand into the config file — that is a real state, not
	// a defect, and is kept distinguishable from an attributed one.
	By string `yaml:"by,omitempty" json:"by,omitempty"`

	// At is when the pause was recorded. A pointer so "unknown" (a hand-written
	// entry) stays distinguishable from the zero time, which YAML would
	// otherwise persist as a real-looking year-1 timestamp.
	At *time.Time `yaml:"at,omitempty" json:"at,omitempty"`

	// Reason is the operator's free-text explanation: "release freeze",
	// "CI red for an unrelated reason", "declared but not onboarded yet".
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// repoPauseMu guards Project.PausedRepos. Unlike the agent pause field, this
// one is READ from goroutines that are not the ones that write it — the proxy's
// per-request enforcement, the scheduler building a kick, the GitHub client
// enumerating work — while the dashboard mutates it under saveMu. A dedicated
// RWMutex keeps those reads off the mutation, so a pause taken mid-enumeration
// is not a race.
//
// Lock order is saveMu → repoPauseMu, never the reverse.
var repoPauseMu sync.RWMutex

// QualifyRepo returns repo as an "org/name" reference. An entry that already
// carries a slash is a deliberate cross-org reference and is returned
// unchanged — prefixing it again is the "laredo/laredo/cuga-agent" bug that has
// bitten every place this join was written inline.
func QualifyRepo(org, repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") {
		return repo
	}
	org = strings.TrimSpace(org)
	if org == "" {
		return repo
	}
	return org + "/" + repo
}

// repoPauseKey is the comparison key for a repo reference: org-qualified and
// case-folded, because GitHub repository names are case-insensitive and an
// operator who pauses "Console" must not find agents still writing to
// "console".
func repoPauseKey(org, repo string) string {
	return strings.ToLower(QualifyRepo(org, repo))
}

// RepoPauseFor returns the pause covering repo, and whether one exists. repo
// may be given bare or org-qualified.
func (c *Config) RepoPauseFor(repo string) (RepoPause, bool) {
	if c == nil {
		return RepoPause{}, false
	}
	key := repoPauseKey(c.Project.Org, repo)
	if key == "" {
		return RepoPause{}, false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, rp := range c.Project.PausedRepos {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			return rp, true
		}
	}
	return RepoPause{}, false
}

// IsRepoPaused reports whether repo is currently operator-paused. It is the
// single predicate every enforcement point asks — the proxy, the hive-mediated
// PR/merge relays, work enumeration and kick assembly — so there is exactly one
// definition of "paused" and one place to change it.
func (c *Config) IsRepoPaused(repo string) bool {
	_, paused := c.RepoPauseFor(repo)
	return paused
}

// PausedRepoSet returns the org-qualified, lower-cased key of every paused
// repo. It is for components that want a snapshot rather than a live predicate.
func (c *Config) PausedRepoSet() map[string]bool {
	if c == nil {
		return nil
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	if len(c.Project.PausedRepos) == 0 {
		return nil
	}
	out := make(map[string]bool, len(c.Project.PausedRepos))
	for _, rp := range c.Project.PausedRepos {
		if key := repoPauseKey(c.Project.Org, rp.Repo); key != "" {
			out[key] = true
		}
	}
	return out
}

// PausedRepoNames returns the paused repos as project.repos spells them, in
// config order. Empty when nothing is paused, which is the state of every hive
// that has never used this feature.
func (c *Config) PausedRepoNames() []string {
	if c == nil {
		return nil
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	if len(c.Project.PausedRepos) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Project.PausedRepos))
	for _, rp := range c.Project.PausedRepos {
		if name := strings.TrimSpace(rp.Repo); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// ActiveRepos returns project.repos with the paused entries removed, preserving
// order and the spelling of each entry.
//
// This is what agent-facing surfaces enumerate: the GitHub client's work scope
// and the AUTHORIZED REPOS section of a kick. Operator-facing surfaces — the
// dashboard's repository cards, the ACMM evaluation — deliberately keep reading
// Project.Repos, because a paused repo is still part of the hive and must not
// silently vanish from the operator's view. That difference is the whole point
// of pause over deletion.
//
// With nothing paused this returns Project.Repos itself, so a hive that never
// uses the feature allocates nothing and behaves exactly as before.
func (c *Config) ActiveRepos() []string {
	if c == nil {
		return nil
	}
	paused := c.PausedRepoSet()
	if len(paused) == 0 {
		return c.Project.Repos
	}
	out := make([]string, 0, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		if paused[repoPauseKey(c.Project.Org, repo)] {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// SetRepoPausedAndSave records or clears one repo's pause and persists the
// config, mirroring SetAgentPausedAndSave: the read-modify-write and the Save
// happen under the SAME saveMu every other saver takes, so a repo pause cannot
// be lost to a concurrent write of an older snapshot — the failure that made
// AgentConfig.Paused necessary in the first place.
//
// Returns whether a change was made. Pausing an already-paused repo is a no-op
// that deliberately keeps the ORIGINAL provenance: re-pausing must not restamp
// who/when, or a stale dashboard could quietly rewrite the record of why a repo
// has been quiet since Tuesday (the agent-pause lesson of #4041).
func (c *Config) SetRepoPausedAndSave(repo string, paused bool, by, reason string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
	}

	saveMu.Lock()
	defer saveMu.Unlock()

	key := repoPauseKey(c.Project.Org, name)
	repoPauseMu.Lock()
	idx := -1
	for i, rp := range c.Project.PausedRepos {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			idx = i
			break
		}
	}
	changed := false
	switch {
	case paused && idx < 0:
		now := time.Now().UTC()
		c.Project.PausedRepos = append(c.Project.PausedRepos, RepoPause{
			Repo:   name,
			By:     strings.TrimSpace(by),
			At:     &now,
			Reason: strings.TrimSpace(reason),
		})
		changed = true
	case !paused && idx >= 0:
		c.Project.PausedRepos = append(c.Project.PausedRepos[:idx:idx], c.Project.PausedRepos[idx+1:]...)
		changed = true
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// PausedRepoWarnings reports paused_repos entries that name a repository the
// hive does not watch. Such an entry is inert — nothing matches it — and the
// usual cause is a typo, or a repo removed from project.repos while still
// paused. It is deliberately a warning and not a validation error: refusing to
// boot over a stale pause entry would be a worse failure than an ignored one,
// and keeping the entry means re-adding the repo restores its pause rather than
// silently un-pausing it.
func PausedRepoWarnings(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	watched := make(map[string]bool, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if key := repoPauseKey(cfg.Project.Org, repo); key != "" {
			watched[key] = true
		}
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	var warnings []string
	for _, rp := range cfg.Project.PausedRepos {
		name := strings.TrimSpace(rp.Repo)
		if name == "" {
			warnings = append(warnings, "project.paused_repos has an entry with no repo name — it pauses nothing")
			continue
		}
		if !watched[repoPauseKey(cfg.Project.Org, name)] {
			warnings = append(warnings, fmt.Sprintf(
				"project.paused_repos names %q, which is not in project.repos — the pause is inert until the repo is watched", name))
		}
	}
	return warnings
}

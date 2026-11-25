package config

import (
	"errors"
	"fmt"
	"strings"
)

// Per-repo "open PRs" mode (project.repo_policies[].open_prs).
//
// project.open_prs is all-or-nothing for the hive. This setting narrows it
// for one repository, at every ACMM level:
//
//   - on (the default; also what an unset value means): agents may open PRs
//     there, as project.open_prs allows.
//   - off: agents may analyse, comment and file issues there, but open no PR.
//   - workflow_only: only a PR that comes from a workflow-dispatched hand-off
//     (workflow.dispatch) may open there. The agent's own cadence work
//     (coverage gaps, test hardening and the like) may not.
//
// project.open_prs: false still wins: every repo then resolves to off.
//
// The mode is enforced where agent PRs are opened (the pr-request relay, the
// forge relay, the sandbox PR path and the sidecar callback), not only in the
// kick text.

// RepoOpenPRsMode is one repo's open-PRs mode.
type RepoOpenPRsMode string

const (
	// RepoOpenPRsOn lets agents open PRs (the default).
	RepoOpenPRsOn RepoOpenPRsMode = "on"
	// RepoOpenPRsOff lets agents open no PRs.
	RepoOpenPRsOff RepoOpenPRsMode = "off"
	// RepoOpenPRsWorkflowOnly lets only workflow hand-off PRs open.
	RepoOpenPRsWorkflowOnly RepoOpenPRsMode = "workflow_only"
)

// RepoOpenPRsModes lists the valid modes, most permissive first.
var RepoOpenPRsModes = []RepoOpenPRsMode{RepoOpenPRsOn, RepoOpenPRsWorkflowOnly, RepoOpenPRsOff}

// ParseRepoOpenPRsMode reads a configured or requested mode. Empty is on.
// It accepts the dashed spelling "workflow-only" as well. ok is false for
// anything else.
func ParseRepoOpenPRsMode(s string) (RepoOpenPRsMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(RepoOpenPRsOn):
		return RepoOpenPRsOn, true
	case string(RepoOpenPRsOff):
		return RepoOpenPRsOff, true
	case string(RepoOpenPRsWorkflowOnly), "workflow-only":
		return RepoOpenPRsWorkflowOnly, true
	}
	return "", false
}

// rank orders modes by how much they allow: off < workflow_only < on.
func (m RepoOpenPRsMode) rank() int {
	switch m {
	case RepoOpenPRsOn:
		return 2
	case RepoOpenPRsWorkflowOnly:
		return 1
	}
	return 0
}

// Widens reports whether moving from prev to m allows more PRs than before.
// The dashboard asks for the owner role for a widening change, as it does
// for switching auto-merge back on.
func (m RepoOpenPRsMode) Widens(prev RepoOpenPRsMode) bool { return m.rank() > prev.rank() }

// RepoOpenPRsSetting is the repo's own stored mode, before the hive-wide
// switch is applied. An unset or unreadable value is on.
func (c *Config) RepoOpenPRsSetting(repo string) RepoOpenPRsMode {
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if m, valid := ParseRepoOpenPRsMode(rp.OpenPRs); valid {
			return m
		}
		// validateRepoOpenPRs refuses such a value at load; one that got in
		// another way fails closed.
		return RepoOpenPRsOff
	}
	return RepoOpenPRsOn
}

// RepoOpenPRsMode resolves the effective open-PRs mode for repo:
// project.open_prs: false makes every repo off; otherwise the repo's own
// setting applies, and an unset one is on. It does not depend on the ACMM
// level: whether an agent may push at all is a separate gate.
func (c *Config) RepoOpenPRsMode(repo string) RepoOpenPRsMode {
	if c == nil {
		return RepoOpenPRsOn
	}
	if !c.Project.PRsAllowed() {
		return RepoOpenPRsOff
	}
	return c.RepoOpenPRsSetting(repo)
}

// RepoOpenPRsRefusal is why an agent may not open a PR in repo under its
// open-PRs mode, or "" when it may. workflowHandoff says whether the PR comes
// from a workflow-dispatched hand-off, as the caller proved from the hive's
// own hand-off records (never from PR text).
func (c *Config) RepoOpenPRsRefusal(repo string, workflowHandoff bool) string {
	switch c.RepoOpenPRsMode(repo) {
	case RepoOpenPRsOff:
		if c != nil && !c.Project.PRsAllowed() {
			return "pull requests are off for this hive (project.open_prs: false); file findings as issues instead"
		}
		return fmt.Sprintf("pull requests are off for %s (repo_policies open_prs: off); file findings as issues or comments instead", repo)
	case RepoOpenPRsWorkflowOnly:
		if workflowHandoff {
			return ""
		}
		return fmt.Sprintf("%s accepts only workflow hand-off pull requests (repo_policies open_prs: workflow_only), and this PR does not come from a workflow hand-off this agent holds; file findings as issues instead", repo)
	}
	return ""
}

// ErrRepoOpenPRsWidenRefused is returned by SetRepoOpenPRsForRepoAndSave
// when the change would widen the repo's mode and the caller may not widen.
var ErrRepoOpenPRsWidenRefused = errors.New("widening the open-PRs mode requires the owner role")

// SetRepoOpenPRsForRepoAndSave records and persists one repo's open-PRs
// mode. "on" is stored explicitly rather than cleared, so a dashboard switch
// back to on outranks a seed that says off when the overlay is reloaded
// (adoptOverlayRepoOpenPRs). The widening check runs under the same lock as
// the write, so a caller that may not widen (widenOK false) cannot race
// another request into widening: it gets ErrRepoOpenPRsWidenRefused.
// changed is false when nothing moved.
func (c *Config) SetRepoOpenPRsForRepoAndSave(repo string, mode RepoOpenPRsMode, widenOK bool) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
	}
	parsed, ok := ParseRepoOpenPRsMode(string(mode))
	if !ok {
		return false, fmt.Errorf("unknown open_prs mode %q (want on, off or workflow_only)", mode)
	}
	name, _ = NormalizeRepoForOrg(c.Project.Org, name)

	saveMu.Lock()
	defer saveMu.Unlock()

	key := repoPauseKey(c.Project.Org, name)
	repoPauseMu.Lock()
	idx := -1
	for i, rp := range c.Project.RepoPolicies {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			idx = i
			break
		}
	}
	prev := RepoOpenPRsOn
	if idx >= 0 {
		if m, ok := ParseRepoOpenPRsMode(c.Project.RepoPolicies[idx].OpenPRs); ok {
			prev = m
		} else {
			prev = RepoOpenPRsOff
		}
	}
	if parsed.Widens(prev) && !widenOK {
		repoPauseMu.Unlock()
		return false, ErrRepoOpenPRsWidenRefused
	}
	changed := false
	v := string(parsed)
	if idx >= 0 {
		if c.Project.RepoPolicies[idx].OpenPRs != v {
			c.Project.RepoPolicies[idx].OpenPRs = v
			changed = true
		}
	} else if parsed != RepoOpenPRsOn {
		// A repo with no row is already on; only a narrowing adds one.
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, OpenPRs: v})
		changed = true
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// validateRepoOpenPRs refuses an open_prs value that is not a known mode. It
// is a new key, so it is strict.
func (c *Config) validateRepoOpenPRs() error {
	for i, rp := range c.Project.RepoPolicies {
		if _, ok := ParseRepoOpenPRsMode(rp.OpenPRs); !ok {
			return fmt.Errorf("project.repo_policies[%d].open_prs: unknown mode %q (want on, off or workflow_only)", i, rp.OpenPRs)
		}
	}
	return nil
}

// adoptOverlayRepoOpenPRs carries the dashboard overlay's per-repo open_prs
// modes onto cfg (hivecommons/hive#19740 shape): the dashboard saves the
// mode into the overlay, and a reload that starts from the seed must not
// drop it. Only entries that set open_prs (including an explicit "on") for a
// repo the hive still watches are read, and only that field is copied, so
// the seed's other per-repo fields are untouched.
func adoptOverlayRepoOpenPRs(cfg, overlay *Config) {
	if cfg == nil || overlay == nil {
		return
	}
	org := cfg.Project.Org
	for _, orp := range overlay.Project.RepoPolicies {
		mode := strings.TrimSpace(orp.OpenPRs)
		if mode == "" {
			continue
		}
		key := repoPauseKey(org, orp.Repo)
		if key == "" || !watchesRepoKey(cfg, key) {
			continue
		}
		found := false
		for i, rp := range cfg.Project.RepoPolicies {
			if repoPauseKey(org, rp.Repo) == key {
				cfg.Project.RepoPolicies[i].OpenPRs = mode
				found = true
				break
			}
		}
		if !found {
			cfg.Project.RepoPolicies = append(cfg.Project.RepoPolicies, RepoPolicy{Repo: orp.Repo, OpenPRs: mode})
		}
	}
}

// watchesRepoKey reports whether a repoPauseKey is one of project.repos.
func watchesRepoKey(cfg *Config, key string) bool {
	for _, r := range cfg.Project.Repos {
		if repoPauseKey(cfg.Project.Org, r) == key {
			return true
		}
	}
	return false
}

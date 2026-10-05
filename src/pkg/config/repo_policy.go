package config

import (
	"fmt"
	"strings"
	"time"
)

// RepoPolicy records optional per-repository policy overrides. It is separate
// from project.repos so existing configs keep their string-list repo shape.
type RepoPolicy struct {
	Repo                  string               `yaml:"repo" json:"repo"`
	SelfAuthorizationHold *bool                `yaml:"self_authorization_hold,omitempty" json:"self_authorization_hold,omitempty"`
	ReporterTrustHold     *bool                `yaml:"reporter_trust_hold,omitempty" json:"reporter_trust_hold,omitempty"`
	AutoMerge             *bool                `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	LabelDriven           bool                 `yaml:"label_driven,omitempty" json:"label_driven,omitempty"`
	ACMMLevel             *int                 `yaml:"acmm_level,omitempty" json:"acmm_level,omitempty"`
	ACMMPinned            bool                 `yaml:"acmm_pinned,omitempty" json:"acmm_pinned,omitempty"`
	ACMMLastAutomatic     *AutonomyLevelChange `yaml:"acmm_last_automatic,omitempty" json:"acmm_last_automatic,omitempty"`
}

type AutonomyLevelChange struct {
	At          time.Time `yaml:"at" json:"at"`
	Direction   string    `yaml:"direction" json:"direction"`
	From        int       `yaml:"from" json:"from"`
	To          int       `yaml:"to" json:"to"`
	Repo        string    `yaml:"repo" json:"repo"`
	EvidenceIDs []string  `yaml:"evidence_ids,omitempty" json:"evidence_ids,omitempty"`
	Reason      string    `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// RepoPolicyFor returns the policy override entry covering repo, if any.
func (c *Config) RepoPolicyFor(repo string) (RepoPolicy, bool) {
	if c == nil {
		return RepoPolicy{}, false
	}
	key := repoPauseKey(c.Project.Org, repo)
	if key == "" {
		return RepoPolicy{}, false
	}
	repoPauseMu.RLock()
	defer repoPauseMu.RUnlock()
	for _, rp := range c.Project.RepoPolicies {
		if repoPauseKey(c.Project.Org, rp.Repo) == key {
			return rp, true
		}
	}
	return RepoPolicy{}, false
}

func (c *Config) EffectiveACMMLevelForRepo(repo string) int {
	hive := c.ACMMLevelOrZero()
	if hive <= 0 {
		return hive
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.ACMMLevel != nil {
		if *rp.ACMMLevel < hive {
			return *rp.ACMMLevel
		}
		return hive
	}
	return hive
}

// RepoLabelDriven reports whether repo opted in to label-driven triage
// (hivecommons/hive#10537): maintainers accept and park issues with labels, so
// Hive posts no un-park "What to reply" notice there and never removes
// `needs-human` / `needs-decision` / `needs-direction` itself, including via
// `/hive approve` or `/hive decision`. Unset means today's behavior.
func (c *Config) RepoLabelDriven(repo string) bool {
	rp, ok := c.RepoPolicyFor(repo)
	return ok && rp.LabelDriven
}

func (c *Config) RepoACMMPinned(repo string) bool {
	rp, ok := c.RepoPolicyFor(repo)
	return ok && rp.ACMMPinned
}

func (c *Config) SetRepoACMMPinnedAndSave(repo string, pinned bool) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
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
	changed := false
	if idx >= 0 {
		if c.Project.RepoPolicies[idx].ACMMPinned != pinned {
			c.Project.RepoPolicies[idx].ACMMPinned = pinned
			changed = true
		}
	} else if pinned {
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, ACMMPinned: true})
		changed = true
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

func (c *Config) RepoACMMLastAutomatic(repo string) (AutonomyLevelChange, bool) {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok || rp.ACMMLastAutomatic == nil {
		return AutonomyLevelChange{}, false
	}
	return *rp.ACMMLastAutomatic, true
}

func (c *Config) SetRepoACMMAutomaticAndSave(repo string, level int, change AutonomyLevelChange) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
	}
	name, _ = NormalizeRepoForOrg(c.Project.Org, name)
	if level < MinACMMLevel || level > MaxACMMLevel {
		return false, fmt.Errorf("acmm level must be 1-6")
	}
	change.Repo = name

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
	changed := false
	if idx >= 0 {
		if c.Project.RepoPolicies[idx].ACMMLevel == nil || *c.Project.RepoPolicies[idx].ACMMLevel != level {
			v := level
			c.Project.RepoPolicies[idx].ACMMLevel = &v
			changed = true
		}
		c.Project.RepoPolicies[idx].ACMMLastAutomatic = &change
	} else {
		v := level
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, ACMMLevel: &v, ACMMLastAutomatic: &change})
		changed = true
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// SelfAuthorizationHoldEnabledForRepo resolves the #5117 hold switch for repo
// without considering ACMM level: per-repo override first, then the hive-wide
// GitHub default, then default ON.
func (c *Config) SelfAuthorizationHoldEnabledForRepo(repo string) bool {
	return c.SelfAuthorizationHoldEnabledForRepoAtLevel(repo, 0)
}

// SelfAuthorizationHoldEnabledForRepoAtLevel resolves the #5117 hold switch for
// repo at the provided live ACMM level: env override first, then per-repo
// override, then hive-wide GitHub override, then L6 defaults OFF and lower
// levels default ON.
func (c *Config) SelfAuthorizationHoldEnabledForRepoAtLevel(repo string, acmmLevel int) bool {
	if c == nil {
		return acmmLevel < MaxACMMLevel
	}
	if c.GitHub.selfAuthorizationHoldEnvOverride != nil {
		return *c.GitHub.selfAuthorizationHoldEnvOverride
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.SelfAuthorizationHold != nil {
		return *rp.SelfAuthorizationHold
	}
	return c.GitHub.SelfAuthorizationHoldEnabledAtLevel(acmmLevel)
}

// SetSelfAuthorizationHoldForRepoAndSave records, clears, and persists one
// repository's #5117 self-authorization hold override. nil clears the override
// so the repo inherits the hive-wide GitHub default.
func (c *Config) SetSelfAuthorizationHoldForRepoAndSave(repo string, enabled *bool) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
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
	changed := false
	if enabled == nil {
		if idx >= 0 && c.Project.RepoPolicies[idx].SelfAuthorizationHold != nil {
			c.Project.RepoPolicies[idx].SelfAuthorizationHold = nil
			if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
				c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
			}
			changed = true
		}
	} else {
		v := *enabled
		if idx >= 0 {
			if c.Project.RepoPolicies[idx].SelfAuthorizationHold == nil || *c.Project.RepoPolicies[idx].SelfAuthorizationHold != v {
				c.Project.RepoPolicies[idx].SelfAuthorizationHold = &v
				changed = true
			}
		} else {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, SelfAuthorizationHold: &v})
			changed = true
		}
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

func repoPolicyHasNoOverrides(rp RepoPolicy) bool {
	return rp.SelfAuthorizationHold == nil && rp.ReporterTrustHold == nil && rp.AutoMerge == nil && rp.ACMMLevel == nil && !rp.ACMMPinned && rp.ACMMLastAutomatic == nil && !rp.LabelDriven
}

// ReporterTrustHoldEnabledForRepo resolves the #9665 reporter-trust hold for
// one repository: env override, then the repo's own override, then the
// hive-wide value (which itself follows reporter_trust.enabled when unset).
func (c *Config) ReporterTrustHoldEnabledForRepo(repo string) bool {
	if c == nil {
		return false
	}
	if c.GitHub.reporterTrustHoldEnvOverride != nil {
		return *c.GitHub.reporterTrustHoldEnvOverride
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.ReporterTrustHold != nil {
		return *rp.ReporterTrustHold
	}
	return c.GitHub.ReporterTrustHoldEnabled(c.Project.IssueFilter.ReporterTrust.IsEnabled())
}

// SetReporterTrustHoldForRepos records per-repo #9665 overrides in memory:
// a true/false value sets the override, nil clears it. Persistence is the
// caller's job (the dashboard handler saves once for the whole request).
func (c *Config) SetReporterTrustHoldForRepos(values map[string]*bool) {
	if c == nil || len(values) == 0 {
		return
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	for repo, enabled := range values {
		name := strings.TrimSpace(repo)
		if name == "" {
			continue
		}
		name, _ = NormalizeRepoForOrg(c.Project.Org, name)
		key := repoPauseKey(c.Project.Org, name)
		idx := -1
		for i, rp := range c.Project.RepoPolicies {
			if repoPauseKey(c.Project.Org, rp.Repo) == key {
				idx = i
				break
			}
		}
		if enabled == nil {
			if idx >= 0 {
				c.Project.RepoPolicies[idx].ReporterTrustHold = nil
				if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
					c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
				}
			}
			continue
		}
		v := *enabled
		if idx >= 0 {
			c.Project.RepoPolicies[idx].ReporterTrustHold = &v
		} else {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, ReporterTrustHold: &v})
		}
	}
}

// RepoAutoMergeEnabled resolves the effective per-repo auto-merge switch.
// Auto-merge is fail-closed below L6, regardless of the stored per-repo
// override. At L6, an unset repo override means enabled so hives keep their
// prior fully-autonomous behavior until an owner switches a repo off.
func (c *Config) RepoAutoMergeEnabled(repo string) bool {
	if c == nil || c.ACMMLevelOrZero() < SelfMergeMinACMMLevel {
		return false
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.AutoMerge != nil {
		return *rp.AutoMerge
	}
	return true
}

// SetRepoAutoMergeForRepoAndSave records, clears, and persists one repo's
// auto-merge override. nil clears the override so the repo inherits default ON.
func (c *Config) SetRepoAutoMergeForRepoAndSave(repo string, enabled *bool) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
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
	changed := false
	if enabled == nil {
		if idx >= 0 && c.Project.RepoPolicies[idx].AutoMerge != nil {
			c.Project.RepoPolicies[idx].AutoMerge = nil
			if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
				c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
			}
			changed = true
		}
	} else {
		v := *enabled
		if idx >= 0 {
			if c.Project.RepoPolicies[idx].AutoMerge == nil || *c.Project.RepoPolicies[idx].AutoMerge != v {
				c.Project.RepoPolicies[idx].AutoMerge = &v
				changed = true
			}
		} else {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, AutoMerge: &v})
			changed = true
		}
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// SetSelfAuthorizationHoldForRepos applies a batch of per-repo #5117 override
// edits without saving. Values set explicit repo overrides; nil clears one so
// the repo inherits the hive-wide default. The caller owns persistence.
func (c *Config) SetSelfAuthorizationHoldForRepos(overrides map[string]*bool) bool {
	if c == nil || len(overrides) == 0 {
		return false
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	changed := false
	for repo, enabled := range overrides {
		name := strings.TrimSpace(repo)
		name, _ = NormalizeRepoForOrg(c.Project.Org, name)
		key := repoPauseKey(c.Project.Org, name)
		if key == "" {
			continue
		}
		idx := -1
		for i, rp := range c.Project.RepoPolicies {
			if repoPauseKey(c.Project.Org, rp.Repo) == key {
				idx = i
				break
			}
		}
		if enabled == nil {
			if idx >= 0 && c.Project.RepoPolicies[idx].SelfAuthorizationHold != nil {
				c.Project.RepoPolicies[idx].SelfAuthorizationHold = nil
				if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
					c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
				}
				changed = true
			}
			continue
		}
		v := *enabled
		if idx >= 0 {
			if c.Project.RepoPolicies[idx].SelfAuthorizationHold == nil || *c.Project.RepoPolicies[idx].SelfAuthorizationHold != v {
				c.Project.RepoPolicies[idx].SelfAuthorizationHold = &v
				changed = true
			}
			continue
		}
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, SelfAuthorizationHold: &v})
		changed = true
	}
	return changed
}

// ClearRepoPolicies removes every per-repo policy override. It is used when a
// repo-list save migrates the hive to another org, where bare overrides from
// the previous org must not silently retarget to same-named repos.
func (c *Config) ClearRepoPolicies() bool {
	if c == nil {
		return false
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	if len(c.Project.RepoPolicies) == 0 {
		return false
	}
	c.Project.RepoPolicies = nil
	return true
}

// PruneRepoPoliciesToWatched drops overrides for repos no longer listed in
// project.repos. The current org is used for matching, so callers that are
// changing orgs should ClearRepoPolicies first instead.
func (c *Config) PruneRepoPoliciesToWatched() bool {
	if c == nil || len(c.Project.RepoPolicies) == 0 {
		return false
	}
	watched := make(map[string]bool, len(c.Project.Repos))
	for _, repo := range c.Project.Repos {
		if key := repoPauseKey(c.Project.Org, repo); key != "" {
			watched[key] = true
		}
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	next := c.Project.RepoPolicies[:0]
	for _, rp := range c.Project.RepoPolicies {
		if watched[repoPauseKey(c.Project.Org, rp.Repo)] {
			next = append(next, rp)
		}
	}
	if len(next) == len(c.Project.RepoPolicies) {
		return false
	}
	c.Project.RepoPolicies = next
	if len(c.Project.RepoPolicies) == 0 {
		c.Project.RepoPolicies = nil
	}
	return true
}

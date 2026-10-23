package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/redmain"
)

// RepoPolicy records optional per-repository policy overrides. It is separate
// from project.repos so existing configs keep their string-list repo shape.
type RepoPolicy struct {
	Repo                  string `yaml:"repo" json:"repo"`
	SelfAuthorizationHold *bool  `yaml:"self_authorization_hold,omitempty" json:"self_authorization_hold,omitempty"`
	ReporterTrustHold     *bool  `yaml:"reporter_trust_hold,omitempty" json:"reporter_trust_hold,omitempty"`
	AutoMerge             *bool  `yaml:"auto_merge,omitempty" json:"auto_merge,omitempty"`
	LabelDriven           bool   `yaml:"label_driven,omitempty" json:"label_driven,omitempty"`
	ReleaseOnApprove      bool   `yaml:"release_on_approve,omitempty" json:"release_on_approve,omitempty"`
	// ReleaseVersionSource is an optional repository-relative text file whose
	// trimmed value must match the approved tag (with an optional leading v).
	ReleaseVersionSource string `yaml:"release_version_source,omitempty" json:"release_version_source,omitempty"`
	// LabelDrivenApprovalLabels overrides project.label_driven_approval_labels
	// for this repo (hivecommons/hive#11313). Only read when LabelDriven is set.
	LabelDrivenApprovalLabels []string `yaml:"label_driven_approval_labels,omitempty" json:"label_driven_approval_labels,omitempty"`
	MergeStrategy             string   `yaml:"merge_strategy,omitempty" json:"merge_strategy,omitempty"`
	// MergeFrontTimeout overrides project.merge_front_timeout for this repo's
	// serialized lane (hivecommons/hive#12952): a Go duration such as "2h".
	MergeFrontTimeout string `yaml:"merge_front_timeout,omitempty" json:"merge_front_timeout,omitempty"`
	// RedMainThreshold overrides project.red_main_threshold for this repo
	// (hivecommons/hive#13630): how long a required check may stay red on
	// the default branch before the red-main circuit breaker trips. A Go
	// duration such as "6h".
	RedMainThreshold string `yaml:"red_main_threshold,omitempty" json:"red_main_threshold,omitempty"`
	// LabelDrivenSupersedeParkActors overrides
	// project.label_driven_supersede_park_actors for this repo
	// (hivecommons/hive#13605). Only read when LabelDriven is set.
	LabelDrivenSupersedeParkActors []string             `yaml:"label_driven_supersede_park_actors,omitempty" json:"label_driven_supersede_park_actors,omitempty"`
	ACMMLevel                      *int                 `yaml:"acmm_level,omitempty" json:"acmm_level,omitempty"`
	ACMMPinned                     bool                 `yaml:"acmm_pinned,omitempty" json:"acmm_pinned,omitempty"`
	ACMMLastAutomatic              *AutonomyLevelChange `yaml:"acmm_last_automatic,omitempty" json:"acmm_last_automatic,omitempty"`
	// CompanionRepos lists sibling repositories (bare or org-qualified) this
	// repo's tooling expects checked out beside it under the agent workspace
	// root, at <root>/<org>/<companion> (hivecommons/hive#13059). Agents are
	// told to clone them alongside the primary checkout instead of parking
	// when a repo-local tool reports the path missing.
	CompanionRepos []string `yaml:"companion_repos,omitempty" json:"companion_repos,omitempty"`
}

// Per-repo merge strategies (hivecommons/hive#10886). The strategy decides how
// Hive merges into a repo, never whether: auto-merge level, the per-repo
// auto_merge switch, pauses and holds still gate merging. Unrelated to
// knowledge.primer.merge_strategy.
const (
	MergeStrategyDirect         = "direct"
	MergeStrategyHiveSerialized = "hive-serialized"
)

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

// RepoReleaseOnApprove reports whether repo opted in to release-on-approve
// (hivecommons/hive#12631). It is only the opt-in switch; unset means false and
// a kind=release owner step behaves as it does today. A label-driven repo
// never executes a release: its sweep answers `/hive approve` with the
// triage-label pointer before reaching the release executor, so the dashboard
// action and the command share the same refusal (hivecommons/hive#12635).
func (c *Config) RepoReleaseOnApprove(repo string) bool {
	rp, ok := c.RepoPolicyFor(repo)
	return ok && rp.ReleaseOnApprove && !rp.LabelDriven
}

// RepoReleaseVersionSource returns the optional repository-relative version
// file checked before a release-on-approve write.
func (c *Config) RepoReleaseVersionSource(repo string) string {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok {
		return ""
	}
	return strings.TrimSpace(rp.ReleaseVersionSource)
}

// DefaultLabelDrivenApprovalLabel is the acceptance label Prow-style
// organisations use; it is what label-driven approval maps to "approve" when
// project.label_driven_approval_labels is unset.
const DefaultLabelDrivenApprovalLabel = "triage/accepted"

// DefaultLabelDrivenTrustedBots are the command bots whose label additions
// count as an acceptance on a label-driven repo when
// project.label_driven_trusted_bots is unset: the Prow instance hivecommons
// runs (hivecommons/hive#11313). github-actions[bot] is deliberately absent:
// it is the shared identity of every workflow in a repo, so operators must
// opt in per deployment (hivecommons/hive#11737).
var DefaultLabelDrivenTrustedBots = []string{"kubestellar-prow[bot]"}

// LabelDrivenApprovalLabels returns the labels whose trusted addition approves
// an issue on repo (hivecommons/hive#11313), or nil when repo is not
// label-driven, so the feature is a no-op everywhere it was not opted into.
// A label-driven repo resolves its own repo_policies override first, then
// project.label_driven_approval_labels, then DefaultLabelDrivenApprovalLabel.
// An explicit empty override turns label approval off for that repo while
// keeping the rest of label-driven behaviour.
func (c *Config) LabelDrivenApprovalLabels(repo string) []string {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok || !rp.LabelDriven {
		return nil
	}
	if rp.LabelDrivenApprovalLabels != nil {
		return normalizeLabelList(rp.LabelDrivenApprovalLabels)
	}
	if c.Project.LabelDrivenApprovalLabels != nil {
		return normalizeLabelList(c.Project.LabelDrivenApprovalLabels)
	}
	return []string{DefaultLabelDrivenApprovalLabel}
}

// LabelDrivenApprovalLabelsDefault returns the project-wide approval labels
// that label-driven repos without their own override inherit.
func (c *Config) LabelDrivenApprovalLabelsDefault() []string {
	if c != nil && c.Project.LabelDrivenApprovalLabels != nil {
		return normalizeLabelList(c.Project.LabelDrivenApprovalLabels)
	}
	return []string{DefaultLabelDrivenApprovalLabel}
}

// LabelDrivenTrustOrgMembers reports whether public members of the repo's
// owning organisation may approve by label (default true: Prow itself lets
// only org members add `triage/accepted`).
func (c *Config) LabelDrivenTrustOrgMembers() bool {
	if c == nil || c.Project.LabelDrivenTrustOrgMembers == nil {
		return true
	}
	return *c.Project.LabelDrivenTrustOrgMembers
}

// LabelDrivenTrustedBots returns the bot logins whose label additions count
// as acceptances on label-driven repos.
func (c *Config) LabelDrivenTrustedBots() []string {
	if c == nil || c.Project.LabelDrivenTrustedBots == nil {
		return append([]string(nil), DefaultLabelDrivenTrustedBots...)
	}
	return normalizeLabelList(c.Project.LabelDrivenTrustedBots)
}

// LabelDrivenSupersedeParkActors returns the automation logins whose park
// labels a later trusted acceptance on repo supersedes and removes
// (hivecommons/hive#13605), or nil when repo is not label-driven. A
// label-driven repo resolves its own repo_policies override first, then
// project.label_driven_supersede_park_actors. The default is empty: Hive
// removes only the park labels it added itself.
func (c *Config) LabelDrivenSupersedeParkActors(repo string) []string {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok || !rp.LabelDriven {
		return nil
	}
	if rp.LabelDrivenSupersedeParkActors != nil {
		return normalizeLabelList(rp.LabelDrivenSupersedeParkActors)
	}
	return normalizeLabelList(c.Project.LabelDrivenSupersedeParkActors)
}

func normalizeLabelList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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
	return c.GitHubSelfAuthorizationHoldEnabledAtLevel(acmmLevel)
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
	return rp.SelfAuthorizationHold == nil && rp.ReporterTrustHold == nil && rp.AutoMerge == nil && rp.ACMMLevel == nil && !rp.ACMMPinned && rp.ACMMLastAutomatic == nil && !rp.LabelDriven && !rp.ReleaseOnApprove && rp.ReleaseVersionSource == "" && rp.MergeStrategy == "" && rp.MergeFrontTimeout == "" && rp.RedMainThreshold == "" && len(rp.LabelDrivenApprovalLabels) == 0 && len(rp.LabelDrivenSupersedeParkActors) == 0 && len(rp.CompanionRepos) == 0
}

// ValidateRepoMergeStrategy accepts "" (unset, meaning direct), "direct" and
// "hive-serialized"; anything else is an error naming the repo and value.
func ValidateRepoMergeStrategy(repo, value string) error {
	switch value {
	case "", MergeStrategyDirect, MergeStrategyHiveSerialized:
		return nil
	}
	return fmt.Errorf("project.repo_policies: repo %q has invalid merge_strategy %q (must be %s or %s)", repo, value, MergeStrategyDirect, MergeStrategyHiveSerialized)
}

// RepoMergeStrategyError reports the configuration error for repo's stored
// merge_strategy, or nil when it is unset or valid.
func (c *Config) RepoMergeStrategyError(repo string) error {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok {
		return nil
	}
	return ValidateRepoMergeStrategy(rp.Repo, rp.MergeStrategy)
}

// RepoMergeStrategy returns repo's effective merge strategy: unset means
// direct. An invalid stored value is returned verbatim, never mapped to
// direct; RepoAutoMergeEnabled reports such a repo as off. Deciding direct
// needs no GitHub call.
func (c *Config) RepoMergeStrategy(repo string) string {
	if c == nil {
		return MergeStrategyDirect
	}
	if rp, ok := c.RepoPolicyFor(repo); ok && rp.MergeStrategy != "" {
		return rp.MergeStrategy
	}
	return MergeStrategyDirect
}

// ValidateMergeFrontTimeout accepts "" (unset) or a positive Go duration.
func ValidateMergeFrontTimeout(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if d, err := time.ParseDuration(value); err != nil || d <= 0 {
		return fmt.Errorf("%s: invalid merge_front_timeout %q (must be a positive duration such as 90m or 2h)", field, value)
	}
	return nil
}

// RepoMergeFrontTimeout returns the serialized lane's front timeout for repo
// (hivecommons/hive#12952): the repo_policies override, else
// project.merge_front_timeout, else 0, which leaves the lane's own default
// (60 minutes). An unparsable or non-positive value counts as unset.
func (c *Config) RepoMergeFrontTimeout(repo string) time.Duration {
	if c == nil {
		return 0
	}
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if d := parseMergeFrontTimeout(rp.MergeFrontTimeout); d > 0 {
			return d
		}
	}
	return parseMergeFrontTimeout(c.Project.MergeFrontTimeout)
}

// ValidateRedMainThreshold accepts "" (unset) or a positive Go duration.
func ValidateRedMainThreshold(field, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if d, err := time.ParseDuration(value); err != nil || d <= 0 {
		return fmt.Errorf("%s: invalid red_main_threshold %q (must be a positive duration such as 6h or 90m)", field, value)
	}
	return nil
}

// RepoRedMainThreshold returns how long a required check may stay red on
// repo's default branch before the red-main circuit breaker trips
// (hivecommons/hive#13630): the repo_policies override, else
// project.red_main_threshold, else redmain.DefaultThreshold (6h). An
// unparsable or non-positive value counts as unset.
func (c *Config) RepoRedMainThreshold(repo string) time.Duration {
	if c == nil {
		return redmain.DefaultThreshold
	}
	if rp, ok := c.RepoPolicyFor(repo); ok {
		if d := parseMergeFrontTimeout(rp.RedMainThreshold); d > 0 {
			return d
		}
	}
	if d := parseMergeFrontTimeout(c.Project.RedMainThreshold); d > 0 {
		return d
	}
	return redmain.DefaultThreshold
}

func parseMergeFrontTimeout(raw string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// SetRepoMergeStrategyForRepoAndSave persists one repo's merge strategy.
// direct clears the key, since unset already means direct. Unknown values are
// refused.
func (c *Config) SetRepoMergeStrategyForRepoAndSave(repo, strategy string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("no config loaded")
	}
	name := strings.TrimSpace(repo)
	if name == "" {
		return false, fmt.Errorf("repo is required")
	}
	strategy = strings.TrimSpace(strategy)
	if strategy == "" {
		return false, fmt.Errorf("merge_strategy is required")
	}
	if err := ValidateRepoMergeStrategy(name, strategy); err != nil {
		return false, err
	}
	name, _ = NormalizeRepoForOrg(c.Project.Org, name)
	stored := strategy
	if stored == MergeStrategyDirect {
		stored = ""
	}

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
		if c.Project.RepoPolicies[idx].MergeStrategy != stored {
			c.Project.RepoPolicies[idx].MergeStrategy = stored
			if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
				c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
			}
			changed = true
		}
	} else if stored != "" {
		c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, MergeStrategy: stored})
		changed = true
	}
	repoPauseMu.Unlock()

	if !changed {
		return false, nil
	}
	return true, c.saveLocked()
}

// CompanionReposFor returns the org-qualified companion repositories
// configured for repo via project.repo_policies[].companion_repos, in config
// order, deduplicated case-insensitively, with the repo itself and malformed
// entries dropped. Nil when none are configured.
func (c *Config) CompanionReposFor(repo string) []string {
	rp, ok := c.RepoPolicyFor(repo)
	if !ok || len(rp.CompanionRepos) == 0 {
		return nil
	}
	self := repoPauseKey(c.Project.Org, repo)
	seen := map[string]bool{self: true}
	var out []string
	for _, raw := range rp.CompanionRepos {
		full := QualifyRepo(c.Project.Org, raw)
		owner, name, found := strings.Cut(full, "/")
		if !found || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" || strings.Contains(name, "/") {
			continue
		}
		key := strings.ToLower(full)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, full)
	}
	return out
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

// LabelDrivenRepoSetting is one repo's label-driven policy as edited from the
// dashboard Repos tab (hivecommons/hive#11313). ApprovalLabels nil or empty
// keeps the project default.
type LabelDrivenRepoSetting struct {
	Enabled        bool
	ApprovalLabels *[]string
}

// SetLabelDrivenForRepos records or clears per-repo label-driven policies. A
// nil entry (or Enabled false) switches the repo back to the default
// comment-driven flow and drops the approval-label override with it; the
// policy row is pruned when nothing else is set on it.
func (c *Config) SetLabelDrivenForRepos(values map[string]*LabelDrivenRepoSetting) {
	if c == nil || len(values) == 0 {
		return
	}
	repoPauseMu.Lock()
	defer repoPauseMu.Unlock()
	for repo, setting := range values {
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
		if setting == nil || !setting.Enabled {
			if idx >= 0 {
				c.Project.RepoPolicies[idx].LabelDriven = false
				c.Project.RepoPolicies[idx].LabelDrivenApprovalLabels = nil
				if repoPolicyHasNoOverrides(c.Project.RepoPolicies[idx]) {
					c.Project.RepoPolicies = append(c.Project.RepoPolicies[:idx:idx], c.Project.RepoPolicies[idx+1:]...)
				}
			}
			continue
		}
		// An empty override cannot round-trip through yaml omitempty, so it
		// means "inherit the project default" here.
		var labels []string
		if setting.ApprovalLabels != nil {
			if labels = normalizeLabelList(*setting.ApprovalLabels); len(labels) == 0 {
				labels = nil
			}
		}
		if idx >= 0 {
			c.Project.RepoPolicies[idx].LabelDriven = true
			c.Project.RepoPolicies[idx].LabelDrivenApprovalLabels = labels
		} else {
			c.Project.RepoPolicies = append(c.Project.RepoPolicies, RepoPolicy{Repo: name, LabelDriven: true, LabelDrivenApprovalLabels: labels})
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
	rp, ok := c.RepoPolicyFor(repo)
	if !ok {
		return true
	}
	// An unrecognised merge_strategy is a config error: stop every merge path
	// here rather than fall back to direct (#10886).
	if ValidateRepoMergeStrategy(rp.Repo, rp.MergeStrategy) != nil {
		return false
	}
	if rp.AutoMerge != nil {
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

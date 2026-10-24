package config

import "strings"

// DecisionsConfig (top-level `decisions:`) tunes how decisions on items parked
// at stage/085-needs-decision / 045 are recorded.
type DecisionsConfig struct {
	// Delegate is the "Let hive decide" owner-delegate decider.
	Delegate DelegateDecisionConfig `yaml:"delegate,omitempty" json:"delegate,omitempty"`
}

// DelegateDecisionConfig (`decisions.delegate:`) gates the activity monitor's
// "Let hive decide" action: the hive decides a needs-human item itself under
// the owner-delegate policy (pkg/delegatedecision.PolicyText) and records it
// as a human decision would be. Default ON, the same default-on `*bool`
// pattern as stage adoption and good-first-issue: the owner's standing
// instruction is that the hive decides for him. `enabled: false` turns the
// action off hive-wide (the menu entry greys out and the endpoint answers
// 403); `repos.<owner/repo>.enabled` overrides per repository.
type DelegateDecisionConfig struct {
	// Enabled turns the delegate on. nil = true. Read via IsEnabledFor.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repos holds per-repository overrides keyed by `owner/repo` or bare
	// repo name (matched case-insensitively, exact slug first).
	Repos map[string]DelegateDecisionRepoConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
}

// DelegateDecisionRepoConfig is one repository's override.
type DelegateDecisionRepoConfig struct {
	// Enabled overrides the hive-wide switch for this repository. nil
	// inherits.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports the hive-wide switch (default on).
func (c DelegateDecisionConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// IsEnabledFor reports whether the delegate may decide items in repo: the
// per-repo override when one is configured, else the hive-wide switch. Repo
// may be a bare name or owner/repo.
func (c DelegateDecisionConfig) IsEnabledFor(repo string) bool {
	repo = strings.TrimSpace(repo)
	bare := repo
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	for key, rc := range c.Repos {
		if strings.EqualFold(strings.TrimSpace(key), repo) && rc.Enabled != nil {
			return *rc.Enabled
		}
	}
	for key, rc := range c.Repos {
		kBare := strings.TrimSpace(key)
		if i := strings.LastIndex(kBare, "/"); i >= 0 {
			kBare = kBare[i+1:]
		}
		if bare != "" && strings.EqualFold(kBare, bare) && rc.Enabled != nil {
			return *rc.Enabled
		}
	}
	return c.IsEnabled()
}

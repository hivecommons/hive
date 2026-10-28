package config

import (
	"strings"
	"time"
)

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
	// LLM is the model step for the non-mechanical cases. A pointer so an
	// unset block leaves the serialised config unchanged.
	LLM *DelegateDecisionLLMConfig `yaml:"llm,omitempty" json:"llm,omitempty"`
	// EffectTimeout (`decisions.delegate.effect_timeout`) is how long a
	// recorded delegated decision may go without its expected effect (the
	// stage or label change, merge, close, or fresh PR) before the hive
	// calls it stuck: the row action reports `no-effect` with the plain
	// reason and the concrete options, and a follow-up comment says so on
	// the PR (hivecommons/hive#14822). A Go duration string; default 30m.
	EffectTimeout string `yaml:"effect_timeout,omitempty" json:"effect_timeout,omitempty"`
}

// DefaultDelegateDecisionEffectTimeout is the effect_timeout default.
const DefaultDelegateDecisionEffectTimeout = 30 * time.Minute

// EffectTimeoutDuration parses EffectTimeout, falling back to the default
// when unset or unparsable (a bad value must not make decisions look
// stuck instantly or never).
func (c DelegateDecisionConfig) EffectTimeoutDuration() time.Duration {
	raw := strings.TrimSpace(c.EffectTimeout)
	if raw == "" {
		return DefaultDelegateDecisionEffectTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultDelegateDecisionEffectTimeout
	}
	return d
}

// DelegateDecisionLLMConfig (`decisions.delegate.llm:`) gates the LLM-backed
// owner-delegate decider. It runs only when the rules-first decider falls
// through to the reversible default, and its result goes through the same
// safety rails. Default OFF: the conservative choice until an operator opts
// in. The model call uses the governor reviewer endpoint
// (governor.trajectory / governor.litellm); Model overrides the model.
type DelegateDecisionLLMConfig struct {
	// Enabled turns the LLM step on. nil = false.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Model overrides the reviewer model for the decider turn.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
}

// LLMEnabled reports whether the LLM step is on (default off). The
// hive-wide / per-repo delegate switch still gates the whole action.
func (c DelegateDecisionConfig) LLMEnabled() bool {
	return c.LLM != nil && c.LLM.Enabled != nil && *c.LLM.Enabled
}

// LLMModel is the configured model override, "" when none.
func (c DelegateDecisionConfig) LLMModel() string {
	if c.LLM == nil {
		return ""
	}
	return strings.TrimSpace(c.LLM.Model)
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

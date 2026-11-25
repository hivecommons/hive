package config

import (
	"fmt"
	"strings"
)

// DefaultDecisionGatePaths are the globs (same syntax as
// auto_merge.human_merge_paths and intent.guardrail_path_patterns) whose
// change needs a person's decision on the exact head before any automerge
// lane merges it. They cover credential, keystore, auth and secret code, the
// push broker that hands tokens to git, and the egress rules that decide
// which host a credential reaches.
//
// The set is the safe default for auto_merge.decision_gate.paths: an unset
// key uses it, an explicit list replaces it, and an explicit empty list
// matches nothing (hivecommons/hive: a trusted author's Forgejo clone-and-push
// change merged with no person deciding).
var DefaultDecisionGatePaths = []string{
	// Credentials and secrets
	"**/*credential*", "**/*secret*", "**/*askpass*",
	"**/*.pem", "**/*.key", "**/id_rsa*", "**/.env", "**/.env.*",
	// Keystores and secret stores
	"**/keystore/**", "**/*keystore*", "**/openbao/**", "**/vault/**",
	// Authentication and authorization
	"**/auth/**", "**/authn/**", "**/authz/**", "**/oauth/**", "**/*oauth*", "**/*jwt*",
	// Token minting and the push path that carries tokens to git
	"**/pushbroker/**", "**/*token_source*", "**/*token_store*", "**/*app_token*", "**/*apptoken*",
	"**/gh-app-token*", "**/git-credential*",
	// Egress rules that decide which host a credential reaches
	"**/proxy/rules*", "**/gh-wrapper*", "**/gh_wrapper*",
}

// DecisionGateConfig is auto_merge.decision_gate. A PR that touches one of
// Paths, or that carries the sentinel alert label while Sentinel is on, is
// not merged by any automerge lane (App self-merge, trusted bot, trusted
// author, contributor) until a person decides on that exact head: a trusted
// human's APPROVED review, the dashboard's Approve, or `/hive decision
// approve`. The author's role never counts as that decision, so a trusted or
// owner author's credential change still waits for a person.
//
// Default ON with DefaultDecisionGatePaths.
type DecisionGateConfig struct {
	// Enabled turns the gate off when explicitly false. nil means on.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Paths are the globs that need a decision. nil means
	// DefaultDecisionGatePaths; an explicit empty list matches nothing.
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
	// Sentinel makes the sentinel alert label (sentinel.label) need a
	// decision in every lane, trusted authors included. nil means on.
	Sentinel *bool `yaml:"sentinel,omitempty" json:"sentinel,omitempty"`
}

// IsEnabled reports whether the gate runs (default on).
func (d DecisionGateConfig) IsEnabled() bool {
	return d.Enabled == nil || *d.Enabled
}

// EffectivePaths resolves nil to DefaultDecisionGatePaths. It returns a copy.
func (d DecisionGateConfig) EffectivePaths() []string {
	if d.Paths == nil {
		return append([]string(nil), DefaultDecisionGatePaths...)
	}
	return append(make([]string, 0, len(d.Paths)), d.Paths...)
}

// SentinelNeedsDecision reports whether the sentinel alert label needs a
// decision (default on).
func (d DecisionGateConfig) SentinelNeedsDecision() bool {
	return d.Sentinel == nil || *d.Sentinel
}

// Validate rejects blank patterns, so a typo cannot quietly match nothing.
func (d DecisionGateConfig) Validate() error {
	for _, p := range d.Paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("auto_merge.decision_gate.paths must not contain blank patterns")
		}
	}
	return nil
}

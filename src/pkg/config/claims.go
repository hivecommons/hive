package config

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// ClaimsConfig is `governor.claims` (hivecommons/hive#8380): whether the hive
// recognises and posts ISSUE CLAIMS — the visible, expiring "I am working on
// this" marker on an issue that covers the window before a PR exists.
//
// Off by default. With Enabled false the hive reads no claim markers, posts
// none, withholds nothing on their account and emits no claim fields on any
// listing: an absent block is zero behaviour change.
type ClaimsConfig struct {
	// Enabled turns claim recognition and agent claim posting on.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TTLS is how long a claim stands, in seconds, when the claimant set no
	// explicit expiry: the expiry the hub writes on an agent's claim, and the
	// window an assignee-inferred claim runs from the issue's last activity.
	// Absent or <= 0 → issueclaim.DefaultTTL (4h).
	TTLS int `yaml:"ttl_s,omitempty" json:"ttl_s,omitempty"`
	// HumanTTLS is how long a ranked human claim lives before it lapses unless
	// renewed. 0 falls back to TTLS, then the package default.
	HumanTTLS int `yaml:"human_ttl_s,omitempty" json:"human_ttl_s,omitempty"`
	// AgentTTLS is the lifetime of a claim auto-recorded when the hub kicks one
	// of its own agents on an issue. 0 uses the package default.
	AgentTTLS int `yaml:"agent_ttl_s,omitempty" json:"agent_ttl_s,omitempty"`
	// ContributorTTLS is the lifetime of a relay contributor claim. 0 uses the
	// package default, which matches the relay lease.
	ContributorTTLS int `yaml:"contributor_ttl_s,omitempty" json:"contributor_ttl_s,omitempty"`
	// MaxTTLS caps requested ranked-claim lifetimes. 0 uses the package default.
	MaxTTLS int `yaml:"max_ttl_s,omitempty" json:"max_ttl_s,omitempty"`
	// Comment posts claim / takeover / release comments on the GitHub issue.
	// Nil defaults to on when claims are enabled.
	Comment *bool `yaml:"comment,omitempty" json:"comment,omitempty"`
	// Label adds/removes the claimed and preempted labels. Nil defaults to on
	// when claims are enabled.
	Label *bool `yaml:"label,omitempty" json:"label,omitempty"`
	// EscalateAfterClaims is how many claims one agent may make on an issue
	// with nothing moving in between (no linked PR, referencing commit,
	// label/assignee change or close/reopen) before the next is refused and
	// the issue is labelled needs-human with one summary comment
	// (hivecommons/hive#10527). 0 uses DefaultClaimEscalateAfter; negative
	// turns the gate off.
	EscalateAfterClaims int `yaml:"escalate_after_claims,omitempty" json:"escalate_after_claims,omitempty"`
	// NeverClaimLabels are labels (matched case-insensitively) that mark an
	// issue as not a unit of agent work — umbrella/tracking issues — so every
	// automated claim producer refuses it and the stage engine never stages
	// a claim on it as stage/020 or stage/030. Absent → claims.DefaultNeverClaimLabels
	// (`epic`); an explicit empty list turns the gate off.
	NeverClaimLabels []string `yaml:"never_claim_labels,omitempty" json:"never_claim_labels,omitempty"`
	// ReclaimCooldownS is how long, in seconds, the SAME relay contributor
	// session must wait before it is handed an issue it just released
	// without shipping (task failed, abandoned, requeued, lease revoked)
	// (hivecommons/hive#13093). The per-issue failure cooldown is global
	// and short (10 min) so that a different contributor may retry at
	// once; without a per-contributor window the one whose environment
	// cannot run the task takes the same issue back every ten minutes,
	// flapping its claim. 0 → DefaultReclaimCooldown (6h); negative turns
	// the window off.
	ReclaimCooldownS int `yaml:"reclaim_cooldown_s,omitempty" json:"reclaim_cooldown_s,omitempty"`
	// ContributorFlapThreshold is how many contributor disconnect-releases
	// one issue may take inside contributor_flap_window_s before the issue
	// is withheld from contributor assignment for contributor_flap_cooloff_s
	// and left to the hive's own agents (hivecommons/hive#13579: a relay
	// that connected, was handed the issue, and dropped 18 times in 12h
	// kept the issue claimed and starved the scanner/coder). 0 →
	// DefaultContributorFlapThreshold (3); negative turns the cool-off off.
	ContributorFlapThreshold int `yaml:"contributor_flap_threshold,omitempty" json:"contributor_flap_threshold,omitempty"`
	// ContributorFlapWindowS is the window, in seconds, the drops are
	// counted over. 0 → DefaultContributorFlapWindow (2h).
	ContributorFlapWindowS int `yaml:"contributor_flap_window_s,omitempty" json:"contributor_flap_window_s,omitempty"`
	// ContributorFlapCooloffS is how long, in seconds, the issue stays
	// withheld from contributors once the threshold trips. 0 →
	// DefaultContributorFlapCooloff (6h).
	ContributorFlapCooloffS int `yaml:"contributor_flap_cooloff_s,omitempty" json:"contributor_flap_cooloff_s,omitempty"`
	// ContributorStarvationS is how long, in seconds, an issue may be
	// continuously withheld from the hive's own agents by contributor
	// claims — with no PR opened — before those claims become advisory and
	// a hive agent may claim it anyway (hivecommons/hive#13579). 0 →
	// DefaultContributorStarvation (4h); negative turns the guard off.
	ContributorStarvationS int `yaml:"contributor_starvation_s,omitempty" json:"contributor_starvation_s,omitempty"`
}

// DefaultReclaimCooldown is the reclaim_cooldown_s default.
const DefaultReclaimCooldown = 6 * time.Hour

// Contributor flap cool-off and starvation guard defaults
// (hivecommons/hive#13579).
const (
	DefaultContributorFlapThreshold = 3
	DefaultContributorFlapWindow    = 2 * time.Hour
	DefaultContributorFlapCooloff   = 6 * time.Hour
	DefaultContributorStarvation    = 4 * time.Hour
)

// EffectiveContributorFlap returns the contributor flap cool-off settings
// with defaults applied. A threshold of 0 means the cool-off is off.
func (c ClaimsConfig) EffectiveContributorFlap() (threshold int, window, cooloff time.Duration) {
	switch {
	case c.ContributorFlapThreshold < 0:
		threshold = 0
	case c.ContributorFlapThreshold == 0:
		threshold = DefaultContributorFlapThreshold
	default:
		threshold = c.ContributorFlapThreshold
	}
	window = DefaultContributorFlapWindow
	if c.ContributorFlapWindowS > 0 {
		window = time.Duration(c.ContributorFlapWindowS) * time.Second
	}
	cooloff = DefaultContributorFlapCooloff
	if c.ContributorFlapCooloffS > 0 {
		cooloff = time.Duration(c.ContributorFlapCooloffS) * time.Second
	}
	return threshold, window, cooloff
}

// EffectiveContributorStarvation returns contributor_starvation_s with the
// default applied; 0 means the guard is off.
func (c ClaimsConfig) EffectiveContributorStarvation() time.Duration {
	switch {
	case c.ContributorStarvationS < 0:
		return 0
	case c.ContributorStarvationS == 0:
		return DefaultContributorStarvation
	}
	return time.Duration(c.ContributorStarvationS) * time.Second
}

// EffectiveReclaimCooldown returns reclaim_cooldown_s with the default
// applied; 0 means the window is off.
func (c ClaimsConfig) EffectiveReclaimCooldown() time.Duration {
	switch {
	case c.ReclaimCooldownS < 0:
		return 0
	case c.ReclaimCooldownS == 0:
		return DefaultReclaimCooldown
	}
	return time.Duration(c.ReclaimCooldownS) * time.Second
}

// EffectiveNeverClaimLabels returns never_claim_labels with the default
// applied: nil (key absent) → claims.DefaultNeverClaimLabels; an explicit
// empty list stays empty.
func (c ClaimsConfig) EffectiveNeverClaimLabels() []string {
	if c.NeverClaimLabels == nil {
		return append([]string(nil), claims.DefaultNeverClaimLabels...)
	}
	return append([]string{}, c.NeverClaimLabels...)
}

// DefaultClaimEscalateAfter is the escalate_after_claims default: an agent's
// third claim on an issue its first two moved nothing on is refused.
const DefaultClaimEscalateAfter = 2

// EscalationThreshold returns escalate_after_claims with the default applied;
// 0 means the gate is off.
func (c ClaimsConfig) EscalationThreshold() int {
	switch {
	case c.EscalateAfterClaims < 0:
		return 0
	case c.EscalateAfterClaims == 0:
		return DefaultClaimEscalateAfter
	}
	return c.EscalateAfterClaims
}

// IsEnabled reports whether issue claims are enabled. The governor feature is
// opt-in for v5, preserving the plain issue-claim default from #8397.
func (c ClaimsConfig) IsEnabled() bool { return c.Enabled }

// EffectiveTTL returns ttl_s as a duration with the default applied.
func (c ClaimsConfig) EffectiveTTL() time.Duration {
	if c.TTLS <= 0 {
		return issueclaim.DefaultTTL
	}
	return time.Duration(c.TTLS) * time.Second
}

// CommentEnabled reports whether GitHub issue comments are posted. Default ON.
func (c ClaimsConfig) CommentEnabled() bool { return c.Comment == nil || *c.Comment }

// LabelEnabled reports whether GitHub labels are applied. Default ON.
func (c ClaimsConfig) LabelEnabled() bool { return c.Label == nil || *c.Label }

// TTLs returns ranked-claim lifetimes. Zero durations mean "use the package
// default" in pkg/claims; human_ttl_s falls back to the original ttl_s knob.
func (c ClaimsConfig) TTLs() (human, agent, contributor, max time.Duration) {
	sec := func(n int) time.Duration {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	human = sec(c.HumanTTLS)
	if human == 0 {
		human = sec(c.TTLS)
	}
	return human, sec(c.AgentTTLS), sec(c.ContributorTTLS), sec(c.MaxTTLS)
}

// Policy returns the ledger TTL policy for these lifetimes. The dashboard and
// the config reload apply it to the live ledger, so a change takes effect
// without a restart (hivecommons/hive#10981).
func (c ClaimsConfig) Policy() claims.Policy {
	return claims.PolicyWith(c.TTLs())
}

// EffectiveTTLs returns the ranked-claim lifetimes in force: TTLs with the
// pkg/claims defaults applied.
func (c ClaimsConfig) EffectiveTTLs() (human, agent, contributor, max time.Duration) {
	p := c.Policy()
	return p.TTL[claims.KindHuman], p.TTL[claims.KindAgent], p.TTL[claims.KindContributor], p.MaxTTL
}

// DefaultTTLs returns what each ranked-claim lifetime falls back to when its
// own key is unset: human_ttl_s falls back to ttl_s, then the package default.
func (c ClaimsConfig) DefaultTTLs() (human, agent, contributor, max time.Duration) {
	human = claims.DefaultHumanTTL
	if c.TTLS > 0 {
		human = time.Duration(c.TTLS) * time.Second
	}
	return human, claims.DefaultAgentTTL, claims.DefaultContributorTTL, claims.DefaultMaxTTL
}

// ValidateTTLs rejects a negative lifetime (0 means the default) and a human,
// agent or contributor lifetime above the effective max_ttl_s.
func (c ClaimsConfig) ValidateTTLs() error {
	for _, f := range []struct {
		key string
		v   int
	}{
		{"ttl_s", c.TTLS},
		{"human_ttl_s", c.HumanTTLS},
		{"agent_ttl_s", c.AgentTTLS},
		{"contributor_ttl_s", c.ContributorTTLS},
		{"max_ttl_s", c.MaxTTLS},
	} {
		if f.v < 0 {
			return fmt.Errorf("claims %s must be zero (default) or positive", f.key)
		}
	}
	human, agent, contributor, max := c.EffectiveTTLs()
	for _, f := range []struct {
		key string
		d   time.Duration
	}{
		{"human_ttl_s", human},
		{"agent_ttl_s", agent},
		{"contributor_ttl_s", contributor},
	} {
		if f.d > max {
			return fmt.Errorf("claims %s (%s) must not exceed max_ttl_s (%s)", f.key, f.d, max)
		}
	}
	return nil
}

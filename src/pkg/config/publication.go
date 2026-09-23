package config

import (
	"fmt"
	"strings"
)

// PublicationMinACMMLevel is the first maturity level at which the audit
// campaign's issue publisher may file anything. The ACMM policy matrix grants
// issue writes to the first "measured" agents at L3 (quality-gated); below
// that every agent is advisory and the hive holds no issue-writing authority,
// so the publisher refuses and audits instead of filing.
const PublicationMinACMMLevel = 3

// Private disclosure channel forms accepted by publication.private_channel.
const (
	// PrivateChannelRepoPrefix routes security-sensitive findings to an issue
	// in the named private repository ("repo:owner/name"), filed through the
	// same forge seam as public issues but never in the audited repo itself.
	PrivateChannelRepoPrefix = "repo:"
	// PrivateChannelNotify routes security-sensitive findings to the hive's
	// operator notification channel, carrying only the title and the finding
	// hash so the evidence never leaves the hive.
	PrivateChannelNotify = "notify"

	privateChannelKindRepo   = "repo"
	privateChannelKindNotify = "notify"
)

// PublicationConfig is the operator opt-in for the audit campaign's authorized
// issue publisher (#8353). The zero value keeps publication OFF: findings are
// validated and recorded but nothing is ever filed.
type PublicationConfig struct {
	// Enabled turns the single publication effect on. Even when true, the
	// publisher files only at ACMM PublicationMinACMMLevel and above and only
	// when the convergence mode is enforce; shadow and off never write.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// PrivateChannel is where security-sensitive findings go instead of a
	// public issue: "repo:owner/name" or "notify". A sensitive finding with no
	// channel configured is refused and audited, never filed publicly.
	PrivateChannel string `yaml:"private_channel,omitempty" json:"private_channel,omitempty"`
	// Owner is the campaign owner principal every publication runs under. It
	// is the actor on the outcome ledger and the publication audit trail;
	// empty falls back to the hive's configured owner.
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`
}

// PublicationEnabled reports whether the operator opt-in and the ACMM floor
// both allow publication at the given level. Keeping the floor here means a
// level downgrade safely disables publication without invalidating the
// persisted config or forgetting the operator's preference.
func (p PublicationConfig) PublicationEnabled(acmmLevel int) bool {
	return p.Enabled && acmmLevel >= PublicationMinACMMLevel
}

// PrivateChannelKind classifies the configured channel: "repo" with the
// repository slug, "notify" with an empty target, or "" when no channel is
// configured. An unrecognised form is reported by Validate.
func (p PublicationConfig) PrivateChannelKind() (kind, target string) {
	raw := strings.TrimSpace(p.PrivateChannel)
	switch {
	case raw == "":
		return "", ""
	case raw == PrivateChannelNotify:
		return privateChannelKindNotify, ""
	case strings.HasPrefix(raw, PrivateChannelRepoPrefix):
		return privateChannelKindRepo, strings.TrimSpace(strings.TrimPrefix(raw, PrivateChannelRepoPrefix))
	}
	return "", raw
}

// Validate rejects a private channel this build cannot route to. A channel
// that parses to nothing would make every sensitive finding a refusal while
// the operator believes disclosure is configured.
func (p PublicationConfig) Validate() error {
	raw := strings.TrimSpace(p.PrivateChannel)
	if raw == "" {
		return nil
	}
	kind, target := p.PrivateChannelKind()
	switch kind {
	case privateChannelKindNotify:
		return nil
	case privateChannelKindRepo:
		if target == "" || strings.Count(target, "/") != 1 || strings.ContainsAny(target, "@#! \t") {
			return fmt.Errorf("publication.private_channel %q: repository must be spelled owner/name", raw)
		}
		return nil
	}
	return fmt.Errorf("publication.private_channel %q is not %q<owner/name> or %q", raw, PrivateChannelRepoPrefix, PrivateChannelNotify)
}

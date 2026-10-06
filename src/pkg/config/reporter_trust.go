package config

import (
	"fmt"
	"strings"
)

// ReporterTrustConfig gates agent work on WHO filed an issue, not only on
// what labels it carries (hivecommons/hive#9665).
//
// Today an issue filed by a repository maintainer and an issue filed by a
// first-time stranger are admitted by the same rule. The only defence is the
// require_labels allow-list, which is all-or-nothing: on, maintainers must
// hand-label their own issues before agents touch them; off, at ACMM L6 a
// stranger's request is worked and merged on green CI with no human in the
// loop. This block splits the two:
//
//   - a reporter whose author_association is in TrustedAssociations (or whose
//     login is in TrustedLogins) is admitted by the ordinary rules;
//   - any other reporter's issue must carry one of UntrustedRequireLabels
//     before it becomes actionable — a maintainer triages it first.
//
// It is OPT-IN (Enabled must be true). The zero value changes nothing, for the
// same reason IssueFilterConfig documents: a default-on gate would silently
// idle every existing hive whose repos take issues from the public.
//
// The merge-side half of the same idea — a PR whose rationale traces to an
// unaccepted untrusted reporter's issue is held at every level — lives on
// GitHubConfig.ReporterTrustHold and follows Enabled unless set explicitly.
type ReporterTrustConfig struct {
	// Enabled turns the reporter gate on. Pointer so an omitted key reads as
	// off while an explicit false is preserved on round-trip.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// TrustedAssociations lists the GitHub author_association values that
	// count as trusted. nil/empty → DefaultTrustedAssociations. Matched
	// case-insensitively.
	TrustedAssociations []string `yaml:"trusted_associations,omitempty" json:"trusted_associations,omitempty"`
	// TrustedLogins is an explicit allow-list of logins trusted regardless of
	// association — an external maintainer GitHub reports as NONE, say.
	// Matched case-insensitively by exact login.
	TrustedLogins []string `yaml:"trusted_logins,omitempty" json:"trusted_logins,omitempty"`
	// UntrustedRequireLabels are the labels an issue from any other reporter
	// must carry (at least one) before agents may work it. nil/empty →
	// DefaultUntrustedRequireLabels. Exact, case-insensitive match, for the
	// same over-admission reason IssueFilterConfig gives.
	UntrustedRequireLabels []string `yaml:"untrusted_require_labels,omitempty" json:"untrusted_require_labels,omitempty"`
	// AwaitingLabel is applied to human-filed issues held out by this gate, so
	// the wait is visible in the tracker and dashboard. Empty disables the
	// label; unset uses DefaultReporterTrustAwaitingLabel.
	AwaitingLabel *string `yaml:"awaiting_label,omitempty" json:"awaiting_label,omitempty"`
	// Comment controls the one-shot explanation comment on held-out issues.
	// nil defaults on.
	Comment *bool `yaml:"comment,omitempty" json:"comment,omitempty"`
	// ClankerRequested turns on the opt-in "clanker-requested" policy
	// (hivecommons/hive#10766). nil defaults off. While on, a PR carrying
	// ClankerRequestedLabel counts as held.
	ClankerRequested *bool `yaml:"clanker_requested,omitempty" json:"clanker_requested,omitempty"`
	// ClankerRequestedLabel names the label; unset uses
	// DefaultClankerRequestedLabel.
	ClankerRequestedLabel *string `yaml:"clanker_requested_label,omitempty" json:"clanker_requested_label,omitempty"`
	// ClankerRequestedAddendum is optional free text appended to the policy's
	// explanation.
	ClankerRequestedAddendum string `yaml:"clanker_requested_addendum,omitempty" json:"clanker_requested_addendum,omitempty"`
}

// DefaultClankerRequestedLabel is the label for the clanker-requested policy
// when the operator has not named their own.
const DefaultClankerRequestedLabel = "clanker-requested"

// MaxClankerRequestedAddendumLen caps ClankerRequestedAddendum, in bytes.
const MaxClankerRequestedAddendumLen = 1000

// GitHub's author_association vocabulary, as the REST API spells it.
const (
	AuthorAssociationOwner                = "OWNER"
	AuthorAssociationMember               = "MEMBER"
	AuthorAssociationCollaborator         = "COLLABORATOR"
	AuthorAssociationContributor          = "CONTRIBUTOR"
	AuthorAssociationFirstTimeContributor = "FIRST_TIME_CONTRIBUTOR"
	AuthorAssociationFirstTimer           = "FIRST_TIMER"
	AuthorAssociationNone                 = "NONE"
)

// KnownAuthorAssociations is every value GitHub can report, in the order the
// dashboard renders them. It is the validation set for TrustedAssociations.
var KnownAuthorAssociations = []string{
	AuthorAssociationOwner,
	AuthorAssociationMember,
	AuthorAssociationCollaborator,
	AuthorAssociationContributor,
	AuthorAssociationFirstTimeContributor,
	AuthorAssociationFirstTimer,
	AuthorAssociationNone,
}

// DefaultTrustedAssociations is the trust set when none is configured: the
// people GitHub itself says have a standing relationship with the repository.
// CONTRIBUTOR is deliberately excluded — one merged typo fix does not make a
// stranger a maintainer.
var DefaultTrustedAssociations = []string{
	AuthorAssociationOwner,
	AuthorAssociationMember,
	AuthorAssociationCollaborator,
}

// DefaultUntrustedRequireLabels is the triage label an untrusted reporter's
// issue must carry when the operator has not named their own.
var DefaultUntrustedRequireLabels = []string{"triage/accepted"}

// DefaultReporterTrustAwaitingLabel marks issues waiting for a maintainer to
// admit an untrusted reporter's request into the automated queue.
const DefaultReporterTrustAwaitingLabel = "needs-triage"

// IsEnabled reports whether the reporter gate is on.
func (r ReporterTrustConfig) IsEnabled() bool {
	return r.Enabled != nil && *r.Enabled
}

// EffectiveTrustedAssociations returns the configured trust set or the default.
func (r ReporterTrustConfig) EffectiveTrustedAssociations() []string {
	if len(r.TrustedAssociations) == 0 {
		return append([]string(nil), DefaultTrustedAssociations...)
	}
	return append([]string(nil), r.TrustedAssociations...)
}

// EffectiveUntrustedRequireLabels returns the configured triage labels or the
// default.
func (r ReporterTrustConfig) EffectiveUntrustedRequireLabels() []string {
	if len(r.UntrustedRequireLabels) == 0 {
		return append([]string(nil), DefaultUntrustedRequireLabels...)
	}
	return append([]string(nil), r.UntrustedRequireLabels...)
}

// EffectiveAwaitingLabel returns the configured waiting label, the default, or
// "" when the label has been explicitly disabled.
func (r ReporterTrustConfig) EffectiveAwaitingLabel() string {
	if r.AwaitingLabel != nil {
		return strings.TrimSpace(*r.AwaitingLabel)
	}
	return DefaultReporterTrustAwaitingLabel
}

// CommentOn reports whether Hive should post the one-shot reporter-trust wait
// explanation. The default is on.
func (r ReporterTrustConfig) CommentOn() bool {
	return r.Comment == nil || *r.Comment
}

// ClankerRequestedOn reports whether the clanker-requested policy is on. The
// default is off.
func (r ReporterTrustConfig) ClankerRequestedOn() bool {
	return r.ClankerRequested != nil && *r.ClankerRequested
}

// EffectiveClankerRequestedLabel returns the configured label or the default.
func (r ReporterTrustConfig) EffectiveClankerRequestedLabel() string {
	if r.ClankerRequestedLabel != nil {
		return strings.TrimSpace(*r.ClankerRequestedLabel)
	}
	return DefaultClankerRequestedLabel
}

// ExtraHoldLabels returns the labels this block adds to the hold-label set:
// the clanker-requested label while that policy is on, otherwise none.
func (r ReporterTrustConfig) ExtraHoldLabels() []string {
	if !r.ClankerRequestedOn() {
		return nil
	}
	if l := r.EffectiveClankerRequestedLabel(); l != "" {
		return []string{l}
	}
	return nil
}

// Trusted reports whether a reporter is trusted: by explicit login first, then
// by association. An empty login with an empty association is NOT trusted —
// "we could not tell who filed this" must fail toward the triage path, the
// same tri-state caution the #5117 gate applies to unknown authors.
func (r ReporterTrustConfig) Trusted(login, association string) bool {
	login = strings.TrimSpace(login)
	for _, l := range r.TrustedLogins {
		if login != "" && strings.EqualFold(strings.TrimSpace(l), login) {
			return true
		}
	}
	association = strings.TrimSpace(association)
	if association == "" {
		return false
	}
	for _, a := range r.EffectiveTrustedAssociations() {
		if strings.EqualFold(strings.TrimSpace(a), association) {
			return true
		}
	}
	return false
}

// Equal reports whether two reporter-trust blocks are identical (order-
// sensitive, exact), for the heartbeat reconcile.
func (r ReporterTrustConfig) Equal(o ReporterTrustConfig) bool {
	if (r.Enabled == nil) != (o.Enabled == nil) {
		return false
	}
	if r.Enabled != nil && *r.Enabled != *o.Enabled {
		return false
	}
	return equalStringSlices(r.TrustedAssociations, o.TrustedAssociations) &&
		equalStringSlices(r.TrustedLogins, o.TrustedLogins) &&
		equalStringSlices(r.UntrustedRequireLabels, o.UntrustedRequireLabels) &&
		stringPtrEqual(r.AwaitingLabel, o.AwaitingLabel) &&
		boolPtrEqual(r.Comment, o.Comment) &&
		boolPtrEqual(r.ClankerRequested, o.ClankerRequested) &&
		stringPtrEqual(r.ClankerRequestedLabel, o.ClankerRequestedLabel) &&
		r.ClankerRequestedAddendum == o.ClankerRequestedAddendum
}

// IsZero reports whether the block is entirely absent.
func (r ReporterTrustConfig) IsZero() bool {
	return r.Enabled == nil && len(r.TrustedAssociations) == 0 && len(r.TrustedLogins) == 0 && len(r.UntrustedRequireLabels) == 0 &&
		r.AwaitingLabel == nil && r.Comment == nil &&
		r.ClankerRequested == nil && r.ClankerRequestedLabel == nil && r.ClankerRequestedAddendum == ""
}

// ValidateReporterTrust rejects association names GitHub never reports, so a
// typo cannot silently widen or narrow the trust set.
func ValidateReporterTrust(r ReporterTrustConfig) error {
	for _, a := range r.TrustedAssociations {
		known := false
		for _, k := range KnownAuthorAssociations {
			if strings.EqualFold(strings.TrimSpace(a), k) {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("reporter_trust.trusted_associations: %q is not a GitHub author association (one of %s)",
				a, strings.Join(KnownAuthorAssociations, ", "))
		}
	}
	for _, l := range r.UntrustedRequireLabels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("reporter_trust.untrusted_require_labels: empty label")
		}
	}
	if r.ClankerRequestedLabel != nil && strings.TrimSpace(*r.ClankerRequestedLabel) == "" {
		return fmt.Errorf("reporter_trust.clanker_requested_label: empty label")
	}
	if len(r.ClankerRequestedAddendum) > MaxClankerRequestedAddendumLen {
		return fmt.Errorf("reporter_trust.clanker_requested_addendum: %d bytes exceeds the %d-byte limit",
			len(r.ClankerRequestedAddendum), MaxClankerRequestedAddendumLen)
	}
	return nil
}

func stringPtrEqual(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}

func boolPtrEqual(a, b *bool) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

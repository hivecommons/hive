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
// untrusted reporter's issue is held at every level — lives on
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
	// AwaitingTriageLabel is applied to human-filed issues held out by this
	// gate, so the wait is visible in the tracker and dashboard. Empty uses
	// DefaultAwaitingTriageLabel.
	AwaitingTriageLabel string `yaml:"awaiting_triage_label,omitempty" json:"awaiting_triage_label,omitempty"`
	// AwaitingTriageLabelEnabled controls whether Hive applies/removes the
	// awaiting label. nil defaults on.
	AwaitingTriageLabelEnabled *bool `yaml:"awaiting_triage_label_enabled,omitempty" json:"awaiting_triage_label_enabled,omitempty"`
	// Comment controls the one-shot explanation comment on held-out issues.
	// nil defaults on.
	Comment *bool `yaml:"comment,omitempty" json:"comment,omitempty"`
}

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

// DefaultAwaitingTriageLabel marks issues waiting for a maintainer to admit an
// untrusted reporter's request into the automated queue.
const DefaultAwaitingTriageLabel = "hive/awaiting-triage"

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

// AwaitingTriageLabelOn reports whether Hive should apply/remove the visible
// waiting-for-triage label. The default is on.
func (r ReporterTrustConfig) AwaitingTriageLabelOn() bool {
	return r.AwaitingTriageLabelEnabled == nil || *r.AwaitingTriageLabelEnabled
}

// EffectiveAwaitingTriageLabel returns the configured waiting label or the
// default. It returns "" when the label is disabled.
func (r ReporterTrustConfig) EffectiveAwaitingTriageLabel() string {
	if !r.AwaitingTriageLabelOn() {
		return ""
	}
	if label := strings.TrimSpace(r.AwaitingTriageLabel); label != "" {
		return label
	}
	return DefaultAwaitingTriageLabel
}

// CommentOn reports whether Hive should post the one-shot reporter-trust wait
// explanation. The default is on.
func (r ReporterTrustConfig) CommentOn() bool {
	return r.Comment == nil || *r.Comment
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
		r.AwaitingTriageLabel == o.AwaitingTriageLabel &&
		boolPtrEqual(r.AwaitingTriageLabelEnabled, o.AwaitingTriageLabelEnabled) &&
		boolPtrEqual(r.Comment, o.Comment)
}

// IsZero reports whether the block is entirely absent.
func (r ReporterTrustConfig) IsZero() bool {
	return r.Enabled == nil && len(r.TrustedAssociations) == 0 && len(r.TrustedLogins) == 0 && len(r.UntrustedRequireLabels) == 0 &&
		strings.TrimSpace(r.AwaitingTriageLabel) == "" && r.AwaitingTriageLabelEnabled == nil && r.Comment == nil
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
	if strings.TrimSpace(r.AwaitingTriageLabel) == "" && r.AwaitingTriageLabel != "" {
		return fmt.Errorf("reporter_trust.awaiting_triage_label: empty label")
	}
	return nil
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

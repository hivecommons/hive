package config

import (
	"strings"
)

// IssueFilterConfig gates which open issues agents may INITIATE work on: an
// issue is eligible only if it carries at least one of RequireLabels (the
// allow-list polarity). It exists for projects whose maintainers gate
// automation on an explicit approval label: without an allowlist, a hive works
// every actionable issue in its repos, which surprises owners who expected
// "only issues I have labeled for automation".
//
// This is deliberately ONLY the require half. The EXCLUDE polarity ("never
// touch issues labeled X") already exists as governor.labels.exempt — the
// dashboard's Settings → Labels tab — plus the permanent
// hold/do-not-merge labels. There is exactly one exclusion mechanism and one
// require mechanism; this type does not duplicate the former. Precedence is
// by construction: fetchIssues applies hold/exempt BEFORE this filter, so an
// issue that is both exempt and approval-labeled stays excluded.
//
// The filter is enforced at enumeration (github.Client.fetchIssues) — the
// same choice point as the hold/exempt checks and upstream of the
// duplicate-PR claim guard — so a refused issue never enters the actionable
// set, never reaches a kick prompt, and cannot be re-selected by a confused
// or prompt-injected agent re-listing the repo.
//
// Semantics:
//   - Absent/empty: current behavior EXACTLY — every issue is eligible. There
//     is no default-on filtering; that would silently idle every existing
//     hive whose repos use no approval label.
//   - Non-empty RequireLabels: an issue is eligible only if it carries AT
//     LEAST ONE of these labels (case-insensitive exact match).
//
// Matching is deliberately EXACT (case-insensitive), not substring/prefix:
// a require-gate that prefix-matched would over-admit ("approved-for-review"
// admitting under "approved"), which is the unsafe direction for an approval
// gate. The exempt/hold checks keep their own looser matching; this filter
// does not change them.
const (
	DefaultNeedsHumanLabel     = "needs-human"
	DefaultNeedsDirectionLabel = "needs-direction"
	DefaultNeedsDecisionLabel  = "needs-decision"
	DefaultNeedsSpecLabel      = "needs-spec"
)

// HardSuppressLabelsConfig names label buckets that remove issues from the
// actionable queue because a maintainer must supply input first. Empty slices
// use the built-in default for that bucket, preserving existing hives while
// allowing owners to replace the labels from Settings → Labels.
type HardSuppressLabelsConfig struct {
	NeedsHuman     []string `yaml:"needs_human,omitempty" json:"needs_human,omitempty"`
	NeedsDirection []string `yaml:"needs_direction,omitempty" json:"needs_direction,omitempty"`
	NeedsDecision  []string `yaml:"needs_decision,omitempty" json:"needs_decision,omitempty"`
	NeedsSpec      []string `yaml:"needs_spec,omitempty" json:"needs_spec,omitempty"`
}

func (h HardSuppressLabelsConfig) IsZero() bool {
	return len(h.NeedsHuman) == 0 && len(h.NeedsDirection) == 0 && len(h.NeedsDecision) == 0 && len(h.NeedsSpec) == 0
}

func (h HardSuppressLabelsConfig) EffectiveNeedsHuman() []string {
	return defaultedLabels(h.NeedsHuman, DefaultNeedsHumanLabel)
}

func (h HardSuppressLabelsConfig) EffectiveNeedsDirection() []string {
	return defaultedLabels(h.NeedsDirection, DefaultNeedsDirectionLabel)
}

func (h HardSuppressLabelsConfig) EffectiveNeedsDecision() []string {
	return defaultedLabels(h.NeedsDecision, DefaultNeedsDecisionLabel)
}

func (h HardSuppressLabelsConfig) EffectiveNeedsSpec() []string {
	return defaultedLabels(h.NeedsSpec, DefaultNeedsSpecLabel)
}

func defaultedLabels(values []string, def string) []string {
	if len(values) == 0 {
		return []string{def}
	}
	return append([]string(nil), values...)
}

type IssueFilterConfig struct {
	RequireLabels      []string                 `yaml:"require_labels,omitempty" json:"require_labels,omitempty"`
	HardSuppressLabels HardSuppressLabelsConfig `yaml:"hard_suppress_labels,omitempty" json:"hard_suppress_labels,omitempty"`
	// ReporterTrust gates admission on who filed the issue (#9665): a trusted
	// reporter's issue passes; anyone else's must carry a triage label. It
	// runs BEFORE RequireLabels, which still applies to everything afterwards,
	// so "everyone needs triage" stays expressible exactly as before.
	ReporterTrust ReporterTrustConfig `yaml:"reporter_trust,omitempty" json:"reporter_trust,omitempty"`
}

// IsZero reports whether the filter is absent/empty — i.e. no filtering at all.
func (f IssueFilterConfig) IsZero() bool {
	return len(f.RequireLabels) == 0 && f.HardSuppressLabels.IsZero() && f.ReporterTrust.IsZero()
}

// Equal reports whether two filters are identical (order-sensitive, exact).
// Used by the heartbeat reconcile to decide whether a hub push changes anything.
func (f IssueFilterConfig) Equal(o IssueFilterConfig) bool {
	if len(f.RequireLabels) != len(o.RequireLabels) {
		return false
	}
	for i := range f.RequireLabels {
		if f.RequireLabels[i] != o.RequireLabels[i] {
			return false
		}
	}
	if !equalStringSlices(f.HardSuppressLabels.NeedsHuman, o.HardSuppressLabels.NeedsHuman) ||
		!equalStringSlices(f.HardSuppressLabels.NeedsDirection, o.HardSuppressLabels.NeedsDirection) ||
		!equalStringSlices(f.HardSuppressLabels.NeedsDecision, o.HardSuppressLabels.NeedsDecision) ||
		!equalStringSlices(f.HardSuppressLabels.NeedsSpec, o.HardSuppressLabels.NeedsSpec) {
		return false
	}
	return f.ReporterTrust.Equal(o.ReporterTrust)
}

// labelMatches reports a case-insensitive exact match, ignoring surrounding
// whitespace on the configured side (a stray space in YAML must not silently
// disable an approval gate).
func labelMatches(configured, actual string) bool {
	return strings.EqualFold(strings.TrimSpace(configured), actual)
}

// Admits reports whether an issue carrying the given labels is eligible for
// agent work under this filter. An empty filter admits everything (see the
// type comment for the full contract). Exclusion is not this method's job:
// governor.labels.exempt and the hold labels are applied by the caller before
// this filter, which is what makes "exclude wins" hold by construction.
func (f IssueFilterConfig) Admits(labels []string) bool {
	if len(f.RequireLabels) == 0 {
		return true
	}
	return anyLabelMatches(f.RequireLabels, labels)
}

// AdmitsReporter is the reporter-trust half of admission (#9665). With the
// gate off it admits everyone; with it on, a trusted reporter passes and any
// other reporter's issue must carry one of the untrusted-require labels. The
// caller (github.Client.fetchIssues) applies this BEFORE Admits, so the
// ordinary require_labels allow-list still applies to trusted reporters too.
func (f IssueFilterConfig) AdmitsReporter(labels []string, login, association string) bool {
	if !f.ReporterTrust.IsEnabled() {
		return true
	}
	if f.ReporterTrust.Trusted(login, association) {
		return true
	}
	return anyLabelMatches(f.ReporterTrust.EffectiveUntrustedRequireLabels(), labels)
}

// ReporterTrustEnabled satisfies github.ReporterAdmitter so the client can
// count triage-pending issues separately from ordinary filter refusals.
func (f IssueFilterConfig) ReporterTrustEnabled() bool {
	return f.ReporterTrust.IsEnabled()
}

// ReporterTrustTrustedAssociationsForNotice returns the association set to
// name in user-facing wait comments.
func (f IssueFilterConfig) ReporterTrustTrustedAssociationsForNotice() []string {
	return f.ReporterTrust.EffectiveTrustedAssociations()
}

// ReporterTrustRequiredLabelsForNotice returns the labels that admit an
// untrusted reporter's issue.
func (f IssueFilterConfig) ReporterTrustRequiredLabelsForNotice() []string {
	return f.ReporterTrust.EffectiveUntrustedRequireLabels()
}

// ReporterTrustAwaitingLabel returns the visible wait label, or "" when
// that label is disabled.
func (f IssueFilterConfig) ReporterTrustAwaitingLabel() string {
	return f.ReporterTrust.EffectiveAwaitingLabel()
}

// ReporterTrustCommentEnabled reports whether the one-shot wait explanation
// should be posted.
func (f IssueFilterConfig) ReporterTrustCommentEnabled() bool {
	return f.ReporterTrust.CommentOn()
}

func (f IssueFilterConfig) HardSuppressIssueBucket(labels []string) string {
	for _, actual := range labels {
		for _, configured := range f.HardSuppressLabels.EffectiveNeedsHuman() {
			if labelMatches(configured, actual) {
				return DefaultNeedsHumanLabel
			}
		}
		for _, configured := range f.HardSuppressLabels.EffectiveNeedsDirection() {
			if labelMatches(configured, actual) {
				return DefaultNeedsDirectionLabel
			}
		}
		for _, configured := range f.HardSuppressLabels.EffectiveNeedsDecision() {
			if labelMatches(configured, actual) {
				return DefaultNeedsDecisionLabel
			}
		}
		for _, configured := range f.HardSuppressLabels.EffectiveNeedsSpec() {
			if labelMatches(configured, actual) {
				return DefaultNeedsSpecLabel
			}
		}
	}
	return ""
}

func anyLabelMatches(required, labels []string) bool {
	for _, l := range labels {
		for _, req := range required {
			if labelMatches(req, l) {
				return true
			}
		}
	}
	return false
}

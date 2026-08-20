package config

import "strings"

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
type IssueFilterConfig struct {
	RequireLabels []string `yaml:"require_labels,omitempty" json:"require_labels,omitempty"`
}

// IsZero reports whether the filter is absent/empty — i.e. no filtering at all.
func (f IssueFilterConfig) IsZero() bool {
	return len(f.RequireLabels) == 0
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
	return true
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
	for _, l := range labels {
		for _, req := range f.RequireLabels {
			if labelMatches(req, l) {
				return true
			}
		}
	}
	return false
}

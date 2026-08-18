package config

import "strings"

// IssueFilterConfig gates which open issues agents may INITIATE work on, by
// label. It exists for projects whose maintainers gate automation on an
// explicit approval label: without an allowlist, a hive works every actionable
// issue in its repos, which surprises owners who expected "only issues I have
// labeled for automation". The filter is enforced at enumeration
// (github.Client.fetchIssues) — the same choice point where hold/do-not-merge
// labels and the duplicate-PR claim guard operate — so a filtered issue never
// enters the actionable set, never reaches a kick prompt, and cannot be
// re-selected by a confused or prompt-injected agent re-listing the repo.
//
// Semantics:
//   - Absent/empty filter: current behavior EXACTLY — every issue is eligible.
//     There is no default-on filtering; that would silently idle every
//     existing hive whose repos use no approval label.
//   - RequireLabels: when non-empty, an issue is eligible only if it carries
//     AT LEAST ONE of these labels (case-insensitive exact match).
//   - ExcludeLabels: an issue carrying ANY of these labels is never eligible.
//     Exclusion wins over require on conflict.
//
// Matching is deliberately EXACT (case-insensitive), not substring/prefix:
// a require-gate that prefix-matched would over-admit ("approved-for-review"
// admitting under "approved"), which is the unsafe direction for an approval
// gate. The pre-existing hold/exempt label checks keep their own looser
// matching; this filter does not change them.
type IssueFilterConfig struct {
	RequireLabels []string `yaml:"require_labels,omitempty" json:"require_labels,omitempty"`
	ExcludeLabels []string `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
}

// IsZero reports whether the filter is absent/empty — i.e. no filtering at all.
func (f IssueFilterConfig) IsZero() bool {
	return len(f.RequireLabels) == 0 && len(f.ExcludeLabels) == 0
}

// Equal reports whether two filters are identical (order-sensitive, exact).
// Used by the heartbeat reconcile to decide whether a hub push changes anything.
func (f IssueFilterConfig) Equal(o IssueFilterConfig) bool {
	if len(f.RequireLabels) != len(o.RequireLabels) || len(f.ExcludeLabels) != len(o.ExcludeLabels) {
		return false
	}
	for i := range f.RequireLabels {
		if f.RequireLabels[i] != o.RequireLabels[i] {
			return false
		}
	}
	for i := range f.ExcludeLabels {
		if f.ExcludeLabels[i] != o.ExcludeLabels[i] {
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
// agent work under this filter. Exclusion wins over require; an empty filter
// admits everything (see the type comment for the full contract).
func (f IssueFilterConfig) Admits(labels []string) bool {
	for _, l := range labels {
		for _, ex := range f.ExcludeLabels {
			if labelMatches(ex, l) {
				return false
			}
		}
	}
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

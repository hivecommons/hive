package config

import "strings"

// ReviewRequireLabelsSkipReason is the one phrase every surface uses when
// review.require_labels keeps a PR out of review, so a kick list, the relay's
// refusal and the governor log all read the same.
const ReviewRequireLabelsSkipReason = "review.require_labels: PR carries none of the approval labels"

// NormalizedRequireLabels returns review.require_labels trimmed and without
// blanks or duplicates, in the operator's order. An all-blank list is nil,
// which every caller reads as "gate off".
func (r ReviewConfig) NormalizedRequireLabels() []string {
	if len(r.RequireLabels) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.RequireLabels))
	seen := map[string]bool{}
	for _, l := range r.RequireLabels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		key := strings.ToLower(l)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RequireLabelsEnabled reports whether the PR review label gate is on.
func (r ReviewConfig) RequireLabelsEnabled() bool {
	return len(r.NormalizedRequireLabels()) > 0
}

// AdmitsPRLabels is the review-side counterpart of IssueFilterConfig.Admits:
// with review.require_labels empty every PR is admitted; otherwise the PR must
// carry at least one of the configured labels (case-insensitive exact match,
// never prefix — "approved-for-review" must not admit under "approved").
func (r ReviewConfig) AdmitsPRLabels(labels []string) bool {
	required := r.NormalizedRequireLabels()
	if len(required) == 0 {
		return true
	}
	return anyLabelMatches(required, labels)
}

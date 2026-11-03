package claims

import "strings"

// Never-claim labels: an issue wearing one is not a unit of agent work, so no
// automated claim producer — the relay `claim` request, the start-signal seam,
// the contributor lane — may record a claim on it, and the stage engine never
// derives stage/020 or stage/030 from a claim that somehow exists on one.
//
// The default set is the umbrella/tracking marker (`epic`, matched
// case-insensitively so GitHub's `Epic` counts): an epic is the list of work,
// not the work, and hivecommons/hive#12999 wore stage/030-pr-in-progress with
// hive/claimed-by-scanner because the scanner's verification label on it was
// taken as a start signal. Operators extend or replace the set with
// `governor.claims.never_claim_labels`.

// DefaultNeverClaimLabels is the never-claim label set when the config sets
// none.
var DefaultNeverClaimLabels = []string{"epic"}

// NeverClaimLabel returns the first of labels that is a never-claim label
// under never (nil → DefaultNeverClaimLabels), spelled as the issue carries
// it, or "" when none is. Matching trims whitespace and ignores case.
func NeverClaimLabel(labels, never []string) string {
	if never == nil {
		never = DefaultNeverClaimLabels
	}
	for _, l := range labels {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" {
			continue
		}
		for _, n := range never {
			if n = strings.TrimSpace(n); n != "" && strings.EqualFold(trimmed, n) {
				return trimmed
			}
		}
	}
	return ""
}

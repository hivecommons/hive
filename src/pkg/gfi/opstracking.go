package gfi

import (
	"regexp"
	"strings"
	"time"
)

// Ops-tracking and time-gated issues (hivecommons/hive#13267).
//
// A maintainer's post-hoc soak issue ("24h after promotion: hub dashboard
// shows hosted stable hives healthy…") is a checklist only the operator can
// tick, and only after a deadline. It is not newcomer work: hivecommons/hive
// #13267 was stamped `good first issue` eight minutes after a contributor's
// agent had already returned the verdict "blocked: awaiting the 24-hour
// checks" and the `blocked` label was on it. Two signals keep the picker
// off such issues:
//
//   - OpsTrackingMarker, a structured marker the promote-stable workflow
//     stamps into the follow-up issue body, with a phrase heuristic for
//     issues written by hand before the marker existed; and
//   - RecheckAfterMarker, a deadline the same workflow emits at
//     promotion+soak so the hive knows when the checklist becomes checkable.
const (
	// OpsTrackingMarker marks an operations tracking issue: a maintainer's
	// checklist (soak evidence, rollout confirmation) rather than a code
	// change anyone can pick up.
	OpsTrackingMarker = "<!-- hive:ops-tracking -->"
	// recheckAfterPrefix opens the deadline marker; the timestamp is
	// RFC 3339 in UTC, e.g. `<!-- hive:recheck-after 2026-10-21T21:20:00Z -->`.
	recheckAfterPrefix = "<!-- hive:recheck-after "

	// SkipOpsTracking / SkipTimeGated are the Decision.Skip reasons.
	SkipOpsTracking = "ops-tracking"
	SkipTimeGated   = "time-gated"
)

var (
	// opsTrackingMarkerRE tolerates whitespace variants of the marker.
	opsTrackingMarkerRE = regexp.MustCompile(`<!--\s*hive:ops-tracking\s*-->`)
	// recheckAfterRE captures the deadline; RFC 3339 with or without seconds
	// and with a Z or numeric offset.
	recheckAfterRE = regexp.MustCompile(`<!--\s*hive:recheck-after\s+(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2})?(?:Z|[+-]\d{2}:\d{2}))\s*-->`)
	// opsTrackingPhrases are the soak-checklist shapes a hand-written
	// tracking issue uses: a wait expressed in hours after a rollout event,
	// or the promote-stable vocabulary itself.
	opsTrackingPhrases = regexp.MustCompile(`(?i)\b\d+\s*h(?:ours?|rs?)?\s+after\s+(?:the\s+)?(?:promotion|promoting|rollout|roll-out|rolling out|deploy\w*|release|merge)\b|\bpost-hoc soak\b|\bsoak evidence\b|\bpromote-stable\b|\bstable promotion\b`)
)

// RecheckAfterMarker renders the deadline marker for at (UTC, RFC 3339).
func RecheckAfterMarker(at time.Time) string {
	return recheckAfterPrefix + at.UTC().Format(time.RFC3339) + " -->"
}

// ParseRecheckAfter returns the first recheck-after deadline in text.
func ParseRecheckAfter(text string) (time.Time, bool) {
	m := recheckAfterRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		if t, err := time.Parse(layout, m[1]); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// HasOpsTrackingMarker reports whether text carries OpsTrackingMarker.
func HasOpsTrackingMarker(text string) bool { return opsTrackingMarkerRE.MatchString(text) }

// OpsTrackingReason classifies an issue as operations tracking. It returns
// "marker" for the structured marker, "soak-checklist" when the title or
// body uses the soak-checklist phrasing, and ok=false otherwise.
func OpsTrackingReason(title, body string) (reason string, ok bool) {
	if HasOpsTrackingMarker(body) {
		return "marker", true
	}
	text := strings.TrimSpace(title) + "\n" + body
	if opsTrackingPhrases.MatchString(text) {
		return "soak-checklist", true
	}
	return "", false
}

// withdrawLabels are the exclusion labels that mean "waiting on time or
// something outside the issue": an issue wearing one of them alongside
// `good first issue` was mislabelled (or became blocked after the label
// went on) and the advertisement is withdrawn, not merely skipped.
var withdrawLabels = map[string]bool{"blocked": true, "hold": true}

// Withdrawable reports whether a Decision.Skip reason means a labelled
// issue must have its advertisement withdrawn: parked on a person
// (hard-suppress), blocked or held, an ops-tracking checklist, a
// tracking/archive issue, or
// time-gated. Softer reasons (too fresh, low score, an epic a maintainer
// labelled by hand) leave a hand-applied label alone.
func Withdrawable(skip string) bool {
	switch {
	case strings.HasPrefix(skip, "hard-suppress:"):
		return true
	case skip == SkipTimeGated, strings.HasPrefix(skip, SkipOpsTracking), strings.HasPrefix(skip, SkipTracking+":"):
		return true
	case strings.HasPrefix(skip, "excluded-label:"):
		return withdrawLabels[strings.ToLower(strings.TrimPrefix(skip, "excluded-label:"))]
	}
	return false
}

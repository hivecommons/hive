package gfi

import (
	"regexp"
	"strings"
)

// Tracking / archive issues (hivecommons/hive#14613).
//
// A persistent tracking issue — a weekly report archive, a standing log a
// workflow appends to — is not work: nothing a PR could merge finishes it,
// and its body usually says so ("Keep this issue open"). hivecommons/hive
// #14613 carried exactly that sentence, was still stamped `good first issue`
// + `help wanted`, drew a junk "Closes #14613" PR from a stranger, and was
// then closed by close-on-merge as "Fixed by #14615". Three signals mark an
// issue as tracking; any one is enough:
//
//   - TrackingLabel on the issue;
//   - TrackingMarker in the body (what hive-owned workflows stamp); or
//   - the literal keep-open sentence in the body, for issues written by
//     hand before the marker existed.
const (
	// TrackingLabel marks a persistent tracking/archive issue.
	TrackingLabel = "hive/tracking"
	// TrackingLabelColor seeds the label when a repository lacks it.
	TrackingLabelColor = "c5def5"
	// TrackingLabelDescription seeds the label when a repository lacks it.
	TrackingLabelDescription = "Persistent tracking/archive issue: not work, never auto-closed"
	// TrackingMarker is the structured body marker.
	TrackingMarker = "<!-- hive:tracking -->"
	// TrackingKeepOpenSentence is the prose form the marker replaces.
	TrackingKeepOpenSentence = "Keep this issue open"

	// SkipTracking is the Decision.Skip reason (and the enumeration's
	// filtered reason) for a tracking issue.
	SkipTracking = "tracking"
)

var (
	// trackingMarkerRE tolerates whitespace variants of the marker.
	trackingMarkerRE = regexp.MustCompile(`<!--\s*hive:tracking\s*-->`)
	// trackingKeepOpenRE matches the keep-open sentence as a whole phrase,
	// case-insensitively, so a quoted or re-flowed copy still counts.
	trackingKeepOpenRE = regexp.MustCompile(`(?i)\bkeep\s+this\s+issue\s+open\b`)
)

// HasTrackingMarker reports whether text carries TrackingMarker.
func HasTrackingMarker(text string) bool { return trackingMarkerRE.MatchString(text) }

// TrackingReason classifies an issue as a tracking/archive issue. It
// returns "label" for TrackingLabel, "marker" for TrackingMarker in the
// body, "keep-open" for the prose sentence, and ok=false otherwise.
func TrackingReason(labels []string, body string) (reason string, ok bool) {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), TrackingLabel) {
			return "label", true
		}
	}
	if HasTrackingMarker(body) {
		return "marker", true
	}
	if trackingKeepOpenRE.MatchString(body) {
		return "keep-open", true
	}
	return "", false
}

// IsTracking reports whether the issue is a tracking/archive issue.
func IsTracking(labels []string, body string) bool {
	_, ok := TrackingReason(labels, body)
	return ok
}

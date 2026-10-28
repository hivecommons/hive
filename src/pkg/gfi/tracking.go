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
//
// GitHub Agentic Workflows (gh-aw) tracking issues (hivecommons/hive#14646)
// are recognised without any hive label: gh-aw stamps `<!-- gh-aw-… -->`
// markers (`gh-aw-detection-runs`, `gh-aw-expires: …`) and the boilerplate
// "This issue is automatically managed by GitHub Agentic Workflows. Do not
// close this issue manually. … Do not assign to an agent." into the body,
// and titles the issue `[aw] …` under the `agentic-workflows` label. Any of
// the markers, any of the three sentences, or the label together with the
// title prefix is enough.
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

	// GHAWLabel is the label GitHub Agentic Workflows puts on the issues it
	// manages; GHAWTitlePrefix is the title prefix it uses for them.
	GHAWLabel       = "agentic-workflows"
	GHAWTitlePrefix = "[aw]"
)

var (
	// trackingMarkerRE tolerates whitespace variants of the marker.
	trackingMarkerRE = regexp.MustCompile(`<!--\s*hive:tracking\s*-->`)
	// trackingKeepOpenRE matches the keep-open sentence as a whole phrase,
	// case-insensitively, so a quoted or re-flowed copy still counts.
	trackingKeepOpenRE = regexp.MustCompile(`(?i)\bkeep\s+this\s+issue\s+open\b`)
	// ghawMarkerRE matches any gh-aw HTML comment marker, with or without a
	// payload: `<!-- gh-aw-detection-runs -->`, `<!-- gh-aw-expires: … -->`.
	ghawMarkerRE = regexp.MustCompile(`<!--\s*gh-aw-[a-z0-9-]+\b[^>]*-->`)
	// ghawManagedRE matches the sentences gh-aw writes into every issue it
	// manages, case-insensitively and tolerant of re-flowed whitespace.
	ghawManagedRE = regexp.MustCompile(`(?i)\bautomatically\s+managed\s+by\s+GitHub\s+Agentic\s+Workflows\b|\bdo\s+not\s+close\s+this\s+issue\s+manually\b|\bdo\s+not\s+assign\s+to\s+an\s+agent\b`)
)

// HasTrackingMarker reports whether text carries TrackingMarker.
func HasTrackingMarker(text string) bool { return trackingMarkerRE.MatchString(text) }

// HasGHAWMarker reports whether text carries any `<!-- gh-aw-… -->` marker.
func HasGHAWMarker(text string) bool { return ghawMarkerRE.MatchString(text) }

// TrackingReason classifies an issue as a tracking/archive issue. It
// returns "label" for TrackingLabel, "marker" for TrackingMarker in the
// body, "keep-open" for the prose sentence, "gh-aw-marker" for a gh-aw
// body marker, "gh-aw-managed" for the gh-aw boilerplate sentences,
// "gh-aw-label" for the GHAWLabel label on a GHAWTitlePrefix title, and
// ok=false otherwise.
func TrackingReason(title string, labels []string, body string) (reason string, ok bool) {
	ghawLabel := false
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if strings.EqualFold(l, TrackingLabel) {
			return "label", true
		}
		if strings.EqualFold(l, GHAWLabel) {
			ghawLabel = true
		}
	}
	if HasTrackingMarker(body) {
		return "marker", true
	}
	if trackingKeepOpenRE.MatchString(body) {
		return "keep-open", true
	}
	if HasGHAWMarker(body) {
		return "gh-aw-marker", true
	}
	if ghawManagedRE.MatchString(body) {
		return "gh-aw-managed", true
	}
	if ghawLabel && hasGHAWTitlePrefix(title) {
		return "gh-aw-label", true
	}
	return "", false
}

func hasGHAWTitlePrefix(title string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(title)), GHAWTitlePrefix)
}

// IsTracking reports whether the issue is a tracking/archive issue.
func IsTracking(title string, labels []string, body string) bool {
	_, ok := TrackingReason(title, labels, body)
	return ok
}

// TrackingReasonText renders a TrackingReason for a human-facing note.
func TrackingReasonText(why string) string {
	switch why {
	case "label":
		return "`" + TrackingLabel + "` label"
	case "marker":
		return "`" + TrackingMarker + "` marker"
	case "keep-open":
		return "its body says \"" + TrackingKeepOpenSentence + "\""
	case "gh-aw-marker":
		return "a `<!-- gh-aw-… -->` marker: it is managed by GitHub Agentic Workflows"
	case "gh-aw-managed":
		return "its body says it is automatically managed by GitHub Agentic Workflows"
	case "gh-aw-label":
		return "`" + GHAWLabel + "` label on a `" + GHAWTitlePrefix + "` title: it is managed by GitHub Agentic Workflows"
	}
	return "tracking issue"
}

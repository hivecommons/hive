package logscrub

import (
	"regexp"
	"strings"
)

// Detecting text that has ALREADY been masked, as opposed to masking it.
//
// A reviewer agent that is handed scrubbed text cannot tell it from source:
// on hivecommons/hive#8067 a reviewer read `Authorization: ******` where the
// file said `Authorization: Bearer %s`, concluded the format string had no
// `%s`, and posted that as a defect. The masking did not come from this
// package — Go writes `[REDACTED]`, never a run of asterisks — but the harm
// lands the same way whichever layer did it, so the recognizer below covers
// foreign placeholders too. Anything that consumes agent-authored text about
// code can use it to refuse a claim that rests on a mask.
//
// This is deliberately a *recognizer*, not a scrubber: it never rewrites, and
// a false negative costs a wrong review while a false positive costs a
// legitimate one. Every alternative below therefore needs an explicit
// redaction word or a mask in unmistakable value position.

const (
	// bracketMarker covers `[REDACTED]` (pkg/logscrub, bin/contributor-relay.js)
	// and the suffixed forms `[REDACTED-JWT]` (bin/agent-launch.sh) and
	// `[REDACTED:bearer-token]`.
	bracketMarker = `\[redacted(?:[-:][a-z0-9_.-]+)?\]`
	// angleMarker covers `<redacted>` and `<redacted:bearer-token>`, the shape
	// most non-hive scrubbers emit.
	angleMarker = `<redacted(?:[-:][^>\n]*)?>`
	// starredMarker covers `***REDACTED***` and `_***REDACTED***`.
	starredMarker = `\*{2,}\s*redacted\s*\*{2,}`
	// opaqueMask covers a placeholder that names nothing — `******` — but only
	// where a *value* belongs: after `:` or `=`, or opening a quoted string.
	// The position requirement is what keeps a quoted C banner comment
	// (`/*********/`) and a shell glob from reading as a redaction.
	opaqueMask = `(?:[:=]\s*|["'])(?:\*{4,}|\x{2022}{4,})`
)

// redactionMarkerPattern recognizes text some scrubber has already masked.
// Case-insensitive: the word is written `REDACTED` by hive's own layers and
// `redacted` by several others.
var redactionMarkerPattern = regexp.MustCompile(`(?i)` + strings.Join([]string{
	bracketMarker,
	angleMarker,
	starredMarker,
	opaqueMask,
}, "|"))

// FindRedactionMarker returns the first redaction marker in s, or "" when s
// carries none. The returned text is the marker itself (with the leading
// delimiter, for an opaque mask), which makes it safe to quote back in an
// error message: a marker is by construction not secret material.
func FindRedactionMarker(s string) string {
	return redactionMarkerPattern.FindString(s)
}

// ContainsRedactionMarker reports whether s carries text that a secret
// scrubber has already masked.
func ContainsRedactionMarker(s string) bool {
	return redactionMarkerPattern.MatchString(s)
}

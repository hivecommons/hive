// Package prosefilter is the root-module entry point to hive's one outbound
// dash filter. The implementation lives in the crux module
// (github.com/hivecommons/hive/crux/prosefilter) so the GitHub egress proxy,
// which cannot import pkg/, applies the very same rule to agent writes
// (hivecommons/hive#19103). Every function here delegates; do not add a
// second implementation.
package prosefilter

import cruxprose "github.com/hivecommons/hive/crux/prosefilter"

const (
	emDash = cruxprose.EmDash
	enDash = cruxprose.EnDash
)

// HasDash reports whether s contains an em-dash or an en-dash.
func HasDash(s string) bool { return cruxprose.HasDash(s) }

// Sanitize replaces em-dashes, and en-dashes used as spaced dashes, in the
// prose of s with a plain hyphen. Code fences, inline code and blockquote
// lines are untouched.
func Sanitize(s string) string { return cruxprose.Sanitize(s) }

// SanitizeTitle is Sanitize for a single-line title.
func SanitizeTitle(s string) string { return cruxprose.SanitizeTitle(s) }

// Keys, Kind, Prose, Title and Stats are the shared filter's JSON field
// selectors and change counts.
type (
	Keys  = cruxprose.Keys
	Kind  = cruxprose.Kind
	Stats = cruxprose.Stats
)

const (
	Prose = cruxprose.Prose
	Title = cruxprose.Title
)

// SanitizeJSON rewrites the string values under keys, at any depth of a JSON
// document, with the shared rule. Anything that is not JSON, or has nothing
// to change, is returned as is.
func SanitizeJSON(body []byte, keys Keys) ([]byte, Stats) { return cruxprose.SanitizeJSON(body, keys) }

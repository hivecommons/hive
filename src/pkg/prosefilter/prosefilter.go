// Package prosefilter is the root-module entry point to the shared outbound
// dash filter. The implementation lives in crux/prosefilter so the GitHub
// egress proxy (nested crux module, which cannot import pkg/) and hive's own
// send paths use the same function. See crux/prosefilter for the rules.
package prosefilter

import cruxfilter "github.com/hivecommons/hive/crux/prosefilter"

// HasDash reports whether s contains an em-dash or an en-dash.
func HasDash(s string) bool { return cruxfilter.HasDash(s) }

// Sanitize rewrites dashes in the prose of s. See crux/prosefilter.Sanitize.
func Sanitize(s string) string { return cruxfilter.Sanitize(s) }

// SanitizeTitle is Sanitize for a single-line title.
func SanitizeTitle(s string) string { return cruxfilter.SanitizeTitle(s) }

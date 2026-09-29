package config

import (
	"regexp"
	"strings"
)

// WildcardMatch checks if text matches a pattern supporting:
// - * wildcards (match any substring)
// - /regex/ syntax for full regex
// - plain substring match (case-insensitive)
func WildcardMatch(text, pattern string) bool {
	text = strings.ToLower(text)
	pattern = strings.TrimSpace(pattern)

	if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		re, err := regexp.Compile("(?i)" + pattern[1:len(pattern)-1])
		if err != nil {
			return false
		}
		return re.MatchString(text)
	}

	pattern = strings.ToLower(pattern)
	if strings.Contains(pattern, "*") {
		parts := strings.Split(pattern, "*")
		idx := 0
		for _, part := range parts {
			if part == "" {
				continue
			}
			found := strings.Index(text[idx:], part)
			if found < 0 {
				return false
			}
			idx += found + len(part)
		}
		if !strings.HasPrefix(pattern, "*") && !strings.HasPrefix(text, parts[0]) {
			return false
		}
		if !strings.HasSuffix(pattern, "*") && !strings.HasSuffix(text, parts[len(parts)-1]) {
			return false
		}
		return true
	}

	return strings.Contains(text, pattern)
}

// MatchesAny returns true if text matches any pattern in the list.
func MatchesAny(text string, patterns []string) bool {
	for _, p := range patterns {
		if WildcardMatch(text, p) {
			return true
		}
	}
	return false
}

// Contribute filter modes for title/author/label gating.
const (
	// FilterModeDeny (the default) skips items that MATCH the list; everything
	// else passes.
	FilterModeDeny = "deny"
	// FilterModeAllow passes ONLY items that MATCH the list; everything else is
	// skipped. An EMPTY allow list is treated as "filter off" (see
	// FilterPasses) so a half-configured allow filter never silently blocks
	// every item.
	FilterModeAllow = "allow"
)

// NormalizeFilterMode returns a valid mode, defaulting to deny for empty/unknown
// values (backward compatible: existing config has no mode field, and its lists
// were always deny lists).
func NormalizeFilterMode(mode string) string {
	if mode == FilterModeAllow {
		return FilterModeAllow
	}
	return FilterModeDeny
}

// FilterPasses reports whether a single value (an issue/PR title, author, or one
// of its labels) passes a mode+list filter.
//
//   - deny  mode: pass unless value matches the list.
//   - allow mode: pass only if value matches the list; BUT an empty allow list
//     means "not configured" → pass (never block everything on an empty list).
//
// Patterns use the same wildcard/regex syntax as MatchesAny.
func FilterPasses(value string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		return MatchesAny(value, list)
	default: // deny
		return !MatchesAny(value, list)
	}
}

// LabelsFilterPasses applies a label filter across ALL of an item's labels.
//
//   - deny  mode: pass unless ANY label matches the deny list.
//   - allow mode: pass only if AT LEAST ONE label matches the allow list; an
//     empty allow list means "not configured" → pass.
//
// This is the label-specific counterpart to FilterPasses (labels are a set, not
// a single scalar, so the allow/deny quantifiers differ).
func LabelsFilterPasses(labels []string, list []string, mode string) bool {
	switch NormalizeFilterMode(mode) {
	case FilterModeAllow:
		if len(list) == 0 {
			return true // allow filter not configured → don't gate
		}
		for _, l := range labels {
			if MatchesAny(l, list) {
				return true
			}
		}
		return false
	default: // deny
		for _, l := range labels {
			if MatchesAny(l, list) {
				return false
			}
		}
		return true
	}
}

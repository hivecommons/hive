// Package jsonextract provides a small shared helper for pulling a JSON
// object out of surrounding free text, typically an LLM chat response that
// wraps the requested JSON in prose or code fences. It was previously
// hand-copied byte-identically into pkg/intent, pkg/ioscan, pkg/retro, and
// pkg/trajectory.
package jsonextract

import "strings"

// Object returns the substring of s spanning the first '{' through the last
// '}', or "" when no such span exists. It is a deliberate heuristic — it does
// not validate the JSON — matching the behavior callers rely on for parsing
// model output.
func Object(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

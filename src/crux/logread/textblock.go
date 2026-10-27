package logread

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// TextScrub is a block of plain text after ScrubText.
type TextScrub struct {
	Text string
	// Counts is what was replaced, by kind.
	Counts map[string]int
	// PreExistingMarkerLines counts lines that already carried text in the
	// shape of a redaction marker before this scrub.
	PreExistingMarkerLines int
}

// ScrubText scrubs a block of plain text (a terminal capture, a kick or stage
// log, a prompt) exactly as Read scrubs log lines: private-key spans across
// the whole block, then this hive's own secret values, then every
// pkg/logscrub credential shape. Its work is linear in len(text); callers that
// need a bounded amount of work cut the text first.
func ScrubText(text string, secrets *SecretScrubber) TextScrub {
	lines := strings.Split(text, "\n")
	spans := keySpans(lines)
	out := TextScrub{Counts: map[string]int{}}
	for i, line := range lines {
		scrubbed, counts, pre := scrubLine(line, spans[i], secrets)
		lines[i] = scrubbed
		for k, v := range counts {
			out.Counts[k] += v
		}
		if pre {
			out.PreExistingMarkerLines++
		}
	}
	out.Text = strings.Join(lines, "\n")
	return out
}

// RedactionList renders redaction counts as a sorted, never-nil list.
func RedactionList(counts map[string]int) []Redaction {
	kinds := make([]string, 0, len(counts))
	for k, v := range counts {
		if v > 0 {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	out := make([]Redaction, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, Redaction{Kind: k, Count: counts[k]})
	}
	return out
}

// HeadText returns at most n bytes from the start of text, cut on a rune
// boundary.
func HeadText(text string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(text) <= n {
		return text
	}
	return cutText(text, n)
}

// TailText returns the newest part of text: at most n bytes from its end,
// starting on a rune boundary and, when one lies in the first half of that
// part, just after a line break so the first line is whole. It reports
// whether anything was dropped.
func TailText(text string, n int) (string, bool) {
	if len(text) <= n {
		return text, false
	}
	if n <= 0 {
		return "", len(text) > 0
	}
	start := len(text) - n
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	tail := text[start:]
	if start > 0 && text[start-1] != '\n' {
		if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)/2 {
			tail = tail[i+1:]
		}
	}
	return tail, true
}

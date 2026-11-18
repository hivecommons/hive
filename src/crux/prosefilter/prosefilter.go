// Package prosefilter is the one place hive rewrites dashes in text that
// leaves the hive (PR and issue titles and bodies, comments, review bodies,
// commit messages, notifications, meeting chat and spoken text).
//
// The owner's rule is that em-dashes are never acceptable in anything hive or
// its agents write. Agents are told so in the shared base prompt, but a model
// can still emit one, so every outward write path also passes its prose
// through Sanitize before it is sent: hive's own sends in the root module
// (via src/pkg/prosefilter, which delegates here) and agent-originated GitHub
// writes in the egress proxy (src/crux/proxy/outbound_prose.go). It lives in
// the crux module so both sides call the same function (hivecommons/hive#19103).
//
// Sanitize rewrites only prose. Fenced code blocks, inline code spans and
// Markdown blockquote lines are left byte-for-byte alone, because they carry
// quoted or pasted content (a diff, a log line, a file excerpt) that must not
// be altered.
package prosefilter

import (
	"regexp"
	"strings"
)

const (
	// EmDash and EnDash are the two characters the filter rewrites.
	EmDash = "\u2014"
	EnDash = "\u2013"

	emDash = EmDash
	enDash = EnDash

	// plainDash is what a dash used as punctuation becomes.
	plainDash = "-"
	// spacedDash is the replacement between two words.
	spacedDash = " - "

	// maxFenceIndent is the Markdown limit on leading spaces for a code fence.
	maxFenceIndent = 3
	// minFenceRun is the shortest run of fence characters that opens a fence.
	minFenceRun = 3
)

var (
	// emDashRE matches an em-dash with the horizontal whitespace around it.
	emDashRE = regexp.MustCompile(`[ \t]*` + emDash + `[ \t]*`)
	// spacedEnDashRE matches an en-dash used as a dash (whitespace on both
	// sides). An en-dash in a range such as 3-5 has no spaces and is left alone
	// here; a hard-coded range is fixed at its source.
	spacedEnDashRE = regexp.MustCompile(`[ \t]+` + enDash + `[ \t]+`)
)

// HasDash reports whether s contains an em-dash or an en-dash.
func HasDash(s string) bool {
	return strings.Contains(s, emDash) || strings.Contains(s, enDash)
}

// Sanitize replaces every em-dash (U+2014), and every en-dash (U+2013) used
// as a spaced dash, in the prose of s with a plain hyphen. Text inside fenced
// code blocks, inline code spans and blockquote lines is untouched. A string
// with no such dash is returned unchanged.
func Sanitize(s string) string {
	if !HasDash(s) {
		return s
	}
	lines := strings.Split(s, "\n")
	var fenceChar byte
	var fenceLen int
	for i, line := range lines {
		if fenceChar != 0 {
			if closesFence(line, fenceChar, fenceLen) {
				fenceChar, fenceLen = 0, 0
			}
			continue
		}
		if c, n := opensFence(line); c != 0 {
			fenceChar, fenceLen = c, n
			continue
		}
		if isBlockquote(line) {
			continue
		}
		lines[i] = sanitizeLine(line)
	}
	return strings.Join(lines, "\n")
}

// SanitizeTitle is Sanitize for a single-line title. A title has no code
// fence or blockquote, so only inline code spans are protected.
func SanitizeTitle(s string) string {
	if !HasDash(s) {
		return s
	}
	return sanitizeLine(s)
}

func opensFence(line string) (byte, int) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > maxFenceIndent || trimmed == "" {
		return 0, 0
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return 0, 0
	}
	n := runLen(trimmed, 0, c)
	if n < minFenceRun {
		return 0, 0
	}
	// A backtick fence's info string may not contain a backtick; such a line
	// is an inline code span, not a fence.
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return 0, 0
	}
	return c, n
}

func closesFence(line string, c byte, openLen int) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > maxFenceIndent || trimmed == "" || trimmed[0] != c {
		return false
	}
	n := runLen(trimmed, 0, c)
	return n >= openLen && strings.TrimSpace(trimmed[n:]) == ""
}

func isBlockquote(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " "), ">")
}

func runLen(s string, from int, c byte) int {
	n := 0
	for from+n < len(s) && s[from+n] == c {
		n++
	}
	return n
}

// sanitizeLine rewrites one prose line, skipping inline code spans.
func sanitizeLine(line string) string {
	if !HasDash(line) {
		return line
	}
	indentLen := len(line) - len(strings.TrimLeft(line, " \t"))
	indent, rest := line[:indentLen], line[indentLen:]

	var b strings.Builder
	b.WriteString(indent)
	segStart := 0
	flush := func(end int) {
		if end > segStart {
			b.WriteString(rewriteSegment(rest[segStart:end], segStart == 0, end == len(rest)))
		}
	}
	for i := 0; i < len(rest); {
		if rest[i] != '`' {
			i++
			continue
		}
		n := runLen(rest, i, '`')
		closeAt := findClosingRun(rest, i+n, n)
		if closeAt < 0 {
			i += n
			continue
		}
		flush(i)
		spanEnd := closeAt + n
		b.WriteString(rest[i:spanEnd])
		segStart = spanEnd
		i = spanEnd
	}
	flush(len(rest))
	return b.String()
}

// findClosingRun returns the index of the next run of exactly n backticks at
// or after from, or -1.
func findClosingRun(s string, from, n int) int {
	for i := from; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		m := runLen(s, i, '`')
		if m == n {
			return i
		}
		i += m
	}
	return -1
}

// rewriteSegment replaces dashes in one non-code stretch of a line. atStart
// and atEnd say whether the segment touches the start or end of the line's
// prose, so a leading or trailing dash does not leave a stray space.
func rewriteSegment(seg string, atStart, atEnd bool) string {
	seg = spacedEnDashRE.ReplaceAllString(seg, spacedDash)
	locs := emDashRE.FindAllStringIndex(seg, -1)
	if len(locs) == 0 {
		return seg
	}
	var b strings.Builder
	prev := 0
	for _, loc := range locs {
		b.WriteString(seg[prev:loc[0]])
		switch {
		case loc[0] == 0 && atStart:
			b.WriteString(plainDash + " ")
		case loc[1] == len(seg) && atEnd:
			b.WriteString(" " + plainDash)
		default:
			b.WriteString(spacedDash)
		}
		prev = loc[1]
	}
	b.WriteString(seg[prev:])
	return b.String()
}

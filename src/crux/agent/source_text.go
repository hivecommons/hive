package agent

import (
	"regexp"
	"strings"
)

// grepPrefixRe matches the "path.go:NN:" prefix grep/rg put on a hit.
var grepPrefixRe = regexp.MustCompile("^\\s*[\\w./-]+\\.\\w+:\\d+[:-]")

// patternTableNames are identifiers whose presence on a line means the pane is
// showing hive's own login-pattern source, not a login screen (#17006).
var patternTableNames = []string{"authScreenPatterns", "loginPromptPatterns", "classify.go"}

// authHitIsSourceText reports whether every match of pat on line is source or
// grep output quoting the pattern (a path.go:NN: prefix, a pattern-table name,
// a string literal, or a |-joined alternation) rather than a CLI login screen.
func authHitIsSourceText(line, pat string) bool {
	if grepPrefixRe.MatchString(line) {
		return true
	}
	for _, name := range patternTableNames {
		if strings.Contains(line, name) {
			return true
		}
	}
	for off := 0; ; {
		i := strings.Index(line[off:], pat)
		if i < 0 {
			return true
		}
		start := off + i
		end := start + len(pat)
		if !hitIsQuoted(line, start, end) {
			return false
		}
		off = end
	}
}

// hitIsQuoted reports whether the match at [start,end) sits inside a quoted
// literal or beside a regex alternation bar.
func hitIsQuoted(line string, start, end int) bool {
	before, after := line[:start], line[end:]
	if strings.Count(before, "\"")%2 == 1 || strings.Count(before, "`")%2 == 1 {
		return true
	}
	b := strings.TrimRight(before, " ")
	a := strings.TrimLeft(after, " ")
	for _, q := range []string{"'", "\"", "`", "|"} {
		if strings.HasSuffix(b, q) || strings.HasPrefix(a, q) {
			return true
		}
	}
	return false
}

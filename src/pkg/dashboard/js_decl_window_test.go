package dashboard

import (
	"regexp"
	"strings"
)

// jsDeclWindow is how far around each literal occurrence of a name the
// declaration regexps are allowed to look. The widest thing a match can carry
// besides the name is `(?m)^\s*(async\s+)?function\s+` on one side or
// `\s*:\s*function\s*\(` on the other; no line in index.html indents or pads
// anywhere near this much.
const jsDeclWindow = 512

// jsDeclaredNear reports whether decl matches index.html at some occurrence of
// name. It is the same answer as decl.MatchString(html) for the declaration
// regexps the *_ui_test.go files build — every one of their alternatives
// contains the literal name — but runs in microseconds instead of seconds.
//
// Those regexps start with an alternation or a `(?m)^\s*` anchor, so Go's
// matcher has no literal prefix to jump to and walks the whole page once
// per name. The full-page regexp scan per name was slow enough that the
// shard approached its CI budget. Limiting the match to a window around each
// strings.Index hit keeps the regexp and the assertion unchanged while
// skipping the pages of text that cannot match.
func jsDeclaredNear(html, name string, decl *regexp.Regexp) bool {
	for from := 0; from < len(html); {
		i := strings.Index(html[from:], name)
		if i < 0 {
			return false
		}
		at := from + i
		lo := at - jsDeclWindow
		if lo < 0 {
			lo = 0
		}
		// Start the window at a real line start so a `(?m)^` anchor cannot
		// match at the cut.
		lo = strings.LastIndexByte(html[:lo], '\n') + 1
		hi := at + len(name) + jsDeclWindow
		if hi > len(html) {
			hi = len(html)
		}
		if decl.MatchString(html[lo:hi]) {
			return true
		}
		from = at + 1
	}
	return false
}

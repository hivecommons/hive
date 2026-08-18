package hub

import (
	"regexp"
	"strings"
	"testing"
)

// The "Loading your hives..." outage was NOT a syntax error — every <script>
// block in dashboardHTML parsed cleanly with `node --check`. A dropped closing
// brace on renderAlertRows silently swallowed ~13 KB of code (including
// _dashSearchQuery, _dashFacets and applyDashFilters) into that function's
// body. The file still parsed; those declarations simply became function-local,
// so renderHives() threw ReferenceError at runtime and the spinner never left.
//
// A brace-balance check over each script block catches that whole class of
// defect, which node --check cannot: nesting is legal JavaScript.
func TestDashboardScriptBracesAreBalanced(t *testing.T) {
	for i, block := range dashboardScriptBlocks(t) {
		if depth := braceDepth(block); depth != 0 {
			t.Errorf("script block %d ends at brace depth %d, want 0 — "+
				"an unbalanced brace nests later top-level declarations inside "+
				"an earlier function and breaks the dashboard at runtime", i, depth)
		}
	}
}

// Declarations the render path depends on must be reachable as top-level
// statements. Anchoring on column 4 ("    var x") is how the file is
// formatted for genuinely top-level inline-script declarations; anything
// swallowed into a function body is indented further.
func TestDashboardRenderStateIsTopLevel(t *testing.T) {
	names := []string{
		"_dashSearchQuery",
		"_dashFacets",
		"_dashFacetCollapsed",
		"_dashSectionCollapsed",
		"_allDashHives",
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			re := regexp.MustCompile(`(?m)^ {4}var ` + regexp.QuoteMeta(n) + `\b`)
			if !re.MatchString(dashboardHTML) {
				t.Errorf("var %s is not declared at top level of the inline script; "+
					"renderHives() will throw ReferenceError at runtime", n)
			}
		})
	}
}

func TestDashboardRenderFunctionsAreTopLevel(t *testing.T) {
	names := []string{
		"renderAlertRows",
		"applyDashFilters",
		"renderFacetRail",
		"renderHives",
		"renderHivesError",
		"paintCachedHives",
	}
	for _, n := range names {
		t.Run(n, func(t *testing.T) {
			re := regexp.MustCompile(`(?m)^ {4}function ` + regexp.QuoteMeta(n) + `\(`)
			if !re.MatchString(dashboardHTML) {
				t.Errorf("function %s is not declared at top level of the inline script", n)
			}
		})
	}
}

// dashboardScriptBlocks returns the body of every <script> element in
// dashboardHTML. Built without a literal closing-script tag in source.
func dashboardScriptBlocks(t *testing.T) []string {
	t.Helper()
	open := regexp.MustCompile(`(?s)<script[^>]*>`)
	closeTag := "</" + "script>"
	var out []string
	rest := dashboardHTML
	for {
		loc := open.FindStringIndex(rest)
		if loc == nil {
			break
		}
		rest = rest[loc[1]:]
		end := strings.Index(rest, closeTag)
		if end < 0 {
			t.Fatalf("unterminated script element in dashboardHTML")
		}
		out = append(out, rest[:end])
		rest = rest[end+len(closeTag):]
	}
	if len(out) == 0 {
		t.Fatal("no script blocks found in dashboardHTML")
	}
	return out
}

// braceDepth counts { and } outside string literals, comments and regex
// literals. It is deliberately conservative: it only needs to be exact enough
// that a genuinely balanced block returns 0.
func braceDepth(src string) int {
	depth := 0
	var inStr byte    // quote char when inside a string
	inLineComment := false
	inBlockComment := false
	inRegex := false
	// prev is the last significant character, used to tell a regex literal
	// from a division operator.
	var prev byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
			}
			continue
		case inBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		case inStr != 0:
			if c == '\\' {
				i++
				continue
			}
			if c == inStr {
				inStr = 0
			}
			continue
		case inRegex:
			if c == '\\' {
				i++
				continue
			}
			if c == '[' {
				// Character class: a '/' inside it does not close the regex.
				for i++; i < len(src); i++ {
					if src[i] == '\\' {
						i++
						continue
					}
					if src[i] == ']' {
						break
					}
				}
				continue
			}
			if c == '/' {
				inRegex = false
			}
			continue
		}
		switch c {
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				inLineComment = true
				i++
				continue
			}
			if i+1 < len(src) && src[i+1] == '*' {
				inBlockComment = true
				i++
				continue
			}
			// A '/' starting a regex can only follow an operator or opening
			// bracket — never a value-producing token.
			if prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*~^%<>", prev) >= 0 {
				inRegex = true
				continue
			}
		case '\'', '"', '`':
			inStr = c
			continue
		case '{':
			depth++
		case '}':
			depth--
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
	}
	return depth
}

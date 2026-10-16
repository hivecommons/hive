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

// An apostrophe typed into a single-quoted JS string literal ('... the
// hive's own evidence ...') ends the string early; the parser then meets the
// leftover prose and fails the whole <script> block with
// "SyntaxError: Unexpected identifier 's'". Nothing in the block runs, so the
// page sits at "Loading your hives..." forever. node --check would catch it,
// but the hub's test suite is Go, and braceDepth only reports brace balance.
//
// A '...' or "..." literal may not contain a raw newline, so a string still
// open when the lexer reaches end-of-line is unterminated — exactly what a
// stray quote produces. Template literals are exempt because they legally
// span lines.
func TestDashboardScriptStringsTerminate(t *testing.T) {
	for i, block := range dashboardScriptBlocks(t) {
		for _, ln := range unterminatedStringLines(block) {
			line := strings.TrimSpace(strings.Split(block, "\n")[ln-1])
			if len(line) > 120 {
				line = line[:120] + "…"
			}
			t.Errorf("script block %d line %d: string literal is still open at end of line "+
				"(an unescaped quote inside the string?) — the whole block fails to parse and "+
				"the dashboard never leaves its loading spinner:\n  %s", i, ln, line)
		}
	}
}

// A literal built the way the quadrant header was (prose with an apostrophe
// inside a single-quoted string) must trip the check; the escaped forms the
// file actually uses (\' and &#39;) must not.
func TestUnterminatedStringLinesDetectsStrayQuote(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []int
	}{
		{"clean", "var a = 'x';\nvar b = \"y\";\n", nil},
		{"escaped quote", "var a = 'the hive\\'s own';\n", nil},
		{"html entity", "var a = 'the hive&#39;s own';\n", nil},
		{"template literal spans lines", "var a = `one\ntwo`;\n", nil},
		{"regex with quote", "var r = /it's/; var b = 'ok';\n", nil},
		{"comment with quote", "// don't\nvar a = 'ok'; /* it's */\n", nil},
		{"stray apostrophe", "var ok = 1;\nvar a = '<th title=\"from the hive's own evidence\">' + x;\nvar b = 2;\n", []int{2}},
		{"stray double quote", "var a = \"say \"hi\" to the fleet;\nvar b = 1;\n", []int{1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unterminatedStringLines(tc.src)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
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
	depth, _ := scanScript(src)
	return depth
}

// unterminatedStringLines returns the 1-based line numbers at which a '...'
// or "..." literal was still open when the line ended.
func unterminatedStringLines(src string) []int {
	_, lines := scanScript(src)
	return lines
}

// scanScript is the shared lexer behind braceDepth and
// unterminatedStringLines: a single pass that tracks string, comment and
// regex state so that braces and quotes are only counted where the JavaScript
// parser would see them.
func scanScript(src string) (depth int, unterminated []int) {
	var inStr byte // quote char when inside a string
	line := 1
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
				line++
				inLineComment = false
			}
			continue
		case inBlockComment:
			if c == '\n' {
				line++
			}
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlockComment = false
				i++
			}
			continue
		case inStr != 0:
			if c == '\\' {
				if i+1 < len(src) && src[i+1] == '\n' {
					line++
				}
				i++
				continue
			}
			if c == '\n' {
				line++
				if inStr != '`' {
					// A quoted string cannot cross a line: a quote earlier on
					// this line closed the literal the author meant to write.
					unterminated = append(unterminated, line-1)
					inStr = 0
				}
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
			if c == '\n' {
				// Regex literals cannot span lines either; if we are still
				// "inside" one here the '/' was really a division operator.
				line++
				inRegex = false
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
		case '\n':
			line++
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			prev = c
		}
	}
	return depth, unterminated
}

package hub

import (
	"os"
	"os/exec"
	"path/filepath"
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
	var inStr byte // quote char when inside a string
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

// TestDashboardScriptStringsTerminateOnTheirLine guards the 2026-10-16 outage:
// a <th title="... the hive's own evidence ..."> fragment was pasted into a
// single-quoted JS string, so the apostrophe closed the string early and the
// whole dashboard block failed to parse ("SyntaxError: Unexpected identifier
// 's'"). My Hives sat on "Loading your hives..." for every user. Quote and
// double-quote literals cannot span lines in JavaScript, so any line that ends
// inside one is a hard syntax error — flag it with its line number so the
// author sees exactly which prose needs \' or a template literal.
func TestDashboardScriptStringsTerminateOnTheirLine(t *testing.T) {
	for i, block := range dashboardScriptBlocks(t) {
		for _, bad := range unterminatedStringLines(block) {
			t.Errorf("script block %d line %d ends inside a %q string literal: %s",
				i, bad.line, bad.quote, strings.TrimSpace(bad.text))
		}
	}
}

// TestDashboardScriptParsesWithNode is the authoritative check when node is
// installed (developer machines, most CI images): every inline block must pass
// `node --check`. Skips cleanly where node is absent so the Go-only guards
// above remain the floor.
func TestDashboardScriptParsesWithNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node not on PATH in CI; the hub test lane installs it via setup-node — this check must not silently skip there")
		}
		t.Skip("node not on PATH; relying on Go-only structure checks")
	}
	for i, block := range dashboardScriptBlocks(t) {
		f := filepath.Join(t.TempDir(), "block.js")
		if err := os.WriteFile(f, []byte(block), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, "--check", f).CombinedOutput()
		if err != nil {
			t.Errorf("script block %d fails node --check:\n%s", i, out)
		}
	}
}

type unterminatedString struct {
	line  int
	quote byte
	text  string
}

// unterminatedStringLines walks src tracking '...', "..." and `...` literals,
// // and /* */ comments, and /regex/ literals (which legally contain quotes), reporting every line that
// ends while a quote or double-quote literal is still open. Template literals
// legitimately span lines and are ignored; block comments are skipped so
// apostrophes in prose comments never trip it.
func unterminatedStringLines(src string) []unterminatedString {
	var out []unterminatedString
	var quote byte
	inLineComment, inBlockComment := false, false
	line := 1
	lineStart := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			if quote == '\'' || quote == '"' {
				out = append(out, unterminatedString{line: line, quote: quote, text: src[lineStart:i]})
				quote = 0
			}
			inLineComment = false
			line++
			lineStart = i + 1
			continue
		}
		switch {
		case inLineComment:
		case inBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlockComment = false
				i++
			}
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			inLineComment = true
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			inBlockComment = true
			i++
		case c == '/' && regexCanStart(src[lineStart:i]):
			i = skipRegexLiteral(src, i)
		case c == '\'' || c == '"' || c == '`':
			quote = c
		}
	}
	return out
}

func TestUnterminatedStringLinesDetectsStrayApostrophe(t *testing.T) {
	bad := "var a = 1;\nvar t = '<th title=\"the hive's own\">';\nvar ok = \"it's fine\";\n"
	got := unterminatedStringLines(bad)
	if len(got) != 1 || got[0].line != 2 {
		t.Fatalf("want exactly line 2 flagged, got %+v", got)
	}
	clean := "var t = `multi\nline`; // it's a comment\n/* don't */ var s = 'it\\'s';\nvar r = a / b / c;\n" +
		"x = s.replace(/'/g, \"\\\\'\");\nif (m = t.match(/^[\"']|[\"']$/g)) y = (on ? \"''\" : \"'\");\nreturn /it's/.test(z);\n"
	if got := unterminatedStringLines(clean); len(got) != 0 {
		t.Fatalf("false positives: %+v", got)
	}
}

// regexCanStart reports whether a `/` at the end of prefix begins a regex
// literal rather than a division: it does when the previous significant token
// is an operator, opener, separator or `return`/`typeof`, or the line is empty
// so far.
func regexCanStart(prefix string) bool {
	p := strings.TrimRight(prefix, " \t")
	if p == "" {
		return true
	}
	switch p[len(p)-1] {
	case '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	for _, kw := range []string{"return", "typeof", "case", "in", "of"} {
		if strings.HasSuffix(p, kw) && (len(p) == len(kw) || !isIdentByte(p[len(p)-len(kw)-1])) {
			return true
		}
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// skipRegexLiteral returns the index of the closing `/` of the regex literal
// opening at src[i], honouring escapes and [...] classes (where `/` is literal).
func skipRegexLiteral(src string, i int) int {
	inClass := false
	for j := i + 1; j < len(src) && src[j] != '\n'; j++ {
		switch src[j] {
		case '\\':
			j++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				return j
			}
		}
	}
	return i
}

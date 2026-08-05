package hub

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// hiveAsyncCallRE finds every `await hiveConfirm(...)` / `await hiveNotify(...)`
// / `await hivePrompt(...)` call site in the rendered dashboard JS. `\b` on
// both ends keeps `awaithiveConfirm` (no word boundary) and things like
// `hiveConfirmed` from matching.
var hiveAsyncCallRE = regexp.MustCompile(`\bawait\s+(hiveConfirm|hiveNotify|hivePrompt)\b`)

// functionBoundaryRE matches the opening line of a function whose body could
// be the innermost enclosing function of a scan position: a traditional
// declaration/expression (`async function foo(...) {`, `function foo(...) {`),
// an arrow function (`const x = async (...) => {`, `x = (...) => {`,
// `(...) => {`), or a method/object-literal shorthand (`foo(...) {`,
// `async foo(...) {`). The optional leading `async` is captured so its
// presence can be checked directly instead of re-deriving it from context.
//
// This is intentionally permissive on the "is this really a function header"
// question — the `\w+\s*(...)  {` branch exists for method/object-literal
// shorthand (`foo(...) {`) but syntactically looks identical to JS control
// keywords followed by a parenthesized expression (`if (...) {`,
// `catch (e) {`, `while (...) {`, `switch (...) {`, `for (...) {`). RE2 has
// no lookahead to exclude them inline, so controlFlowKeywordRE filters them
// out of a match after the fact; the walk-back loop then keeps climbing past
// the block to the real enclosing function. `else {` and bare `try {` don't
// match at all (no parameter list), so they never need filtering.
var functionBoundaryRE = regexp.MustCompile(
	`(?:(async)\s+)?(?:function\b\s*\w*\s*\([^()]*\)|(\w+)\s*\([^()]*\)|\([^()]*\)\s*=>)\s*\{\s*$`,
)

// controlFlowKeywordRE matches the JS control-flow keywords that take a
// parenthesized expression and so are syntactically indistinguishable from
// method-shorthand function headers under functionBoundaryRE's permissive
// `\w+(...)  {` branch.
var controlFlowKeywordRE = regexp.MustCompile(`^(if|for|while|switch|catch)$`)

// asyncKeywordRE matches a function header that carries the `async` keyword
// (in any of the forms functionBoundaryRE recognizes: `async function foo(`,
// `= async (...) =>`, `async foo(...) {`).
var asyncKeywordRE = regexp.MustCompile(`^\s*async\b`)

// maxFunctionSearchBack bounds how far back (in bytes) the walk-back scan
// looks for an enclosing function header before giving up. dashboardHTML's
// biggest functions are well under this; it exists so a stray unmatched
// brace earlier in the file can't turn a single bad match into an
// unbounded/quadratic scan.
const maxFunctionSearchBack = 20000

// TestNoAwaitOutsideAsync guards the `await hiveConfirm/hiveNotify/hivePrompt`
// call sites the overlay-modal helpers rely on (see TestNoNativeBrowserModals).
//
// hiveConfirm/hiveNotify/hivePrompt are promise-based replacements for the
// native confirm/prompt/alert dialogs, meant to be awaited. `await` is only
// legal inside a function declared `async`; using it inside a non-async
// function is a SyntaxError under strict-mode JS ("await is only valid in
// async functions") and in sloppy mode silently reinterprets the token,
// discarding the dialog result — a cancelled hiveConfirm then behaves as if
// the user clicked OK. Either way the surrounding function is broken. This
// scan finds every such call site and asserts its nearest enclosing function
// is declared async, so a future call site added to a plain `function` can't
// silently ship.
//
// This is a text/regex heuristic over embedded JS, not a real parser: it
// walks backwards from each match tracking brace depth to find the innermost
// unmatched `{`, checks whether the line that opens it is a function header,
// and if not keeps climbing past it (an `if`/`for`/`try`/object-literal
// block) to the next one out, until it finds a function header or gives up.
func TestNoAwaitOutsideAsync(t *testing.T) {
	src := dashboardHTML

	// Same sanity floor as TestNoNativeBrowserModals: a scan over an empty or
	// truncated string would pass for the wrong reason.
	if len(src) < 10000 {
		t.Fatalf("dashboardHTML is only %d bytes — not being read, so a pass proves nothing", len(src))
	}

	matches := hiveAsyncCallRE.FindAllStringIndex(src, -1)
	if len(matches) == 0 {
		t.Fatalf("found zero `await hiveConfirm/hiveNotify/hivePrompt` call sites — the scan is not matching the real content, so a pass proves nothing")
	}

	var offenders []string
	checked := 0
	for _, m := range matches {
		pos := m[0]

		if inCommentOrString(src, pos) {
			continue
		}
		checked++

		header, headerPos, ok := enclosingFunctionHeader(src, pos)
		if !ok {
			offenders = append(offenders, describeOffender(src, pos,
				"no enclosing function found (await at top level or search bound exceeded)"))
			continue
		}

		if !asyncKeywordRE.MatchString(header) {
			offenders = append(offenders, describeOffender(src, pos,
				"enclosing function is not declared async: line "+strconv.Itoa(lineOf(src, headerPos))+": "+truncate(strings.TrimSpace(header), 100)))
		}
	}

	if checked < len(matches)/2 {
		t.Fatalf("only %d/%d await-hive* call sites were scanned (rest looked like comments/strings) — the comment/string filter is probably too aggressive", checked, len(matches))
	}

	if len(offenders) > 0 {
		t.Errorf("await hiveConfirm/hiveNotify/hivePrompt used outside an async function. "+
			"await is only legal in a function declared async — in strict mode this is a SyntaxError, "+
			"in sloppy mode it silently discards the dialog result (a cancelled hiveConfirm then acts as if OK was clicked). "+
			"Declare the enclosing function `async`:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// inCommentOrString reports whether pos falls inside a `//` line comment, a
// `/* */` block comment, or a single/double-quoted string literal, scanning
// from the start of src. dashboardHTML is a single big Go raw string of
// mostly-JS, so this is a best-effort lexer: it does not need to be exact,
// only good enough that a literal `await hiveConfirm(` inside documentation
// prose or an example string doesn't get flagged as a real call site.
func inCommentOrString(src string, pos int) bool {
	inLineComment := false
	inBlockComment := false
	var stringQuote byte
	for i := 0; i < pos; i++ {
		c := src[i]
		switch {
		case inLineComment:
			if c == '\n' {
				inLineComment = false
			}
		case inBlockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				inBlockComment = false
				i++
			}
		case stringQuote != 0:
			if c == '\\' {
				i++ // skip escaped char
				continue
			}
			if c == stringQuote {
				stringQuote = 0
			}
		default:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				inLineComment = true
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				inBlockComment = true
			case c == '\'' || c == '"' || c == '`':
				stringQuote = c
			}
		}
	}
	return inLineComment || inBlockComment || stringQuote != 0
}

// enclosingFunctionHeader walks backwards from pos tracking brace depth to
// find the innermost function whose body contains pos, returning the header
// line (the line containing the function's opening `{`) and its byte offset.
//
// Depth starts at 0 counting from pos; each `}` seen while walking backwards
// increases depth (it's a nested block that already closed before pos), each
// `{` decreases it. When depth goes negative we've found an unmatched `{`
// that encloses pos. If the line it sits on looks like a function header,
// that's the answer. Otherwise it's a non-function block (if/for/try/object
// literal/etc.) — reset depth to 0 and keep walking past it to find the next
// enclosing `{`.
func enclosingFunctionHeader(src string, pos int) (header string, headerPos int, ok bool) {
	depth := 0
	searchFloor := pos - maxFunctionSearchBack
	if searchFloor < 0 {
		searchFloor = 0
	}
	for i := pos - 1; i >= searchFloor; i-- {
		switch src[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				lineStart := strings.LastIndexByte(src[:i+1], '\n') + 1
				lineEnd := strings.IndexByte(src[i:], '\n')
				if lineEnd == -1 {
					lineEnd = len(src)
				} else {
					lineEnd += i
				}
				line := src[lineStart:lineEnd]
				if isFunctionHeader(line) {
					return line, lineStart, true
				}
				// Not a function header (if/for/try/while/catch/object
				// literal/plain block) — keep climbing past it.
				depth = 0
				continue
			}
			depth--
		}
	}
	return "", 0, false
}

// isFunctionHeader reports whether line is a genuine function header rather
// than a control-flow statement that happens to match the same permissive
// shape (see controlFlowKeywordRE's doc comment).
func isFunctionHeader(line string) bool {
	m := functionBoundaryRE.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	// m[2] is the identifier captured by the method-shorthand branch; it's
	// empty for the function-keyword and arrow-function branches, which are
	// unambiguous and never need filtering.
	if m[2] != "" && controlFlowKeywordRE.MatchString(m[2]) {
		return false
	}
	return true
}

// lineOf returns the 1-based line number of pos within src.
func lineOf(src string, pos int) int {
	return strings.Count(src[:pos], "\n") + 1
}

// describeOffender renders one failing call site as `line N: snippet` plus
// the reason, matching the style of TestNoNativeBrowserModals's offender
// list so failures from either test read the same way.
func describeOffender(src string, pos int, reason string) string {
	lineStart := strings.LastIndexByte(src[:pos], '\n') + 1
	lineEnd := strings.IndexByte(src[pos:], '\n')
	if lineEnd == -1 {
		lineEnd = len(src)
	} else {
		lineEnd += pos
	}
	snippet := truncate(strings.TrimSpace(src[lineStart:lineEnd]), 100)
	return "line " + strconv.Itoa(lineOf(src, pos)) + ": " + snippet + " -- " + reason
}

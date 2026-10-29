package dashboard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// jsBodyOpenRe matches the `) {` (or minified `){`) that opens a function body.
var jsBodyOpenRe = regexp.MustCompile(`\)\s*\{`)

// jsFunc brace-matched from the first `{` after the signature. A default
// parameter such as `opts = {}` closes that brace immediately, so the slice
// was the truncated `function costSortTh(table, key, label, opts = {}` and the
// node harness that pulled it in (your-turn → render → renderStageTracker →
// … → connect → renderCost → costSortTh) died with "SyntaxError: Unexpected
// token 'function'" pointing at the NEXT function in the closure, not this
// one. These tests pin the extractor on the shapes the page actually uses.
func TestJsFuncSkipsDefaultParameterBraces(t *testing.T) {
	cases := []struct {
		name string
		html string
		want string
	}{
		{
			name: "object default",
			html: "    function f(a, opts = {}) {\n      return a;\n    }\n    function g() { return 1; }\n",
			want: "function f(a, opts = {}) {\n      return a;\n    }",
		},
		{
			name: "async with object default",
			html: "    async function f(opts = { x: 1 }) {\n      return opts.x;\n    }\n",
			want: "async function f(opts = { x: 1 }) {\n      return opts.x;\n    }",
		},
		{
			name: "string default containing braces and parens",
			html: "    function f(sep = '})(') {\n      return sep;\n    }\n",
			want: "function f(sep = '})(') {\n      return sep;\n    }",
		},
		{
			name: "array default and arrow default",
			html: "    function f(xs = [], cb = (v) => ({ v })) {\n      return cb(xs);\n    }\n",
			want: "function f(xs = [], cb = (v) => ({ v })) {\n      return cb(xs);\n    }",
		},
		{
			name: "plain parameters unchanged",
			html: "    function f(a, b) {\n      if (a) { return b; }\n      return a;\n    }\n",
			want: "function f(a, b) {\n      if (a) { return b; }\n      return a;\n    }",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsFunc(t, tc.html, "f"); got != tc.want {
				t.Fatalf("jsFunc sliced\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// Every top-level function in the shipped page must slice to a complete
// declaration. The check is structural — the slice ends on the body's closing
// brace, so it must contain the `) {` that opens it — and runs on the real
// page so a new signature shape the extractor cannot read fails here, with
// the function named, instead of as an opaque SyntaxError in whichever node
// harness happens to reach it first.
func TestJsFuncSlicesEveryPageFunctionToItsBody(t *testing.T) {
	html := indexHTML(t)
	var bad []string
	for _, m := range jsDeclRe.FindAllStringSubmatch(html, -1) {
		src := jsFunc(t, html, m[1])
		if !jsBodyOpenRe.MatchString(src) || !strings.HasSuffix(src, "}") {
			bad = append(bad, m[1])
		}
	}
	if len(bad) > 0 {
		t.Fatalf("%d page functions slice without reaching their body (default-parameter braces?): %s", len(bad), strings.Join(bad, ", "))
	}
}

// costSortTh is the shipped function with the `opts = {}` signature; its
// slice must be a program node accepts, which is what the your-turn harness
// needs when the cost panel is in its closure.
func TestJsFuncCostSortThSliceIsValidJavaScript(t *testing.T) {
	src := jsFunc(t, indexHTML(t), "costSortTh")
	if !strings.Contains(src, "return `<th") {
		t.Fatalf("costSortTh slice stopped before its body:\n%s", src)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: the costSortTh slice was NOT parsed by this run")
	}
	path := filepath.Join(t.TempDir(), "cost-sort-th.js")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("node rejected the costSortTh slice: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}

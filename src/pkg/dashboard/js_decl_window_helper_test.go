package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// TestJSDeclaredNearMatchesFullPage pins the contract jsDeclaredNear promises
// the *_ui_test.go callers: for a declaration regexp whose every alternative
// contains the literal name, it returns exactly decl.MatchString(html), even
// when the name sits further from the declaration keyword than the window
// allows, occurs only as a call, or only inside a longer identifier.
func TestJSDeclaredNearMatchesFullPage(t *testing.T) {
	filler := strings.Repeat("// padding so the declaration is far from both page edges\n", 40)
	longLine := strings.Repeat("x", 2*jsDeclWindow)

	declShapes := map[string]func(name string) *regexp.Regexp{
		"anchored function": func(name string) *regexp.Regexp {
			return regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + regexp.QuoteMeta(name) + `\s*\(`)
		},
		"alternation": func(name string) *regexp.Regexp {
			q := regexp.QuoteMeta(name)
			return regexp.MustCompile(`(?:function\s+` + q + `\s*\(|(?:const|let|var)\s+` + q + `\s*=|\b` + q + `\s*:\s*function\s*\()`)
		},
	}

	// want is keyed by declShapes entry: the anchored form only accepts a
	// line-leading `function name(`, the alternation also takes const/let/var
	// bindings and dispatch-table entries.
	cases := []struct {
		name string
		html string
		fn   string
		want map[string]bool
	}{
		{
			name: "declared far from the window edges",
			html: filler + "function renderThing(a, b) {\n  return a + b;\n}\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": true, "alternation": true},
		},
		{
			name: "declared at the very start of the page",
			html: "function renderThing() {}\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": true, "alternation": true},
		},
		{
			name: "declared only after earlier call sites",
			html: filler + "renderThing();\nrenderThing(1);\n" + filler + "const renderThing = (x) => x;\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": true},
		},
		{
			name: "present only as a call",
			html: filler + "  renderThing(1, 2);\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": false},
		},
		{
			name: "present only inside a longer identifier",
			html: filler + "function renderThingLater() {}\nconst prerenderThing = 1;\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": false},
		},
		{
			name: "absent",
			html: filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": false},
		},
		{
			// The declaration keyword sits on a line that begins more than
			// jsDeclWindow bytes before the name. A window cut mid-line would
			// let `(?m)^` match at the cut and turn the anchored form into a
			// false positive; the helper re-aligns the window to the line
			// start. The unanchored alternation correctly matches either way.
			name: "long line before the name defeats the ^ anchor",
			html: filler + longLine + " function renderThing() {}\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": true},
		},
		{
			name: "dispatch table entry",
			html: filler + "const actions = {\n  renderThing: function (event, A) {},\n};\n" + filler,
			fn:   "renderThing",
			want: map[string]bool{"anchored function": false, "alternation": true},
		},
	}

	for shape, build := range declShapes {
		for _, tc := range cases {
			decl := build(tc.fn)
			full := decl.MatchString(tc.html)
			got := jsDeclaredNear(tc.html, tc.fn, decl)
			if got != full {
				t.Errorf("%s / %s: jsDeclaredNear=%v but MatchString=%v", shape, tc.name, got, full)
			}
			if got != tc.want[shape] {
				t.Errorf("%s / %s: got %v, want %v", shape, tc.name, got, tc.want[shape])
			}
		}
	}
}

// TestJSDeclaredNearRealPage checks the helper against the shipped page with
// the exact regexp shape watchdog_ui_test.go uses, on a name that is declared
// and on one that is not.
func TestJSDeclaredNearRealPage(t *testing.T) {
	html := watchdogUIHTML(t)
	for _, tc := range []struct {
		fn   string
		want bool
	}{
		{"renderWatchdogSection", true},
		{"thisFunctionIsNeverDeclaredAnywhere", false},
	} {
		decl := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+` + regexp.QuoteMeta(tc.fn) + `\s*\(`)
		full := decl.MatchString(html)
		got := jsDeclaredNear(html, tc.fn, decl)
		if got != full || got != tc.want {
			t.Errorf("%s: jsDeclaredNear=%v MatchString=%v want=%v", tc.fn, got, full, tc.want)
		}
	}
}

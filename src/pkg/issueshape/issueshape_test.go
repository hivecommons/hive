package issueshape

import "testing"

func TestUnsubstitutedTemplatePlaceholder(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"issue #7014 title placeholder", "[guide] <specific description of the documentation gap>", true},
		{"what is missing placeholder", "<what is missing or incorrect>", true},
		{"what should be added placeholder", "<what should be added>", true},
		{"single-word analysis", "<analysis>", true},
		{"single-word fix", "<fix>", true},
		{"generic type parameter", "the API documents Foo<T> but not Foo<U>", false},
		{"autolinked URL", "see <https://example.com/path?q=1> for details", false},
		{"html tag with attribute", "<details open>", false},
		{"plain html tag", "<summary>", false},
		{"no placeholder", "a perfectly ordinary sentence", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := UnsubstitutedTemplatePlaceholder(c.text)
			if got != c.want {
				t.Fatalf("UnsubstitutedTemplatePlaceholder(%q)=%v want %v", c.text, got, c.want)
			}
		})
	}
}

func TestBodyHasMisEscapedNewlines(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"mis-escaped markdown", `## Finding\n\nDetails.\n\n## Recommendation\n\nFix it.`, true},
		{"real newlines", "## Finding\n\nDetails.\n\n## Recommendation\n\nFix it.", false},
		{"one-line prose mentioning escape", `use \n for a newline`, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BodyHasMisEscapedNewlines(c.body); got != c.want {
				t.Fatalf("BodyHasMisEscapedNewlines(%q)=%v want %v", c.body, got, c.want)
			}
		})
	}
}

// TestComparisonProseIsNotAPlaceholder pins the fix for the false positive
// found while wiring pkg/github's CreateIssue guard onto this validator
// (#7184). A sentence containing two comparison operators yields an
// angle-bracket span whose contents are a multi-word lowercase phrase once
// the surrounding spaces are removed -- "a < b and c > d" gives "< b and c >".
// Trimming that span made ordinary prose look exactly like an unfilled
// template and refused the filing on both the watcher and the proxy paths.
func TestComparisonProseIsNotAPlaceholder(t *testing.T) {
	for _, in := range []string{
		"holds when a < b and c > d in practice",
		"the value a < b and c > d holds",
		"assert that x < y and y > z before the sweep runs",
	} {
		if got, ok := UnsubstitutedTemplatePlaceholder(in); ok {
			t.Errorf("UnsubstitutedTemplatePlaceholder(%q) = %q, true; comparison prose is not a placeholder", in, got)
		}
	}

	// The tight form is still a placeholder, so the fix cannot be a blanket
	// escape hatch for anything containing a space.
	for _, in := range []string{
		"## Gap\n\n<what is missing or incorrect>\n",
		"<specific description of the documentation gap>",
	} {
		if _, ok := UnsubstitutedTemplatePlaceholder(in); !ok {
			t.Errorf("UnsubstitutedTemplatePlaceholder(%q) = false; a tight multi-word span is still a placeholder", in)
		}
	}
}

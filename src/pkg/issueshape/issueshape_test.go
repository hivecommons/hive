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

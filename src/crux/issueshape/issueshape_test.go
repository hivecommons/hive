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

func TestUnchosenOptionList(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"issue #7898 body verbatim", "## Priority\n- Impact: high/medium/low\n- Effort: high/medium/low\n", true},
		{"sec-check severity list", "**Severity**: critical/high/medium/low", true},
		{"case-insensitive", "Impact: HIGH/Medium/low", true},
		{"a chosen value", "- Impact: high\n- Effort: low", false},
		{"two of three is not the list", "risk is high/medium at most", false},
		{"unrelated slashes", "see pkg/proxy/github_proxy.go and v4/v5 branches", false},
		{"no list", "a perfectly ordinary sentence", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := UnchosenOptionList(c.text)
			if got != c.want {
				t.Fatalf("UnchosenOptionList(%q)=%v want %v", c.text, got, c.want)
			}
		})
	}
}

// Every issue the hive files names its filer in the title; TitleFor is the
// one helper that adds the tag, and it never adds a second one.
func TestTitleForTagsOnceAndKeepsExistingTag(t *testing.T) {
	cases := []struct{ agent, title, want string }{
		{"reviewer", "Review backlog (security): token logged", "[reviewer] Review backlog (security): token logged"},
		{"Reviewer", "  Review backlog (style): x  ", "[reviewer] Review backlog (style): x"},
		{"reviewer", "[reviewer] Review backlog (security): token logged", "[reviewer] Review backlog (security): token logged"},
		{"tester", "[Tests] v5 test failure on 4536f90", "[Tests] v5 test failure on 4536f90"},
		{"dashboard", "[ACMM L0] Add CI/CD pipeline", "[ACMM L0] Add CI/CD pipeline"},
		{"selfreport", "[selfreport] unclassified_error_repeated: x", "[selfreport] unclassified_error_repeated: x"},
		{"governor", "refactor: src/a.go is a merge hotspot — split into 4 units", "[governor] refactor: src/a.go is a merge hotspot — split into 4 units"},
		{"", "untagged stays untagged", "untagged stays untagged"},
		{"scanner", "", ""},
	}
	for _, tc := range cases {
		if got := TitleFor(tc.agent, tc.title); got != tc.want {
			t.Errorf("TitleFor(%q, %q) = %q, want %q", tc.agent, tc.title, got, tc.want)
		}
		if again := TitleFor(tc.agent, TitleFor(tc.agent, tc.title)); again != tc.want {
			t.Errorf("TitleFor is not idempotent for (%q, %q): %q", tc.agent, tc.title, again)
		}
	}
}

// Dedupe compares titles with the tag ignored, so an issue filed before
// titles carried the filer still matches its tagged re-filing.
func TestStripAgentPrefixAndSameTitle(t *testing.T) {
	if got := StripAgentPrefix("[reviewer] Review backlog (security): x"); got != "Review backlog (security): x" {
		t.Fatalf("StripAgentPrefix = %q", got)
	}
	if got := StripAgentPrefix("  no tag here "); got != "no tag here" {
		t.Fatalf("StripAgentPrefix(untagged) = %q", got)
	}
	if got := AgentPrefix("[Scanner] split x.go"); got != "scanner" {
		t.Fatalf("AgentPrefix = %q", got)
	}
	if AgentPrefix("plain") != "" || HasAgentPrefix("plain") {
		t.Fatal("untagged title reported a tag")
	}
	if !SameTitle("Review backlog (security): x", "[reviewer] Review backlog (security): x") {
		t.Fatal("legacy untagged title must match its tagged successor")
	}
	if SameTitle("[reviewer] Review backlog (security): x", "[reviewer] Review backlog (security): y") {
		t.Fatal("different subjects must not match")
	}
}

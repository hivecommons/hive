package dashboard

import "testing"

// titleCaseWords must replicate the deprecated strings.Title word-boundary
// rule byte-for-byte: a boundary is unicode whitespace only, so runs of
// non-letter, non-space characters (underscores, hyphens inside a word) do
// NOT start a new word.
func TestTitleCaseWords(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"single word", "pattern", "Pattern"},
		{"two words", "coverage gap", "Coverage Gap"},
		{"underscore is not a boundary", "test_scaffold", "Test_scaffold"},
		{"hyphen is not a boundary", "hold-gated mode", "Hold-gated Mode"},
		{"already titled", "Already Titled", "Already Titled"},
		{"leading spaces", "  two  spaces", "  Two  Spaces"},
		{"tab and newline are boundaries", "a\tb\nc", "A\tB\nC"},
		{"digits are untouched", "3 dogs", "3 Dogs"},
		{"unicode letter", "über wichtig", "Über Wichtig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleCaseWords(tc.in); got != tc.want {
				t.Fatalf("titleCaseWords(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// issueURLFor builds the canonical GitHub issue URL, and returns "" for the
// inputs the triage snapshot can legitimately lack (no repo, no number).
func TestIssueURLFor(t *testing.T) {
	cases := []struct {
		name, repo string
		number     int
		want       string
	}{
		{"valid", "kubestellar/hive", 42, "https://github.com/kubestellar/hive/issues/42"},
		{"empty repo", "", 42, ""},
		{"zero number", "kubestellar/hive", 0, ""},
		{"negative number", "kubestellar/hive", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := issueURLFor(tc.repo, tc.number); got != tc.want {
				t.Fatalf("issueURLFor(%q, %d) = %q, want %q", tc.repo, tc.number, got, tc.want)
			}
		})
	}
}

// intFromAny accepts every shape an integer can take after (or without) a JSON
// round-trip — float64, int, int64 — and yields 0 for anything else rather
// than panicking.
func TestIntFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64 from JSON", float64(7), 7},
		{"float64 truncates", float64(7.9), 7},
		{"native int", int(3), 3},
		{"int64", int64(5), 5},
		{"string is not a number", "12", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intFromAny(tc.in); got != tc.want {
				t.Fatalf("intFromAny(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// stringFromAny yields "" for missing or wrongly-typed values instead of
// panicking on the type assertion.
func TestStringFromAny(t *testing.T) {
	if got := stringFromAny("ok"); got != "ok" {
		t.Fatalf("stringFromAny(\"ok\") = %q, want \"ok\"", got)
	}
	if got := stringFromAny(nil); got != "" {
		t.Fatalf("stringFromAny(nil) = %q, want \"\"", got)
	}
	if got := stringFromAny(42); got != "" {
		t.Fatalf("stringFromAny(42) = %q, want \"\"", got)
	}
}

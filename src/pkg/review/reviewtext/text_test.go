package reviewtext

import "testing"

func TestConcreteRemediationText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want bool
	}{
		{name: "empty", text: "", want: false},
		{name: "whitespace", text: "  \n ", want: false},
		{name: "vague", text: "Something feels off here.", want: false},
		{name: "add verb", text: "Add a nil check before the loop.", want: true},
		{name: "file reference", text: "see pkg/foo/bar.go:42", want: true},
		{name: "changelog ask", text: "Missing changelog fragment.", want: true},
		{name: "uppercase", text: "RENAME the helper.", want: true},
		{name: "either add", text: "Either add the field or drop the flag.", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ConcreteRemediationText(tc.text); got != tc.want {
				t.Fatalf("ConcreteRemediationText(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestRiskEscalationText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want bool
	}{
		{name: "empty", text: "", want: false},
		{name: "benign remediation", text: "Add a unit test for the parser.", want: false},
		{name: "security", text: "This weakens the Security posture.", want: true},
		{name: "data loss", text: "Possible data loss on restart.", want: true},
		{name: "licensing", text: "licensing question for the vendored file", want: true},
		{name: "cannot judge", text: "I could not judge the migration safely.", want: true},
		{name: "spend", text: "increases spend on the runner pool", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RiskEscalationText(tc.text); got != tc.want {
				t.Fatalf("RiskEscalationText(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestRiskEscalationTermsAreLowercaseAndNonEmpty(t *testing.T) {
	t.Parallel()
	terms := RiskEscalationTerms()
	if len(terms) == 0 {
		t.Fatal("RiskEscalationTerms returned no terms")
	}
	seen := map[string]bool{}
	for _, term := range terms {
		if term == "" {
			t.Fatal("RiskEscalationTerms contains an empty term")
		}
		if seen[term] {
			t.Fatalf("RiskEscalationTerms contains duplicate %q", term)
		}
		seen[term] = true
		for _, r := range term {
			if r >= 'A' && r <= 'Z' {
				t.Fatalf("term %q is not lowercase; matching lowercases the input", term)
			}
		}
	}
}

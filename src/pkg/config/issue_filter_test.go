package config

import "testing"

// TestIssueFilterAdmits_AbsentAdmitsAll is the regression pin: a zero-value
// filter must admit EVERYTHING — including issues with no labels at all —
// because absent config means "current behavior exactly". A default-on filter
// would silently idle every existing hive.
func TestIssueFilterAdmits_AbsentAdmitsAll(t *testing.T) {
	var f IssueFilterConfig
	if !f.IsZero() {
		t.Fatal("zero-value filter must report IsZero")
	}
	for _, labels := range [][]string{nil, {}, {"bug"}, {"approved"}, {"anything", "else"}} {
		if !f.Admits(labels) {
			t.Errorf("zero-value filter refused labels %v — absent config must change nothing", labels)
		}
	}
}

// TestIssueFilterAdmits_RequireLabel: positive control (the labeled issue IS
// admitted) plus the refusal for the unlabeled one — so the test cannot pass
// by refusing everything.
func TestIssueFilterAdmits_RequireLabel(t *testing.T) {
	f := IssueFilterConfig{RequireLabels: []string{"approved-for-agents"}}
	if f.IsZero() {
		t.Fatal("filter with require_labels must not report IsZero")
	}
	if !f.Admits([]string{"bug", "approved-for-agents"}) {
		t.Error("positive control failed: issue carrying the required label was refused")
	}
	if !f.Admits([]string{"Approved-For-Agents"}) {
		t.Error("label match must be case-insensitive")
	}
	if f.Admits([]string{"bug"}) {
		t.Error("issue without the required label was admitted")
	}
	if f.Admits(nil) {
		t.Error("unlabeled issue was admitted despite require_labels")
	}
	// Exact match only: a prefix must NOT satisfy an approval gate.
	if f.Admits([]string{"approved-for-agents-maybe"}) {
		t.Error("prefix-extended label satisfied the require gate — matching must be exact")
	}
}

// TestIssueFilterAdmits_RequireAnyOf: multiple require labels are OR'd.
func TestIssueFilterAdmits_RequireAnyOf(t *testing.T) {
	f := IssueFilterConfig{RequireLabels: []string{"clanker", "triage-accepted"}}
	if !f.Admits([]string{"triage-accepted"}) {
		t.Error("issue with the second of two require labels was refused")
	}
	if !f.Admits([]string{"clanker"}) {
		t.Error("issue with the first of two require labels was refused")
	}
	if f.Admits([]string{"triage"}) {
		t.Error("near-miss label admitted")
	}
}

// TestIssueFilterAdmits_ExcludeWinsOverRequire: an issue carrying BOTH a
// required and an excluded label is refused — exclusion is the stronger claim.
func TestIssueFilterAdmits_ExcludeWinsOverRequire(t *testing.T) {
	f := IssueFilterConfig{
		RequireLabels: []string{"approved"},
		ExcludeLabels: []string{"no-ai"},
	}
	if !f.Admits([]string{"approved"}) {
		t.Error("positive control failed: approved-only issue refused")
	}
	if f.Admits([]string{"approved", "no-ai"}) {
		t.Error("exclude label did not win over require label")
	}
	if f.Admits([]string{"no-ai"}) {
		t.Error("excluded issue admitted")
	}
}

// TestIssueFilterAdmits_ExcludeOnly: exclude without require admits everything
// except the excluded labels.
func TestIssueFilterAdmits_ExcludeOnly(t *testing.T) {
	f := IssueFilterConfig{ExcludeLabels: []string{"wontfix"}}
	if !f.Admits([]string{"bug"}) {
		t.Error("exclude-only filter refused an ordinary issue")
	}
	if !f.Admits(nil) {
		t.Error("exclude-only filter refused an unlabeled issue")
	}
	if f.Admits([]string{"WontFix"}) {
		t.Error("excluded label admitted (case-insensitivity)")
	}
}

// TestIssueFilterAdmits_ConfigWhitespaceTrimmed: a stray space in YAML must not
// silently disable an approval gate.
func TestIssueFilterAdmits_ConfigWhitespaceTrimmed(t *testing.T) {
	f := IssueFilterConfig{RequireLabels: []string{" approved "}}
	if !f.Admits([]string{"approved"}) {
		t.Error("whitespace-padded configured label failed to match")
	}
}

func TestIssueFilterEqual(t *testing.T) {
	a := IssueFilterConfig{RequireLabels: []string{"x"}, ExcludeLabels: []string{"y"}}
	if !a.Equal(IssueFilterConfig{RequireLabels: []string{"x"}, ExcludeLabels: []string{"y"}}) {
		t.Error("identical filters reported unequal")
	}
	if a.Equal(IssueFilterConfig{RequireLabels: []string{"x"}}) {
		t.Error("filters with different exclude sets reported equal")
	}
	if (IssueFilterConfig{}).Equal(a) {
		t.Error("zero filter reported equal to a configured one")
	}
	if !(IssueFilterConfig{}).Equal(IssueFilterConfig{}) {
		t.Error("two zero filters reported unequal")
	}
}

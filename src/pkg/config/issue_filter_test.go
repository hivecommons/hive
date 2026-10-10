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

// TestIssueFilterAdmits_ConfigWhitespaceTrimmed: a stray space in YAML must not
// silently disable an approval gate.
func TestIssueFilterAdmits_ConfigWhitespaceTrimmed(t *testing.T) {
	f := IssueFilterConfig{RequireLabels: []string{" approved "}}
	if !f.Admits([]string{"approved"}) {
		t.Error("whitespace-padded configured label failed to match")
	}
}

func TestIssueFilterEqual(t *testing.T) {
	a := IssueFilterConfig{RequireLabels: []string{"x", "y"}}
	if !a.Equal(IssueFilterConfig{RequireLabels: []string{"x", "y"}}) {
		t.Error("identical filters reported unequal")
	}
	if a.Equal(IssueFilterConfig{RequireLabels: []string{"x"}}) {
		t.Error("filters with different require sets reported equal")
	}
	if (IssueFilterConfig{}).Equal(a) {
		t.Error("zero filter reported equal to a configured one")
	}
	if !(IssueFilterConfig{}).Equal(IssueFilterConfig{}) {
		t.Error("two zero filters reported unequal")
	}
}

func TestIssueFilterHardSuppressDefaultsAndOverrides(t *testing.T) {
	var f IssueFilterConfig
	if got := f.HardSuppressIssueBucket([]string{"needs-direction"}); got != DefaultNeedsDirectionLabel {
		t.Fatalf("default bucket = %q, want %q", got, DefaultNeedsDirectionLabel)
	}

	f.HardSuppressLabels.NeedsDirection = []string{"direction-needed"}
	if got := f.HardSuppressIssueBucket([]string{"needs-direction"}); got != "" {
		t.Fatalf("default label matched after override: %q", got)
	}
	if got := f.HardSuppressIssueBucket([]string{"Direction-Needed"}); got != DefaultNeedsDirectionLabel {
		t.Fatalf("override bucket = %q, want %q", got, DefaultNeedsDirectionLabel)
	}
}

// TestIssueFilterNeedsDecisionLabel: the issue relay parks an agent-filed
// decision issue with the first configured needs-decision label, falling back
// to the built-in default (hivecommons/hive#11215).
func TestIssueFilterNeedsDecisionLabel(t *testing.T) {
	var f IssueFilterConfig
	if got := f.NeedsDecisionLabel(); got != DefaultNeedsDecisionLabel {
		t.Errorf("default NeedsDecisionLabel = %q, want %q", got, DefaultNeedsDecisionLabel)
	}
	f.HardSuppressLabels.NeedsDecision = []string{" ", "owner-call", "decide"}
	if got := f.NeedsDecisionLabel(); got != "owner-call" {
		t.Errorf("configured NeedsDecisionLabel = %q, want %q", got, "owner-call")
	}
}

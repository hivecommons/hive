package classify

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/standby"
)

// The classifier's standby item-tier PROPOSAL, step S7 of RFC #7629.
//
// These tests hold two things: that the proposal is derived from labels the
// way lane routing reads labels, and that it is exactly as non-binding as its
// documentation says. The authority tests live in pkg/standby — this file only
// has to prove the proposal is honest about what it is.

func TestProposeStandbyItemTierFromLabels(t *testing.T) {
	for _, tc := range []struct {
		name      string
		labels    []string
		wantTier  string
		wantLabel string
	}{
		{"nothing to say about an ordinary issue", []string{"kind/bug", "area/api"}, "", ""},
		{"no labels at all", nil, "", ""},
		{"a security label proposes the strongest tier", []string{"kind/security"}, "T1", "kind/security"},
		{"a regression label does too", []string{"kind/regression"}, "T1", "kind/regression"},
		{"an auto-qa finding proposes the weakest", []string{"auto-qa-finding"}, "T3", "auto-qa-finding"},
		{"a Simple keyword as a label proposes the weakest", []string{"typo"}, "T3", "typo"},
		{"a namespaced Simple keyword matches its segment", []string{"kind/typo"}, "T3", "kind/typo"},
		{"the strong signal wins over a cosmetic one", []string{"typo", "kind/security"}, "T1", "kind/security"},
		{"casing is not substance", []string{"KIND/Security"}, "T1", "kind/security"},
		{"a substring is not a match", []string{"typographic-review"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ProposeStandbyItemTierFromLabels(tc.labels)
			if got.Tier != tc.wantTier || got.Label != tc.wantLabel {
				t.Errorf("ProposeStandbyItemTierFromLabels(%v) = %+v, want tier %q from label %q",
					tc.labels, got, tc.wantTier, tc.wantLabel)
			}
		})
	}
}

// The title is NOT evidence. "fix typo in the scheduler's lock ordering" is
// not a typo, and a proposal drawn from prose would be a guess wearing a
// tier's clothes — so an issue whose title alone looks cheap proposes nothing.
func TestProposeStandbyItemTierIgnoresTheTitle(t *testing.T) {
	issue := github.Issue{Title: "typo: rename the const in the retry loop", Labels: []string{"kind/bug"}}
	if got := ProposeStandbyItemTier(issue); got.Tier != "" {
		t.Errorf("ProposeStandbyItemTier(%q) = %+v, want no proposal: the title is not evidence", issue.Title, got)
	}
	// Whereas classifyTier, which the dashboard uses for model routing, does
	// read the title — the two are deliberately different questions.
	if got := Classify(issue).Tier; got != TierSimple {
		t.Errorf("precondition: Classify().Tier = %q, want %q — this test is only meaningful while the title still moves the complexity tier",
			got, TierSimple)
	}
}

// TestStandbyLabelMatchingAgreesWithLaneRouting: pkg/standby restates this
// package's label rule rather than importing it, so that the matching package
// stays dependency-free and pure. This test is what stops the two copies
// drifting — the failure mode #5856 was, one loose matcher handing work
// somewhere it should not go.
func TestStandbyLabelMatchingAgreesWithLaneRouting(t *testing.T) {
	labels := []string{
		"dependencies", "kind/dependencies", "dependencies-bot", "external-dependencies",
		"ai-fix-requested", "fix", "kind/bug", "area/api/v2", "", "  ", "Dependencies",
	}
	tokens := []string{"dependencies", "fix", "bug", "api", "v2", "", "kind"}
	for _, label := range labels {
		for _, token := range tokens {
			// classify lowercases both before routing; standby.LabelMatches
			// folds them itself, so feed the folded form to both.
			folded, foldedToken := strings.ToLower(strings.TrimSpace(label)), strings.ToLower(strings.TrimSpace(token))
			want := labelMatchesRoutingToken(folded, foldedToken)
			if got := standby.LabelMatches(label, token); got != want {
				t.Errorf("label %q, token %q: standby.LabelMatches = %v, lane routing = %v — the two copies of the rule have drifted",
					label, token, got, want)
			}
		}
	}
}

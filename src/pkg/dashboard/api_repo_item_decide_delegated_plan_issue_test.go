package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/delegatedecision"
)

// hivecommons/hive#15754: default-approve on an issue said "Approve #15734
// as it is and let the merge queue merge it." The plan for an issue speaks
// issue; a PR's stays as it was.
func TestDelegatedPlanWordingBranchesOnItemType(t *testing.T) {
	approve := delegatedecision.Decision{Verdict: delegatedecision.VerdictApprove, Rule: "default-approve"}
	for _, tc := range []struct {
		name     string
		in       delegatedecision.Input
		d        delegatedecision.Decision
		summary  string
		first    string
		notWords []string
	}{
		{"issue approve", delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 15734}, approve,
			"Approve the direction of #15734 and let the hive pick it up.",
			"Hive records the approval and lifts the decision park on #15734.", []string{"merge", "commit", "CI"}},
		{"pr approve", delegatedecision.Input{Kind: delegatedecision.KindPR, Number: 8}, approve,
			"Approve #8 as it is and let the merge queue merge it.",
			"Hive approves the current commit.", nil},
		{"issue close duplicate", delegatedecision.Input{Kind: delegatedecision.KindIssue, Number: 5},
			delegatedecision.Decision{Verdict: delegatedecision.VerdictCloseDuplicate, Actions: []delegatedecision.Action{{Target: 4}}},
			"Close #5 as a duplicate of #4.", "#5 is closed with a comment pointing at #4.", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, consequences := delegatedPlanWording(tc.in, tc.d)
			summary, consequences = delegatedPlanIssueWording(tc.in, tc.d, summary, consequences)
			if summary != tc.summary || len(consequences) == 0 || consequences[0] != tc.first {
				t.Fatalf("summary=%q consequences=%q", summary, consequences)
			}
			text := summary + " " + strings.Join(consequences, " ")
			for _, w := range tc.notWords {
				if strings.Contains(text, w) {
					t.Errorf("issue plan mentions %q: %s", w, text)
				}
			}
		})
	}
}

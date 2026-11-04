package review

import (
	"strings"
	"testing"
)

// The two asks that parked green, finding-free, hive-authored PRs for a
// person (hivecommons/docs#618 and hivecommons/hive#16456), verbatim.
const (
	docs618Ask   = "@clubanderson should decide whether to apply the hive-docs-refresh.yml opt-in from the PR body (adding the \"hive-docs-refresh\" workflow_dispatch input) and whether this PR should merge ahead of that workflow edit."
	hive16456Ask = "Decision needed: @clubanderson should confirm the merge-request relay may lift the hold…"
)

func TestClassifyDecisionQuestion(t *testing.T) {
	green := func(q string, files ...DecisionQuestionFile) DecisionQuestionInput {
		return DecisionQuestionInput{Question: q, HiveAuthored: true, CIGreen: true, Files: files}
	}
	cases := []struct {
		name   string
		in     DecisionQuestionInput
		want   DecisionQuestionClass
		reason string
	}{
		{
			name: "docs#618 scope question is the hive's",
			in:   green(docs618Ask, DecisionQuestionFile{Path: "docs/sync/README.md"}, DecisionQuestionFile{Path: "scripts/sync.sh"}),
			want: DecisionQuestionScope, reason: "scope/follow-up",
		},
		{
			name: "hive#16456 confirm-the-relay question is the hive's",
			in:   green(hive16456Ask, DecisionQuestionFile{Path: "src/pkg/github/review_verdict_relay.go"}),
			want: DecisionQuestionScope, reason: "scope/follow-up",
		},
		{
			name: "should-we-also follow-up",
			in:   green("Should we also update the operator reference to mention the new flag, or is a follow-up fine?"),
			want: DecisionQuestionScope,
		},
		{
			name: "confirm acceptable wording",
			in:   green("@clubanderson should confirm the renamed caption is acceptable before this merges."),
			want: DecisionQuestionScope,
		},
		{
			name: "fork PR editing a workflow is a sign-off",
			in: DecisionQuestionInput{Question: "Should the new job run on pull_request_target?", HiveAuthored: true, CIGreen: true, FromFork: true,
				Files: []DecisionQuestionFile{{Path: ".github/workflows/ci.yml"}}},
			want: DecisionQuestionPolicySignOff, reason: "fork PR edits workflow path",
		},
		{
			name: "same-repo hive PR editing a workflow is still the hive's",
			in:   green("Should this merge ahead of the docs refresh?", DecisionQuestionFile{Path: ".github/workflows/docs.yml"}),
			want: DecisionQuestionScope,
		},
		{
			name: "OWNERS edit is a sign-off however it is phrased",
			in:   green("Should we also add the new reviewer to the docs alias?", DecisionQuestionFile{Path: "docs/OWNERS"}),
			want: DecisionQuestionPolicySignOff, reason: "policy path docs/OWNERS",
		},
		{
			name: "SECURITY.md edit is a sign-off",
			in:   green("Confirm the new disclosure address is right.", DecisionQuestionFile{Path: "SECURITY.md"}),
			want: DecisionQuestionPolicySignOff, reason: "policy path",
		},
		{
			name: "auto_merge config change in a patch is a sign-off",
			in: green("Confirm the lane should merge its own PRs.", DecisionQuestionFile{Path: "deploy/overlays/prod/governor.yaml",
				Patch: "@@ -1,3 +1,4 @@\n repos:\n   hive:\n+    auto_merge: true\n"}),
			want: DecisionQuestionPolicySignOff, reason: "changes auto_merge",
		},
		{
			name: "auto_merge named in a Go test is not a policy edit",
			in: green("Should the test also cover the off case?", DecisionQuestionFile{Path: "src/pkg/config/config_test.go",
				Patch: "+\tcfg.AutoMerge.Enabled = true // auto_merge: on\n"}),
			want: DecisionQuestionScope,
		},
		{
			name: "explicit sentinel guard is a sign-off",
			in:   green("The sentinel flagged the install script; @clubanderson should acknowledge it before merge."),
			want: DecisionQuestionPolicySignOff, reason: `owner-only guard "sentinel"`,
		},
		{
			name: "GitHub permissions guard is a sign-off",
			in:   green("Whether the App's GitHub permissions allow pushing this is the owner's call."),
			want: DecisionQuestionPolicySignOff, reason: "github permissions",
		},
		{
			name: "secret token guard is a sign-off",
			in:   green("Confirm the new token scope is intended."),
			want: DecisionQuestionPolicySignOff, reason: "token",
		},
		{
			name: "contributor PR is not a candidate",
			in:   DecisionQuestionInput{Question: "Should this also handle the empty case?", CIGreen: true},
			want: DecisionQuestionIneligible, reason: "not hive-authored",
		},
		{
			name: "red CI is not a candidate",
			in:   DecisionQuestionInput{Question: "Should this also handle the empty case?", HiveAuthored: true},
			want: DecisionQuestionIneligible, reason: "CI is not green",
		},
		{
			name: "open findings are not a candidate",
			in:   DecisionQuestionInput{Question: "Should this also handle the empty case?", HiveAuthored: true, CIGreen: true, UnresolvedFindings: true},
			want: DecisionQuestionIneligible, reason: "unresolved review findings",
		},
		{
			name: "no question",
			in:   DecisionQuestionInput{HiveAuthored: true, CIGreen: true},
			want: DecisionQuestionIneligible, reason: "no reviewer question",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := ClassifyDecisionQuestion(tc.in)
			if got != tc.want {
				t.Fatalf("class = %q (%s), want %q", got, reason, tc.want)
			}
			if tc.reason != "" && !strings.Contains(strings.ToLower(reason), strings.ToLower(tc.reason)) {
				t.Fatalf("reason = %q, want it to mention %q", reason, tc.reason)
			}
		})
	}
}

// The two real asks still land in a DecisionRiskClass by wording alone
// (that is why they parked); the classifier is what lets the hive answer
// them regardless.
func TestClassifyDecisionQuestionRealAsksMatchRiskClassByWording(t *testing.T) {
	if _, risk := DecisionQuestionRiskClass(docs618Ask); !risk {
		t.Fatalf("docs#618 ask no longer matches a risk class; the classifier is not what routes it")
	}
}

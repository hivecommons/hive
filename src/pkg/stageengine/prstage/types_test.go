package prstage

import (
	"reflect"
	"testing"
)

func TestDetailRedCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		detail Detail
		head   string
		want   string
	}{
		{name: "no red checks", detail: Detail{}, head: "abc", want: ""},
		{name: "same head", detail: Detail{RedChecks: []string{"lint", "test"}, RedChecksHead: "abc"}, head: "abc", want: "lint"},
		{name: "stale head", detail: Detail{RedChecks: []string{"lint"}, RedChecksHead: "abc"}, head: "def", want: ""},
		{name: "head unknown on detail", detail: Detail{RedChecks: []string{"lint"}}, head: "def", want: "lint"},
		{name: "head unknown on query", detail: Detail{RedChecks: []string{"lint"}, RedChecksHead: "abc"}, head: "", want: "lint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.detail.RedCheck(tc.head); got != tc.want {
				t.Fatalf("RedCheck(%q) = %q, want %q", tc.head, got, tc.want)
			}
		})
	}
}

func TestNormalizeReviewVerdict(t *testing.T) {
	t.Parallel()
	remediation := "Add the missing changelog fragment under changelog.d/ and reword the title."
	cases := []struct {
		name string
		in   ReviewVerdict
		want string
	}{
		{name: "not configured", in: ReviewVerdict{Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "requires_human"},
		{name: "approved", in: ReviewVerdict{Configured: true, Approved: true, Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "requires_human"},
		{name: "decision requested", in: ReviewVerdict{Configured: true, DecisionRequested: true, Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "requires_human"},
		{name: "known non-hive authorship", in: ReviewVerdict{Configured: true, HiveAuthoredKnown: true, HiveAuthored: false, Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "requires_human"},
		{name: "fix cycle cap", in: ReviewVerdict{Configured: true, FixCycleCapReached: true, Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "requires_human"},
		{name: "other verdict untouched", in: ReviewVerdict{Configured: true, Verdict: "approve", HumanSummaries: []string{remediation}}, want: "approve"},
		{name: "risk text stays human", in: ReviewVerdict{Configured: true, Verdict: "requires_human", DecisionQuestion: "security: add a credential check"}, want: "requires_human"},
		{name: "vague text stays human", in: ReviewVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{"looks odd"}}, want: "requires_human"},
		{name: "concrete remediation becomes changes_requested", in: ReviewVerdict{Configured: true, Verdict: "requires_human", HumanSummaries: []string{remediation}}, want: "changes_requested"},
		{name: "case and whitespace tolerant", in: ReviewVerdict{Configured: true, Verdict: " Requires_Human ", HiveAuthoredKnown: true, HiveAuthored: true, HumanSummaries: []string{remediation}}, want: "changes_requested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeReviewVerdict(tc.in)
			if got.Verdict != tc.want {
				t.Fatalf("NormalizeReviewVerdict verdict = %q, want %q", got.Verdict, tc.want)
			}
			// Only the verdict may change; every other field passes through.
			tc.in.Verdict = got.Verdict
			if got.Configured != tc.in.Configured || got.Approved != tc.in.Approved || got.DecisionRequested != tc.in.DecisionRequested ||
				got.FixCycleCapReached != tc.in.FixCycleCapReached || got.HiveAuthoredKnown != tc.in.HiveAuthoredKnown || got.HiveAuthored != tc.in.HiveAuthored ||
				got.DecisionQuestion != tc.in.DecisionQuestion || len(got.HumanSummaries) != len(tc.in.HumanSummaries) {
				t.Fatalf("NormalizeReviewVerdict changed fields other than Verdict: %+v", got)
			}
		})
	}
}

func TestReviewVerdictWithDecisionAnswer(t *testing.T) {
	t.Parallel()
	base := ReviewVerdict{Verdict: "requires_human"}
	cases := []struct {
		name      string
		by, head  string
		directive string
		want      ReviewVerdict
	}{
		{
			name: "blank actor leaves verdict untouched",
			by:   "  ", head: "abc", directive: DecisionDirectiveApprove,
			want: base,
		},
		{
			name: "blank head leaves verdict untouched",
			by:   "alice", head: "", directive: DecisionDirectiveApprove,
			want: base,
		},
		{
			name: "approve directive is preserved and trimmed",
			by:   " alice ", head: " abc ", directive: " approve ",
			want: ReviewVerdict{
				Verdict: "requires_human", DecisionAnswered: true,
				DecisionAnsweredBy: "alice", DecisionAnsweredHead: "abc",
				DecisionDirective: DecisionDirectiveApprove,
			},
		},
		{
			name: "changes_requested directive",
			by:   "alice", head: "abc", directive: DecisionDirectiveChanges,
			want: ReviewVerdict{
				Verdict: "requires_human", DecisionAnswered: true,
				DecisionAnsweredBy: "alice", DecisionAnsweredHead: "abc",
				DecisionDirective: DecisionDirectiveChanges,
			},
		},
		{
			name: "unknown directive defaults to changes_requested",
			by:   "alice", head: "abc", directive: "shrug",
			want: ReviewVerdict{
				Verdict: "requires_human", DecisionAnswered: true,
				DecisionAnsweredBy: "alice", DecisionAnsweredHead: "abc",
				DecisionDirective: DecisionDirectiveChanges,
			},
		},
		{
			name: "empty directive defaults to changes_requested",
			by:   "alice", head: "abc", directive: "",
			want: ReviewVerdict{
				Verdict: "requires_human", DecisionAnswered: true,
				DecisionAnsweredBy: "alice", DecisionAnsweredHead: "abc",
				DecisionDirective: DecisionDirectiveChanges,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := base.WithDecisionAnswer(tc.by, tc.head, tc.directive); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("WithDecisionAnswer(%q,%q,%q) = %+v, want %+v", tc.by, tc.head, tc.directive, got, tc.want)
			}
		})
	}
}

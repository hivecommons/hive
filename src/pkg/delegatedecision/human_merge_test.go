package delegatedecision

import (
	"strings"
	"testing"
)

// "Let hive decide" on a PR at 090 Ready for human merge
// (hivecommons/hive#15173): approve and let the sweep merge it under its
// own number when review, CI and mergeability are good; adopt only when
// the head cannot merge as it is.
func TestDecideHumanMergePaths(t *testing.T) {
	base := Input{Repo: "hivecommons/hotshot", Number: 317, Kind: KindPR, Author: "Karibhou",
		ReadyForHumanMerge: true, ReviewKnown: true, ReviewGood: true, CIGreen: true, HeadSHA: "abc123"}
	cases := []struct {
		name    string
		mutate  func(*Input)
		verdict Verdict
		rule    string
		action  ActionKind
		path    string
	}{
		{"good review, green, mergeable → approve", func(*Input) {}, VerdictApprove, RuleHumanMergeApprove, ActionApproveReview, "review passed, CI green, mergeable"},
		{"no review recorded at 090 → approve, says the lane found nothing wrong", func(in *Input) { in.ReviewKnown, in.ReviewGood = false, false }, VerdictApprove, RuleHumanMergeApprove, ActionApproveReview, "review: lane found nothing wrong, CI green, mergeable"},
		{"red CI → adopt", func(in *Input) { in.CIGreen = false }, VerdictAdoptSupersede, RuleHumanMergeAdopt, ActionAdoptSupersede, "CI red"},
		{"conflicting → adopt with rebase", func(in *Input) { in.Conflicting = true; in.CIGreen = false }, VerdictAdoptSupersede, RuleHumanMergeAdopt, ActionAdoptSupersede, "CI red, conflicting, needs rebase"},
		{"review not good → adopt", func(in *Input) { in.ReviewGood = false }, VerdictAdoptSupersede, RuleHumanMergeAdopt, ActionAdoptSupersede, "review not good"},
		{"sign-off missing → adopt", func(in *Input) { in.DCOMissing = true }, VerdictAdoptSupersede, RuleHumanMergeAdopt, ActionAdoptSupersede, "sign-off missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			d, ok := DecideHumanMerge(in)
			if !ok {
				t.Fatal("expected a 090 decision")
			}
			if d.Verdict != tc.verdict || d.Rule != tc.rule || d.Path != tc.path {
				t.Fatalf("got verdict=%s rule=%s path=%q, want %s/%s/%q", d.Verdict, d.Rule, d.Path, tc.verdict, tc.rule, tc.path)
			}
			if d.KeepsPark {
				t.Fatal("a 090 decision must not keep the park")
			}
			if len(d.Actions) != 2 || d.Actions[0].Kind != ActionCommentDecision || d.Actions[1].Kind != tc.action {
				t.Fatalf("actions = %+v, want comment + %s", d.Actions, tc.action)
			}
			if !strings.Contains(d.Text, "#317") || !strings.Contains(d.Text, tc.path) {
				t.Fatalf("text %q must name the PR and the path", d.Text)
			}
			if strings.ContainsAny(d.Path, "()_") {
				t.Fatalf("path %q must be plain words", d.Path)
			}
			if tc.verdict == VerdictApprove && strings.Contains(strings.ToLower(d.Text), "adopt") {
				t.Fatalf("approve text must not mention adoption: %q", d.Text)
			}
			if tc.verdict == VerdictAdoptSupersede && !strings.Contains(d.Text, "author's commits") {
				t.Fatalf("adopt text must say the author's commits are preserved: %q", d.Text)
			}
			// The rails must keep both the comment and the effect.
			railed, _ := RunHumanMerge(in)
			if len(railed.Actions) != 2 || len(railed.Refusals) != 0 {
				t.Fatalf("rails altered the decision: %+v", railed)
			}
			in.Labels = append(in.Labels, LabelHold)
			if held, _ := RunHumanMerge(in); held.Verdict != VerdictRefused {
				t.Fatalf("a held PR must be refused, got %s", held.Verdict)
			}
		})
	}
}

func TestDecideHumanMergeOnlyForReadyPRs(t *testing.T) {
	if _, ok := DecideHumanMerge(Input{Kind: KindPR, Number: 1, CIGreen: true}); ok {
		t.Fatal("a PR not at 090 is not a human-merge decision")
	}
	if _, ok := DecideHumanMerge(Input{Kind: KindIssue, Number: 1, ReadyForHumanMerge: true}); ok {
		t.Fatal("an issue is never a human-merge decision")
	}
}

func TestHumanMergeApprovable(t *testing.T) {
	if !HumanMergeApprovable(Input{CIGreen: true}) {
		t.Fatal("green, no review, no conflict, signed → approvable")
	}
	for name, in := range map[string]Input{
		"red":        {CIGreen: false},
		"conflict":   {CIGreen: true, Conflicting: true},
		"bad review": {CIGreen: true, ReviewKnown: true},
		"dco":        {CIGreen: true, DCOMissing: true},
	} {
		if HumanMergeApprovable(in) {
			t.Fatalf("%s must not be approvable", name)
		}
	}
}

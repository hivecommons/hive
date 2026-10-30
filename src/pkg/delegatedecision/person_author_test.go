package delegatedecision

import (
	"strings"
	"testing"
)

// hivecommons/hive#15202: a PR pushed by a person (author/human label or one
// of this hive's owners) is never a "contributor" with an offline hive. The
// delegate hands it back to that person by name and never adopts it.
func TestDecidePRPersonAuthoredNeverAdopted(t *testing.T) {
	base := Input{Kind: KindPR, Number: 15157, Title: "Fix the tracker", Author: "clubanderson", ContributorOnline: false}
	cases := []struct {
		name     string
		in       Input
		verdict  Verdict
		rule     string
		wantText []string
		noAdopt  bool
	}{
		{
			name:    "human author offline conflicting → return to author, not adopt",
			in:      with(base, func(i *Input) { i.HumanAuthor = true; i.Conflicting = true; i.CIGreen = true }),
			verdict: VerdictDirection, rule: RuleReturnToAuthor,
			wantText: []string{"@clubanderson", "rebase onto the base branch", "#15157"},
			noAdopt:  true,
		},
		{
			name:    "owner author offline red CI → return to author",
			in:      with(base, func(i *Input) { i.OwnerAuthor = true; i.CIGreen = false }),
			verdict: VerdictDirection, rule: RuleReturnToAuthor,
			wantText: []string{"@clubanderson", "make CI green"},
			noAdopt:  true,
		},
		{
			name:    "human author offline, adoption stalled, green → approves (never adopts)",
			in:      with(base, func(i *Input) { i.HumanAuthor = true; i.AdoptionStalled = true; i.CIGreen = true; i.Small = true }),
			verdict: VerdictApprove, rule: "small-low-risk",
			noAdopt: true,
		},
		{
			name:    "human author green with an open reviewer question → relays the question",
			in:      with(base, func(i *Input) { i.HumanAuthor = true; i.CIGreen = true; i.Question = "is the retry bounded?" }),
			verdict: VerdictDirection, rule: RuleReturnToAuthor,
			wantText: []string{"@clubanderson, the reviewer asked: is the retry bounded?"},
			noAdopt:  true,
		},
		{
			name:    "clanker author offline conflicting still adopts",
			in:      with(base, func(i *Input) { i.Conflicting = true; i.CIGreen = true }),
			verdict: VerdictAdoptSupersede, rule: "contributor-offline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if d.Verdict != tc.verdict || d.Rule != tc.rule {
				t.Fatalf("got %s/%s want %s/%s (%+v)", d.Verdict, d.Rule, tc.verdict, tc.rule, d)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(d.Text, w) {
					t.Errorf("text %q lacks %q", d.Text, w)
				}
			}
			for _, a := range d.Actions {
				if tc.noAdopt && a.Kind == ActionAdoptSupersede {
					t.Errorf("person-authored PR must never be adopted: %+v", d)
				}
			}
		})
	}
}

// The rail holds even when a rule (or an LLM) asked for adoption outright.
func TestApplyRailsRefusesAdoptSupersedeForPersonAuthored(t *testing.T) {
	d := Decision{Verdict: VerdictAdoptSupersede, Rule: "contributor-offline", Text: "supersede now",
		Actions: []Action{decisionComment("supersede now"), {Kind: ActionAdoptSupersede}}}
	out := ApplyRails(d, Rails{PersonAuthored: true})
	for _, a := range out.Actions {
		if a.Kind == ActionAdoptSupersede {
			t.Fatalf("adopt-supersede survived the person-authored rail: %+v", out)
		}
	}
	found := false
	for _, r := range out.Refusals {
		if r == RefusalAdoptPersonAuthored {
			found = true
		}
	}
	if !found {
		t.Fatalf("refusal missing: %+v", out.Refusals)
	}
	if out.Verdict != VerdictAdoptSupersede || len(out.Actions) != 1 || out.Actions[0].Kind != ActionCommentDecision {
		t.Fatalf("expected the decision comment to survive alone: %+v", out)
	}
	if plain := RulePlain(RuleReturnToAuthor); plain == "" {
		t.Fatal("return-to-author needs a glossary entry")
	}
}

func with(in Input, f func(*Input)) Input { f(&in); return in }

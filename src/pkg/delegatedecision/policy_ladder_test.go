package delegatedecision

import (
	"slices"
	"strings"
	"testing"
)

// The #15219 shape (hivecommons/hive#15289): a clanker-relayed contributor
// PR parked for a decision, conflicting and offline, whose issue #15202 was
// already fixed by merged PR #15240.
var ladderPR = Input{
	Repo: "hivecommons/hive", Kind: KindPR, Number: 15219, Title: "Fix the thing", Body: "Fixes #15202", Author: "thatguywob",
	Labels:          []string{"author/clanker", LabelAdoptSupersede, LabelNeedsDecision, "needs-rebase"},
	AdoptionStalled: true, Conflicting: true, CIGreen: true,
}

func hasAction(d Decision, k ActionKind) bool {
	return slices.ContainsFunc(d.Actions, func(a Action) bool { return a.Kind == k })
}

func TestLadderAlreadyFixedClosesSupersededNeverAdopts(t *testing.T) {
	in := with(ladderPR, func(i *Input) {
		i.AlreadyFixedBy, i.AlreadyFixedIssue, i.AlreadyFixedSHA = 15240, 15202, "25de3ba24aa0"
	})
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"offline conflicting", in},
		{"grace elapsed", with(in, func(i *Input) { i.AdoptionGraceElapsed = true })},
		{"green", with(in, func(i *Input) { i.Conflicting = false })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if d.Verdict != VerdictCloseSuperseded || d.Rule != RuleAlreadyFixed || d.Rung != RuleAlreadyFixed {
				t.Fatalf("decision = %+v", d)
			}
			if hasAction(d, ActionAdoptSupersede) || !sameKinds(kinds(d), []ActionKind{ActionCloseDuplicate}) {
				t.Fatalf("actions = %v", kinds(d))
			}
			if d.Actions[0].Target != 15240 || !strings.Contains(d.Actions[0].Text, "#15240 (25de3ba)") || !strings.Contains(d.Actions[0].Text, "@thatguywob") {
				t.Fatalf("close action = %+v", d.Actions[0])
			}
			if d.Text != "#15202 already fixed by #15240 (25de3ba)" {
				t.Fatalf("text = %q", d.Text)
			}
			if d.Reason == "" {
				t.Fatal("reason must say why")
			}
		})
	}
}

func TestLadderAlreadyFixedWhilePausedOnlyRecommends(t *testing.T) {
	in := with(ladderPR, func(i *Input) { i.AlreadyFixedBy, i.AlreadyFixedIssue, i.RepoPaused = 15240, 15202, true })
	d := Run(in)
	if d.Verdict != VerdictRecommendOnly || !d.KeepsPark || d.Rule != RuleAlreadyFixed || hasAction(d, ActionCloseDuplicate) || hasAction(d, ActionAdoptSupersede) {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.Contains(d.Text, "close #15219 as superseded: #15202 already fixed by #15240") {
		t.Fatalf("text = %q", d.Text)
	}
}

func TestLadderMergeableAsIsGoesToMergeGate(t *testing.T) {
	d := Run(with(ladderPR, func(i *Input) { i.Conflicting = false; i.AdoptionGraceElapsed = true }))
	if d.Verdict != VerdictApprove || d.Rule != RuleMergeableAsIs || d.Rung != RuleMergeableAsIs || hasAction(d, ActionAdoptSupersede) {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.RuledOut) != 1 || !strings.HasPrefix(d.RuledOut[0], RuleAlreadyFixed+":") {
		t.Fatalf("ruled out = %v", d.RuledOut)
	}
}

func TestLadderConnectedAuthorIsReturnedNotAdopted(t *testing.T) {
	d := Run(with(ladderPR, func(i *Input) { i.AdoptionGraceElapsed = true; i.ContributorOnline = true }))
	if d.Verdict != VerdictDirection || d.Rule != RuleReturnToContributor || hasAction(d, ActionAdoptSupersede) {
		t.Fatalf("decision = %+v", d)
	}
	if !strings.HasPrefix(d.Text, "the author's clanker: rebase onto the base branch on #15219") {
		t.Fatalf("text = %q", d.Text)
	}
}

func TestLadderAdoptsLastAndRecordsRuledOut(t *testing.T) {
	d := Run(ladderPR)
	if d.Verdict != VerdictAdoptSupersede || d.Rule != "contributor-offline" || d.Rung != RungAdoptSupersede {
		t.Fatalf("decision = %+v", d)
	}
	var rungs []string
	for _, r := range d.RuledOut {
		rung, _, _ := strings.Cut(r, ":")
		rungs = append(rungs, rung)
	}
	if want := []string{RuleAlreadyFixed, RuleMergeableAsIs, RungRebaseThenMerge, RuleReturnToContributor}; !slices.Equal(rungs, want) {
		t.Fatalf("ruled out = %v, want rungs %v", d.RuledOut, want)
	}
	body := CommentBody(d, "h", "andy")
	if !strings.Contains(body, "**Why:** The author's clanker hasn't been connected since the nudge.") || !strings.Contains(body, "**Ruled out first:** already-fixed:") {
		t.Fatalf("comment:\n%s", body)
	}
}

// The text names the author's agent as it is: a clanker relay is never
// called "the author's hive" (hivecommons/hive#15289).
func TestLadderAuthorAgentWording(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		{"clanker", []string{"author/clanker"}, "the author's clanker"},
		{"hive", []string{"author/hive"}, "the author's hive"},
		{"unknown", nil, "the author"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := with(ladderPR, func(i *Input) { i.Labels = tc.labels })
			if got := in.AuthorAgent(); got != tc.want {
				t.Fatalf("AuthorAgent = %q, want %q", got, tc.want)
			}
			d := Run(in)
			if d.Reason != tc.want+" hasn't been connected since the nudge" {
				t.Fatalf("reason = %q", d.Reason)
			}
			if tc.name != "hive" && strings.Contains(d.Reason+d.Rationale+strings.Join(d.RuledOut, " "), "author's hive") {
				t.Fatalf("decision calls a non-hive author's agent their hive: %+v", d)
			}
		})
	}
	if got := (Input{AuthorClanker: true}).AuthorAgent(); got != "the author's clanker" {
		t.Fatalf("relay clanker: %q", got)
	}
}

// An owner's or author/human PR is never adopted, whatever the ladder
// would otherwise reach (hivecommons/hive#15202).
func TestLadderNeverAdoptsPersonAuthored(t *testing.T) {
	base := with(ladderPR, func(i *Input) { i.Labels = []string{"author/human"}; i.CIGreen = false; i.AdoptionGraceElapsed = true })
	for _, in := range []Input{
		with(base, func(i *Input) { i.HumanAuthor = true }),
		with(base, func(i *Input) { i.OwnerAuthor = true }),
	} {
		d := Run(in)
		if d.Verdict == VerdictAdoptSupersede || hasAction(d, ActionAdoptSupersede) || d.Rung == RungAdoptSupersede {
			t.Fatalf("person-authored PR adopted: %+v", d)
		}
	}
}

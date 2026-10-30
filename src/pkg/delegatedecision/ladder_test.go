package delegatedecision

import (
	"strings"
	"testing"
)

// hivecommons/hive#15289: the contributor-PR ladder. #15219 (author via a
// clanker, conflicting, offline) was adopted in supersede mode although
// #15202 had already been fixed and merged by #15240; the ladder closes it
// as superseded instead, and only reaches adoption as the last resort.
func TestDecideLadder(t *testing.T) {
	// The #15219 scenario as the dashboard gathers it.
	incident := Input{Repo: "hivecommons/hive", Kind: KindPR, Number: 15219, Title: "Fix the thing", Author: "thatguywob",
		Labels:      []string{"author/clanker", "adoption/supersede", "stage/085-needs-decision", "needs-rebase"},
		Conflicting: true, CIGreen: false, ContributorOnline: false, AdoptionStalled: true, AuthorTooling: AuthorToolingClanker}
	fixed := &MergedCloser{PR: 15240, Issue: 15202, SHA: "25de3ba24fa0c0ffee0000000000000000000000"}
	cases := []struct {
		name        string
		in          Input
		verdict     Verdict
		rule        string
		kinds       []ActionKind
		target      int
		textHas     []string
		ruledOutHas []string
		noLadder    bool
	}{
		{
			name:    "15219: issue closed by merged 15240 → close as superseded, never adopt",
			in:      with(incident, func(i *Input) { i.SupersededBy = fixed }),
			verdict: VerdictCloseSuperseded, rule: RuleAlreadyFixed, kinds: []ActionKind{ActionCloseDuplicate}, target: 15240,
			textHas: []string{"#15202 was already fixed by merged PR #15240 (25de3ba)"},
		},
		{
			name:    "already fixed on a paused repo → recommend only, no write",
			in:      with(incident, func(i *Input) { i.SupersededBy = fixed; i.RepoPaused = true }),
			verdict: VerdictRecommendOnly, rule: RuleAlreadyFixed, kinds: []ActionKind{ActionRecommendOnly},
			textHas: []string{"close #15219 as superseded"},
		},
		{
			name:    "already fixed, person-authored → recommend only (theirs to close)",
			in:      with(incident, func(i *Input) { i.SupersededBy = fixed; i.HumanAuthor = true; i.AuthorTooling = AuthorToolingPerson }),
			verdict: VerdictRecommendOnly, rule: RuleAlreadyFixed, kinds: []ActionKind{ActionRecommendOnly},
		},
		{
			name:    "declared supersession (no issue) → close as superseded",
			in:      with(incident, func(i *Input) { i.SupersededBy = &MergedCloser{PR: 15206} }),
			verdict: VerdictCloseSuperseded, rule: RuleAlreadyFixed, target: 15206, textHas: []string{"superseded by merged PR #15206"},
		},
		{
			name:    "mergeable as-is → merge gate, no adoption",
			in:      with(incident, func(i *Input) { i.Conflicting = false; i.CIGreen = true; i.AdoptionStalled = false }),
			verdict: VerdictApprove, rule: RuleMergeableAsIs, kinds: []ActionKind{ActionCommentDecision, ActionApproveReview},
			ruledOutHas: []string{RuleAlreadyFixed + ": no merged PR has closed this PR's issue"},
		},
		{
			name:    "offline clanker, conflicting → adopt-supersede as the last resort",
			in:      incident,
			verdict: VerdictAdoptSupersede, rule: RuleContributorOffline, kinds: []ActionKind{ActionCommentDecision, ActionAdoptSupersede},
			ruledOutHas: []string{
				RuleAlreadyFixed + ":", RuleMergeableAsIs + ": conflicting", RuleRebaseThenMerge + ":",
				RuleReturnToContributor + ": the author's clanker hasn't been connected since the nudge",
			},
		},
		{
			name:    "conflicts only, offline → rung 3 recorded as unavailable (#15104), adopted",
			in:      with(incident, func(i *Input) { i.CIGreen = true; i.AdoptionStalled = false }),
			verdict: VerdictAdoptSupersede, rule: RuleContributorOffline,
			ruledOutHas: []string{RuleRebaseThenMerge + ": conflicts are the only blocker, but the unattended rebase path (hivecommons/hive#15104) is not available yet"},
		},
		{
			name:    "online clanker, conflicting → returned to the contributor, not adopted",
			in:      with(incident, func(i *Input) { i.ContributorOnline = true }),
			verdict: VerdictDirection, rule: RuleReturnToContributor, kinds: []ActionKind{ActionCommentDecision},
			textHas: []string{"the author's clanker is connected: rebase onto the base branch, make CI green"},
		},
		{
			name:    "rework picked up by the relay → returned to the contributor",
			in:      with(incident, func(i *Input) { i.ReworkPickedUp = true; i.AuthorTooling = AuthorToolingHive }),
			verdict: VerdictDirection, rule: RuleReturnToContributor,
			textHas: []string{"the author's hive has picked the rework up"},
		},
		{
			name:    "owner PR, offline, conflicting → never adopted (return to author)",
			in:      with(incident, func(i *Input) { i.OwnerAuthor = true; i.Author = "clubanderson"; i.AuthorTooling = AuthorToolingPerson }),
			verdict: VerdictDirection, rule: RuleReturnToAuthor, textHas: []string{"@clubanderson"}, noLadder: true,
		},
		{
			name:    "hive PR is not the ladder's",
			in:      with(incident, func(i *Input) { i.HiveAuthored = true; i.TrustedAuthor = true }),
			verdict: VerdictDirection, rule: "fix-then-merge", noLadder: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Run(tc.in)
			if d.Verdict != tc.verdict || d.Rule != tc.rule {
				t.Fatalf("verdict/rule = %s/%s, want %s/%s (%+v)", d.Verdict, d.Rule, tc.verdict, tc.rule, d)
			}
			if tc.kinds != nil {
				var got []ActionKind
				for _, a := range d.Actions {
					got = append(got, a.Kind)
				}
				if strings.Join(kindsOf(got), ",") != strings.Join(kindsOf(tc.kinds), ",") {
					t.Fatalf("actions = %v, want %v", got, tc.kinds)
				}
			}
			if tc.target > 0 {
				found := false
				for _, a := range d.Actions {
					if a.Target == tc.target {
						found = true
					}
				}
				if !found {
					t.Fatalf("no action targets #%d: %+v", tc.target, d.Actions)
				}
			}
			for _, w := range tc.textHas {
				if !strings.Contains(d.Text, w) {
					t.Errorf("text %q lacks %q", d.Text, w)
				}
			}
			for _, a := range d.Actions {
				if a.Kind == ActionAdoptSupersede && tc.in.PersonAuthored() {
					t.Fatalf("a person's PR was adopted: %+v", d)
				}
			}
			if tc.noLadder {
				if d.Ladder != nil {
					t.Fatalf("ladder set on a non-ladder decision: %+v", d.Ladder)
				}
				return
			}
			if d.Ladder == nil || d.Ladder.Decision != tc.rule || d.Ladder.Reason == "" {
				t.Fatalf("ladder = %+v", d.Ladder)
			}
			joined := strings.Join(d.Ladder.RuledOut, "\n")
			for _, w := range tc.ruledOutHas {
				if !strings.Contains(joined, w) {
					t.Errorf("ruled_out lacks %q:\n%s", w, joined)
				}
			}
		})
	}
}

func kindsOf(k []ActionKind) []string {
	out := make([]string, 0, len(k))
	for _, x := range k {
		out = append(out, string(x))
	}
	return out
}

// The decision names the author's tooling truthfully: a clanker relay is
// not a hive (hivecommons/hive#15289: #15219's decision said "the author's
// hive" for a clanker-relayed author).
func TestAuthorWording(t *testing.T) {
	cases := []struct {
		tooling AuthorTooling
		word    string
		offline string
	}{
		{AuthorToolingClanker, "the author's clanker", "the author's clanker hasn't been connected since the nudge"},
		{AuthorToolingHive, "the author's hive", "the author's hive hasn't been connected since the nudge"},
		{AuthorToolingPerson, "the author", "the author hasn't been reachable since the nudge"},
		{AuthorToolingUnknown, "the author", "the author hasn't been reachable since the nudge"},
	}
	for _, tc := range cases {
		in := Input{AuthorTooling: tc.tooling}
		if got := in.AuthorWord(); got != tc.word {
			t.Errorf("%q: AuthorWord = %q, want %q", tc.tooling, got, tc.word)
		}
		if got := in.OfflineWording(); got != tc.offline {
			t.Errorf("%q: OfflineWording = %q, want %q", tc.tooling, got, tc.offline)
		}
	}
	d := Run(Input{Kind: KindPR, Number: 15219, Conflicting: true, AuthorTooling: AuthorToolingClanker})
	if d.Rule != RuleContributorOffline || !strings.Contains(d.Rationale, "the author's clanker hasn't been connected") || strings.Contains(d.Rationale, "author's hive") {
		t.Fatalf("offline rationale must name the clanker: %+v", d)
	}
	if got := AuthorToolingFromLabels([]string{"author/clanker"}, "author/hive", "author/clanker", "author/human", false); got != AuthorToolingClanker {
		t.Errorf("labels → %q", got)
	}
	if got := AuthorToolingFromLabels([]string{"author/clanker", "author/hive"}, "author/hive", "author/clanker", "author/human", false); got != AuthorToolingHive {
		t.Errorf("hive beats clanker: %q", got)
	}
	if got := AuthorToolingFromLabels([]string{"author/clanker"}, "author/hive", "author/clanker", "author/human", true); got != AuthorToolingPerson {
		t.Errorf("owner is a person: %q", got)
	}
	if Plain(RuleContributorOffline) != "the author hasn't been reachable since the nudge" {
		t.Errorf("glossary plain words must not presume a hive: %q", Plain(RuleContributorOffline))
	}
}

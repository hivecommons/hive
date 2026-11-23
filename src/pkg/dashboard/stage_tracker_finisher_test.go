package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Each activity monitor row says who can finish it, from the status
// refresh's your-turn decision rather than the current step: a PR at review
// whose repo requires an approving review is the operator's from the start,
// an issue its PR covers waits elsewhere, and a row with no stored decision
// says nothing. A PR back with its author or a person's draft is neither the
// operator's nor Hive's. An issue's linked PR entry carries the PR's answer.
func TestStageTrackerSaysWhoFinishesEachRow(t *testing.T) {
	const approval = "Waiting on a person's review: an approving review is required by branch protection (0 given)"
	repo := FrontendRepo{Name: "x",
		ActionableIssues: []any{
			github.Issue{Repo: "x", Number: 1, Title: "parked"},
			github.Issue{Repo: "x", Number: 2, Title: "covered", LinkedPRs: []github.IssueLinkedPR{{Number: 9, State: "open"}}},
			github.Issue{Repo: "x", Number: 3, Title: "no decision"},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 8, Title: "hive's"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 9, Title: "needs approval"}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 10, Title: "returned", Author: "alice", Labels: []string{"stage/072-returned-to-contributor"}}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 11, Title: "person's draft", Author: "bob", Draft: true, Labels: []string{"author/human"}}},
			FrontendPR{PullRequest: github.PullRequest{Repo: "x", Number: 12, Title: "bot's red PR", Author: "mergeraptor[bot]", Labels: []string{"stage/070-fixing"}}},
		},
		Ownership: map[string]Ownership{
			ownershipKey(ownershipKindIssue, 1): {YourTurn: true, Verb: OwnershipVerbDecide, ReasonCode: OwnershipReasonParkedNeedsDecision, Reason: "Parked until a maintainer decides"},
			ownershipKey(ownershipKindIssue, 2): waitingElsewhere(),
			ownershipKey(ownershipKindPR, 8):    notYourTurn(),
			ownershipKey(ownershipKindPR, 9):    {YourTurn: true, Verb: OwnershipVerbReview, ReasonCode: OwnershipReasonReviewDecision, Reason: approval},
			ownershipKey(ownershipKindPR, 10):   notYourTurn(),
			ownershipKey(ownershipKindPR, 11):   notYourTurn(),
			ownershipKey(ownershipKindPR, 12):   notYourTurn(),
		},
	}
	tr := (&Server{}).buildStageTracker(&StatusPayload{Repos: []FrontendRepo{repo}}, nil, time.Now())

	type want struct{ finisher, verb, reason string }
	wants := map[string]want{
		"issue#1": {finisherPerson, OwnershipVerbDecide, "Parked until a maintainer decides"},
		"issue#2": {finisherElsewhere, "", ""},
		"issue#3": {"", "", ""},
		"pr#8":    {finisherHive, "", ""},
		"pr#9":    {finisherPerson, OwnershipVerbReview, approval},
		// Not the operator's turn, but not Hive's either: the row's own step
		// or a person's draft names the author.
		"pr#10": {finisherElsewhere, "", "Returned to @alice — @alice must act"},
		"pr#11": {finisherElsewhere, "", "Draft — its author must mark it ready for review"},
		// Another bot's PR at 070: no hive lane fixes it (projectbluefin/utah#1490).
		"pr#12": {finisherElsewhere, "", "Bot-authored PR (@mergeraptor[bot]): the hive does not fix bot PRs; it waits for the bot's next run to replace it"},
	}
	seen := 0
	for _, it := range tr.Items {
		key := ownershipKey(it.Kind, it.Number)
		w, ok := wants[key]
		if !ok {
			continue
		}
		seen++
		if got := (want{it.Finisher, it.FinisherVerb, it.FinisherReason}); got != w {
			t.Errorf("%s: finisher = %+v, want %+v", key, got, w)
		}
		if key == "issue#2" {
			if len(it.LinkedPRs) != 1 || it.LinkedPRs[0].Number != 9 {
				t.Fatalf("issue#2 linked PRs = %+v, want #9", it.LinkedPRs)
			}
			lp := it.LinkedPRs[0]
			if got := (want{lp.Finisher, lp.FinisherVerb, lp.FinisherReason}); got != wants["pr#9"] {
				t.Errorf("issue#2's linked PR #9: finisher = %+v, want the PR's own %+v", got, wants["pr#9"])
			}
		}
	}
	if seen != len(wants) {
		t.Fatalf("saw %d of %d rows: %+v", seen, len(wants), tr.Items)
	}
}

// A row ownership leaves to Hive is not always one a hive lane finishes: a
// foreign bot's PR at 070 is never pushed to by the fixer (its caption says
// so; projectbluefin/utah#1490 and #1497, mergeraptor[bot], read finisher
// "hive"), and a PR held at 050 / 060 because its base branch or a shared CI
// incident is red waits on that recovery. Your-turn and waiting decisions,
// the hive's own 070 and an ordinary red 060 keep their answer.
func TestStageTrackerFinisherForRowsNoHiveLaneFinishes(t *testing.T) {
	const bot = "Bot-authored PR (@mergeraptor[bot]): the hive does not fix bot PRs; it waits for the bot's next run to replace it"
	const baseRed = "Red because the base branch is red (E2E, lint); it moves on once the base branch's CI recovers"
	cases := []struct {
		name               string
		it                 StageTrackerItem
		decision           Ownership
		draft              bool
		finisher, reasonIs string
	}{
		{name: "bot PR at 070", it: StageTrackerItem{Author: "mergeraptor[bot]", Stage: trackerNumberFixing}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: bot},
		{name: "bot PR at 070, your turn", it: StageTrackerItem{Author: "mergeraptor[bot]", Stage: trackerNumberFixing}, decision: Ownership{YourTurn: true, Verb: OwnershipVerbReview, Reason: "Needs human review"}, finisher: finisherPerson, reasonIs: "Needs human review"},
		{name: "bot PR at 070, waiting elsewhere", it: StageTrackerItem{Author: "mergeraptor[bot]", Stage: trackerNumberFixing}, decision: waitingElsewhereFor("held"), finisher: finisherElsewhere, reasonIs: "held"},
		{name: "hive's own PR at 070", it: StageTrackerItem{Author: "hive-app[bot]", AuthorKind: authorKindHive, Stage: trackerNumberFixing}, decision: notYourTurn(), finisher: finisherHive},
		{name: "bot PR at 050", it: StageTrackerItem{Author: "mergeraptor[bot]", Stage: trackerNumberReviewPending}, decision: notYourTurn(), finisher: finisherHive},
		{name: "base red at 050", it: StageTrackerItem{Stage: trackerNumberReviewPending, Reason: "base-branch-red:E2E,lint"}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: baseRed},
		{name: "base red at 060, approved by a person", it: StageTrackerItem{Stage: trackerNumberReviewed, Reason: "base-branch-red:E2E,lint" + stagelabels.HumanApprovedReasonSuffix + "alice"}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: baseRed},
		{name: "shared CI incident at 060", it: StageTrackerItem{Stage: trackerNumberReviewed, Reason: "shared-ci-incident:42"}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: "Red because of shared-CI incident #42; it moves on once the incident closes"},
		{name: "base red with no checks named", it: StageTrackerItem{Stage: trackerNumberReviewPending, Reason: "base-branch-red:"}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: "Red because the base branch is red; it moves on once the base branch's CI recovers"},
		{name: "shared CI incident with no number", it: StageTrackerItem{Stage: trackerNumberReviewed, Reason: "shared-ci-incident:"}, decision: notYourTurn(), finisher: finisherElsewhere, reasonIs: "Red because of a shared-CI incident; it moves on once the incident closes"},
		{name: "base red at 070 is not held", it: StageTrackerItem{Stage: trackerNumberFixing, Reason: "base-branch-red:E2E"}, decision: notYourTurn(), finisher: finisherHive},
		{name: "own red check at 060", it: StageTrackerItem{Stage: trackerNumberReviewed, Reason: "required-check-failing:build"}, decision: notYourTurn(), finisher: finisherHive},
		{name: "person's draft at 050, base red", it: StageTrackerItem{Author: "bob", AuthorKind: authorKindHuman, Stage: trackerNumberReviewPending, Reason: "base-branch-red:E2E"}, decision: notYourTurn(), draft: true, finisher: finisherElsewhere, reasonIs: "Draft — its author must mark it ready for review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := tc.it
			it.Kind, it.Repo, it.Number = "pr", "projectbluefin/utah", 1490
			stageTrackerSetFinisher(&it, map[string]Ownership{ownershipKey(ownershipKindPR, 1490): tc.decision}, tc.draft)
			if it.Finisher != tc.finisher || it.FinisherReason != tc.reasonIs {
				t.Fatalf("finisher = %q / %q, want %q / %q", it.Finisher, it.FinisherReason, tc.finisher, tc.reasonIs)
			}
		})
	}
}

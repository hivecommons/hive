package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// answeredEnv is an ownership env whose decision ledger holds reqs.
func answeredEnv(t *testing.T, verdicts []review.Aggregate, dispatch review.DispatchState, reqs ...review.HumanChangeRequest) ownershipEnv {
	t.Helper()
	store := review.NewHumanChangeRequests("")
	for _, r := range reqs {
		if err := store.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	env := ownershipEnv11565(6)
	env.reviewVerdicts = verdicts
	env.reviewSwarm.dispatch = dispatch
	env.reviewSwarm.answer = func(repo string, number int, head string) (review.HumanChangeRequest, bool) {
		return review.DecisionAnswerFor(store, repo, number, head)
	}
	return env
}

func decisionAnswer(pr github.PullRequest, head, kind string, at time.Time) review.HumanChangeRequest {
	return review.HumanChangeRequest{Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision,
		SourceID: 6657994532, Actor: "Danathar", Body: "Fix the symlink case before merge.", Kind: kind, RecordedAt: at}
}

// Danathar/dns-failover#155: the swarm returned requires_human on 8bb4580,
// the owner answered `/hive decision <instruction>` and the hive moved the PR
// to 070-fixing, yet the row stayed "Hive's review needs a human decision".
// An answered head waits on the hive until a new head or a new question.
func TestAnsweredRequiresHumanLeavesYourTurn(t *testing.T) {
	pr := withHead12329(hivePR11565("stage/070-fixing"), "head-2")
	asked := verdict12377(pr, "head-2", review.VerdictRequiresHuman) // recorded now-2h
	answered := ownershipNow11565.Add(-time.Hour)
	fixCap := dispatchHold12377(pr, "head-2", "review fix cap reached (6/6)")
	rejected := verdict12377(pr, "head-2", review.VerdictReject)
	capped := verdict12377(pr, "head-2", review.VerdictChangesRequested)
	capped.FixAttempts, capped.MaxFixAttempts = 3, 3
	askedAgain := verdict12377(pr, "head-2", review.VerdictRequiresHuman)
	askedAgain.RecordedAt = ownershipNow11565.Add(-30 * time.Minute)

	for _, tc := range []struct {
		name     string
		verdicts []review.Aggregate
		dispatch review.DispatchState
		reqs     []review.HumanChangeRequest
		code     string // "" = waiting on the hive
	}{
		{name: "instruction on the current head", verdicts: []review.Aggregate{asked},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)}},
		{name: "a changes-requested review answers it too", verdicts: []review.Aggregate{asked},
			reqs: []review.HumanChangeRequest{func() review.HumanChangeRequest {
				r := decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)
				r.Source = review.HumanChangeRequestSourceReview
				return r
			}()}},
		{name: "a rejected head answered", verdicts: []review.Aggregate{rejected},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)}},
		{name: "no answer", verdicts: []review.Aggregate{asked}, code: OwnershipReasonReviewRequiresHuman},
		{name: "answer on another head", verdicts: []review.Aggregate{asked},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-1", review.HumanChangeRequestKindChanges, answered)}, code: OwnershipReasonReviewRequiresHuman},
		{name: "a bare approval is left to the release rules", verdicts: []review.Aggregate{asked},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindApprove, answered)}, code: OwnershipReasonReviewRequiresHuman},
		{name: "the swarm asked again after the answer", verdicts: []review.Aggregate{askedAgain},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)}, code: OwnershipReasonReviewRequiresHuman},
		{name: "a dispatcher hold is still the owner's", verdicts: []review.Aggregate{asked}, dispatch: fixCap,
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)}, code: OwnershipReasonReviewDispatchHold},
		{name: "the loop cap is still the owner's", verdicts: []review.Aggregate{capped},
			reqs: []review.HumanChangeRequest{decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, answered)}, code: OwnershipReasonReviewLoopCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decidePROwnership(pr, false, answeredEnv(t, tc.verdicts, tc.dispatch, tc.reqs...))
			if tc.code != "" {
				checkOwnership11565(t, got, want11565{OwnershipVerbReview, tc.code})
				return
			}
			if got.YourTurn || !got.WaitingElsewhere || !strings.Contains(got.Reason, "@Danathar answered Hive's review on this head") || !strings.Contains(got.Reason, "the hive is fixing it") {
				t.Fatalf("got %+v, want waiting on the hive's fix", got)
			}
		})
	}
}

// Snapshot rows can carry the bare repository name ("dns-failover" in
// /api/overview/prs.json); the ledger records owner/repo. The env built from
// configuration reads it under the project org.
func TestAnsweredReviewHoldLookupUsesProjectOrg(t *testing.T) {
	store := review.NewHumanChangeRequests("")
	pr := github.PullRequest{Repo: "Danathar/dns-failover", Number: 155}
	if err := store.Record(decisionAnswer(pr, "8bb4580", review.HumanChangeRequestKindChanges, ownershipNow11565)); err != nil {
		t.Fatal(err)
	}
	prev := ownershipDecisionAnswers
	ownershipDecisionAnswers = func() review.HumanChangeRequestStore { return store }
	t.Cleanup(func() { ownershipDecisionAnswers = prev })

	cfg := &config.Config{}
	cfg.Project.Org = "Danathar"
	env := newOwnershipEnv(cfg, nil, "", ownershipNow11565, nil)
	for _, repo := range []string{"dns-failover", "Danathar/dns-failover"} {
		if _, ok := env.reviewSwarm.answer(repo, 155, "8bb4580"); !ok {
			t.Fatalf("answer for %q not found", repo)
		}
	}
	if _, ok := env.reviewSwarm.answer("dns-failover", 155, "other-head"); ok {
		t.Fatal("an answer on another head must not count")
	}
}

// projectbluefin/testsuite#1037: the decision answered the review on
// 9c0ec3d for the hive's fixer, and the row read "the hive is fixing it" for
// days while every push was refused. An answer the hive cannot carry out on
// its branch stays with the person who gave it, with the reason.
func TestAnsweredReviewOnUnpushableAdoptedBranchStaysYourTurn(t *testing.T) {
	pr := withHead12329(hivePR11565("stage/074-adopted-by-hive"), "head-2")
	asked := verdict12377(pr, "head-2", review.VerdictRequiresHuman)
	answer := decisionAnswer(pr, "head-2", review.HumanChangeRequestKindChanges, ownershipNow11565.Add(-time.Hour))
	for _, tc := range []struct {
		name    string
		refusal string
	}{
		{name: "pushable: waits on the hive"},
		{name: "fork-rehome-refused: your turn", refusal: "the hive recorded that it cannot finish this fork PR on the fork's own branch (fork-rehome-refused)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := answeredEnv(t, []review.Aggregate{asked}, review.DispatchState{}, answer)
			env.adoptedPushRefused = func(github.PullRequest) string { return tc.refusal }
			got := decidePROwnership(pr, false, env)
			if tc.refusal == "" {
				if got.YourTurn || !strings.Contains(got.Reason, "the hive is fixing it") {
					t.Fatalf("got %+v, want waiting on the hive", got)
				}
				return
			}
			checkOwnership11565(t, got, want11565{OwnershipVerbDecide, OwnershipReasonAdoptedPushRefused})
			if strings.Contains(got.Reason, "the hive is fixing it") || !strings.Contains(got.Reason, "fork-rehome-refused") || !strings.Contains(got.Reason, "@Danathar answered") {
				t.Fatalf("reason %q", got.Reason)
			}
		})
	}
}

package dashboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// hivecommons/hive#12377: every review-swarm reason /api/review/pipeline puts
// a PR in human_hold for is your turn, for any author; #12374: a
// requires_human verdict on an earlier head still holds after a fix push.

type reviewHold12377 struct {
	verdicts []review.Aggregate
	dispatch review.DispatchState
}

func dispatchHold12377(pr github.PullRequest, head, reason string) review.DispatchState {
	return review.DispatchState{Human: []review.HumanReviewHold{{Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Reason: reason, UpdatedAt: ownershipNow11565.Add(-time.Hour)}}}
}

func verdict12377(pr github.PullRequest, head string, v review.Verdict) review.Aggregate {
	a := review.Aggregate{Repo: pr.Repo, Number: pr.Number, HeadSHA: head, Verdict: v, RecordedAt: ownershipNow11565.Add(-2 * time.Hour),
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveSecurity: v, review.PerspectiveCorrectness: review.VerdictApprove}}
	switch v {
	case review.VerdictRequiresHuman:
		a.RequiresHuman = true
	case review.VerdictApprove:
		a.MergeEligible = true
	}
	return a
}

// pipelineHumanHold12377 places the PR on the review pipeline from the same
// records the decision read, as /api/review/pipeline does.
func pipelineHumanHold12377(pr github.PullRequest, h reviewHold12377) bool {
	card := pipeline.Derive(pipeline.Inputs{
		PR:       github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Labels: pr.Labels, HiveAuthored: prAgentRole(pr) != ""},
		Verdicts: h.verdicts,
		Dispatch: h.dispatch,
	})
	return card.Stage == pipeline.StageHumanHold
}

func TestReviewHoldsAreYourTurnForEveryAuthor12377(t *testing.T) {
	authors := map[string]github.PullRequest{
		"hive":   withHead12329(hivePR11565(), "head-2"),
		"person": withHead12329(personPR11565(), "head-2"),
	}
	type holdCase struct {
		name string
		hold func(pr github.PullRequest) reviewHold12377
		code string // "" means not your turn and not human_hold
	}
	cases := []holdCase{
		{"requires_human on the current head", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{verdicts: []review.Aggregate{verdict12377(pr, "head-2", review.VerdictRequiresHuman)}}
		}, OwnershipReasonReviewRequiresHuman},
		{"reject on the current head", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{verdicts: []review.Aggregate{verdict12377(pr, "head-2", review.VerdictReject)}}
		}, OwnershipReasonReviewRejected},
		{"requires_human on an earlier head, fix pushed since (#12374)", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{verdicts: []review.Aggregate{verdict12377(pr, "head-1", review.VerdictRequiresHuman)}}
		}, OwnershipReasonReviewRequiresHuman},
		{"fresh approve on the new head clears an earlier requires_human", func(pr github.PullRequest) reviewHold12377 {
			approve := verdict12377(pr, "head-2", review.VerdictApprove)
			approve.RecordedAt = ownershipNow11565.Add(-time.Hour)
			return reviewHold12377{verdicts: []review.Aggregate{verdict12377(pr, "head-1", review.VerdictRequiresHuman), approve}}
		}, ""},
		{"review dispatch exhausted", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{dispatch: dispatchHold12377(pr, "head-2", review.HoldReasonDispatchExhausted+": 3 review dispatches for this head ended without a verdict")}
		}, OwnershipReasonReviewDispatchHold},
		{"review fix cap reached", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{dispatch: dispatchHold12377(pr, "head-2", "review fix cap reached (6/6)")}
		}, OwnershipReasonReviewDispatchHold},
		{"no fixer agent", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{dispatch: dispatchHold12377(pr, "head-2", "no enabled kick-capable fixer agent is available")}
		}, OwnershipReasonReviewDispatchHold},
		{"dispatch hold on an older head", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{dispatch: dispatchHold12377(pr, "head-1", review.HoldReasonDispatchExhausted+": 3 review dispatches for this head ended without a verdict")}
		}, ""},
		{"loop cap while changes_requested", func(pr github.PullRequest) reviewHold12377 {
			capped := verdict12377(pr, "head-2", review.VerdictChangesRequested)
			capped.FixAttempts, capped.MaxFixAttempts = 3, 3
			return reviewHold12377{verdicts: []review.Aggregate{capped}}
		}, OwnershipReasonReviewLoopCap},
		{"loop cap while fixing", func(pr github.PullRequest) reviewHold12377 {
			return reviewHold12377{dispatch: review.DispatchState{Fixes: []review.PendingFix{{Repo: pr.Repo, Number: pr.Number, HeadSHA: "head-2", Agent: "fixer", Attempts: review.DefaultFixCycleCap, Dispatched: ownershipNow11565.Add(-time.Hour)}}}}
		}, OwnershipReasonReviewLoopCap},
	}
	for author, pr := range authors {
		for _, tc := range cases {
			h := tc.hold(pr)
			held := pipelineHumanHold12377(pr, h)
			if held != (tc.code != "") {
				t.Fatalf("%s/%s: pipeline human_hold = %v, want %v", author, tc.name, held, tc.code != "")
			}
			for level := 1; level <= 6; level++ {
				env := ownershipEnv11565(level)
				env.reviewVerdicts = h.verdicts
				env.reviewSwarm.dispatch = h.dispatch
				got := decidePROwnership(pr, false, env)
				if got.YourTurn != held {
					t.Fatalf("%s/%s L%d: your turn = %v but pipeline human_hold = %v (%+v)", author, tc.name, level, got.YourTurn, held, got)
				}
				if tc.code == "" {
					continue
				}
				checkOwnership11565(t, got, want11565{OwnershipVerbReview, tc.code})
			}
		}
	}
}

// A Hive PR inside the adjudicator's needs-human wait waits for the
// adjudicator first; once the wait elapses the PR is your turn either way.
func TestReviewHoldWaitsForTheAdjudicator12377(t *testing.T) {
	pr := withHead12329(hivePR11565("needs-human"), "head-2")
	h := reviewHold12377{dispatch: dispatchHold12377(pr, "head-2", "review fix cap reached (6/6)")}
	env := ownershipEnv11565(6)
	env.reviewSwarm.dispatch = h.dispatch
	got := decidePROwnership(pr, false, env)
	if got.YourTurn || !got.WaitingElsewhere {
		t.Fatalf("within the adjudicator wait = %+v, want waiting elsewhere", got)
	}
	env.escalatedAt = func(string, int) time.Time { return ownershipNow11565.Add(-ownershipAdjudicatorWait) }
	checkOwnership11565(t, decidePROwnership(pr, false, env), want11565{OwnershipVerbReview, OwnershipReasonAdjudicatorWaitElapsed})
}

// A person's hold still hides a review-held PR, and the sentinel still wins.
func TestReviewHoldKeepsEarlierRules12377(t *testing.T) {
	held := withHead12329(personPR11565("hold"), "head-2")
	held.HoldSources = []string{github.HoldSourcePerson}
	flagged := withHead12329(hivePR11565(config.DefaultSentinelLabel), "head-2")
	for _, pr := range []github.PullRequest{held, flagged} {
		isHeld := len(pr.HoldSources) > 0
		without := ownershipEnv11565(6)
		with := ownershipEnv11565(6)
		with.reviewSwarm.dispatch = dispatchHold12377(pr, "head-2", "no enabled kick-capable fixer agent is available")
		if got, want := decidePROwnership(pr, isHeld, with), decidePROwnership(pr, isHeld, without); got != want {
			t.Fatalf("pr %v: with a dispatch hold %+v, without %+v; want unchanged", pr.Labels, got, want)
		}
	}
}

// hivecommons/hive#12962: a person's approving review or `/hive approve`
// after a requires_human verdict, as recorded by the self-merge sweep,
// releases the your-turn row and the pipeline's human_hold together. A
// release from before the verdict, or a bot's, still holds.
func TestReviewHoldReleasedByAPerson12962(t *testing.T) {
	pr := withHead12329(hivePR11565(), "head-2")
	v := verdict12377(pr, "head-2", review.VerdictRequiresHuman)
	after := v.RecordedAt.Add(30 * time.Minute)
	before := v.RecordedAt.Add(-30 * time.Minute)
	verdictDispatch := dispatchHold12377(pr, "head-2", "review verdict requires_human")
	for _, tc := range []struct {
		name     string
		release  github.ReviewHoldRelease
		wantHeld bool
	}{
		{"approving review after the verdict", github.ReviewHoldRelease{Via: github.ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: after}, false},
		{"/hive approve after the verdict", github.ReviewHoldRelease{Via: github.ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: after}, false},
		{"/hive approve before the verdict", github.ReviewHoldRelease{Via: github.ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: before}, true},
		{"a bot's approval", github.ReviewHoldRelease{Via: github.ReviewHoldReleaseViaReview, State: "APPROVED", By: "hive[bot]", At: after}, true},
	} {
		release := tc.release
		card := pipeline.Derive(pipeline.Inputs{
			PR:          github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, HiveAuthored: true},
			Verdicts:    []review.Aggregate{v},
			Dispatch:    verdictDispatch,
			HoldRelease: &release,
		})
		if held := card.Stage == pipeline.StageHumanHold; held != tc.wantHeld {
			t.Fatalf("%s: pipeline human_hold = %v, want %v (%+v)", tc.name, held, tc.wantHeld, card)
		}
		env := ownershipEnv11565(6)
		env.reviewVerdicts = []review.Aggregate{v}
		env.reviewSwarm.dispatch = verdictDispatch
		path := filepath.Join(t.TempDir(), github.ReviewHoldReleasesFile)
		if err := github.RecordReviewHoldRelease(path, pr.Repo, pr.Number, release); err != nil {
			t.Fatalf("%s: record: %v", tc.name, err)
		}
		releases, err := github.LoadReviewHoldReleases(path)
		if err != nil {
			t.Fatalf("%s: load: %v", tc.name, err)
		}
		env.reviewSwarm.releases = releases
		got := decidePROwnership(pr, false, env)
		if got.YourTurn != tc.wantHeld {
			t.Fatalf("%s: your turn = %v, want %v (%+v)", tc.name, got.YourTurn, tc.wantHeld, got)
		}
		if tc.wantHeld {
			checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonReviewRequiresHuman})
		}
	}
}

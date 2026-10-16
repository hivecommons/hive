package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// hivecommons/hive#12329: a PR not written by Hive whose current-head review
// verdict is requires_human is your turn (REVIEW, review_requires_human), as
// /api/review/pipeline already shows it in human_hold.

func requiresHumanVerdict12329(repo string, number int, head string) review.Aggregate {
	return review.Aggregate{Repo: repo, Number: number, HeadSHA: head, Verdict: review.VerdictRequiresHuman, RequiresHuman: true, RecordedAt: ownershipNow11565.Add(-30 * time.Minute)}
}

func withHead12329(pr github.PullRequest, head string) github.PullRequest {
	pr.HeadSHA = head
	return pr
}

// pipelineHumanHold12329 places the PR in the review pipeline from the same
// verdicts the decision read.
func pipelineHumanHold12329(pr github.PullRequest, verdicts []review.Aggregate) bool {
	card := pipeline.Derive(pipeline.Inputs{
		PR:       github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Labels: pr.Labels, HiveAuthored: prAgentRole(pr) != ""},
		Verdicts: verdicts,
	})
	return card.Stage == pipeline.StageHumanHold
}

func TestPersonPRReviewRequiresHumanIsYourTurn12329(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-1")
	verdicts := []review.Aggregate{requiresHumanVerdict12329("o/r", pr.Number, "head-1")}
	for level := 1; level <= 6; level++ {
		env := ownershipEnv11565(level)
		env.reviewVerdicts = verdicts
		got := decidePROwnership(pr, false, env)
		checkOwnership11565(t, got, want11565{OwnershipVerbReview, OwnershipReasonReviewRequiresHuman})
		if got.Reason != "Hive's review needs a human decision" {
			t.Fatalf("L%d reason = %q", level, got.Reason)
		}
	}
	if !pipelineHumanHold12329(pr, verdicts) {
		t.Fatal("review pipeline does not place the PR in human_hold; the two surfaces disagree")
	}

	// The aggregate's requires_human flag counts on its own, whatever the
	// verdict, as it does for the pipeline.
	flagged := requiresHumanVerdict12329("o/r", pr.Number, "head-1")
	flagged.Verdict = review.VerdictChangesRequested
	env := ownershipEnv11565(6)
	env.reviewVerdicts = []review.Aggregate{flagged}
	checkOwnership11565(t, decidePROwnership(pr, false, env), want11565{OwnershipVerbReview, OwnershipReasonReviewRequiresHuman})
}

func TestPersonPRReviewRequiresHumanClearedByNewHead12329(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-2")
	verdicts := []review.Aggregate{requiresHumanVerdict12329("o/r", pr.Number, "head-1")}
	for level := 1; level <= 6; level++ {
		env := ownershipEnv11565(level)
		env.reviewVerdicts = verdicts
		checkOwnership11565(t, decidePROwnership(pr, false, env), notYours11565)
	}
	if pipelineHumanHold12329(pr, verdicts) {
		t.Fatal("review pipeline holds a PR whose only requires_human verdict is on an older head")
	}
}

func TestReviewRequiresHumanDoesNotOverrideOtherRules12329(t *testing.T) {
	verdictsFor := func(pr github.PullRequest) []review.Aggregate {
		return []review.Aggregate{requiresHumanVerdict12329(pr.Repo, pr.Number, pr.HeadSHA)}
	}
	cases := []struct {
		name string
		pr   github.PullRequest
		held bool
	}{
		{"hive PR, no labels", withHead12329(hivePR11565(), "h"), false},
		{"hive PR with needs-human", withHead12329(hivePR11565("needs-human"), "h"), false},
		{"hive PR with needs-human and reviewer-passed", withHead12329(hivePR11565("needs-human", "reviewer-passed"), "h"), false},
		{"person PR with needs-human", withHead12329(personPR11565("needs-human"), "h"), false},
		{"person PR flagged by the sentinel", withHead12329(personPR11565(config.DefaultSentinelLabel), "h"), false},
		{"person PR held by a person", func() github.PullRequest {
			pr := withHead12329(personPR11565("hold"), "h")
			pr.HoldSources = []string{github.HoldSourcePerson}
			return pr
		}(), true},
	}
	for _, tc := range cases {
		for level := 1; level <= 6; level++ {
			without := ownershipEnv11565(level)
			with := ownershipEnv11565(level)
			with.reviewVerdicts = verdictsFor(tc.pr)
			if got, want := decidePROwnership(tc.pr, tc.held, with), decidePROwnership(tc.pr, tc.held, without); got != want {
				t.Fatalf("%s L%d: with a requires_human verdict %+v, without %+v; want unchanged", tc.name, level, got, want)
			}
		}
	}
}

func TestReviewRequiresHumanReachesTheInbox12329(t *testing.T) {
	pr := withHead12329(personPR11565(), "head-1")
	other := withHead12329(personPR11565(), "head-9")
	other.Number = 4
	repos := []FrontendRepo{{Name: "r", Full: "o/r", OpenPrs: []any{FrontendPR{PullRequest: pr}, FrontendPR{PullRequest: other}}}}
	env := ownershipEnv11565(6)
	env.reviewVerdicts = []review.Aggregate{
		requiresHumanVerdict12329("o/r", pr.Number, "head-1"),
		requiresHumanVerdict12329("o/r", other.Number, "head-8"),
	}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	got := repos[0].Ownership[ownershipKey(ownershipKindPR, pr.Number)]
	if !got.YourTurn || got.ReasonCode != OwnershipReasonReviewRequiresHuman || got.InboxRank != 1 {
		t.Fatalf("pr#%d = %+v, want your turn review_requires_human at inbox rank 1", pr.Number, got)
	}
	if stale := repos[0].Ownership[ownershipKey(ownershipKindPR, other.Number)]; stale.YourTurn || stale.InboxRank != 0 {
		t.Fatalf("pr#%d with a verdict on an older head = %+v, want not your turn", other.Number, stale)
	}
}

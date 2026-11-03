package review

import (
	"strings"
	"testing"
	"time"
)

// Observed live on hive's own spoke (hivecommons/hive#12045 head 7e027fc):
// correctness, security, intent-alignment and style all approved, and the
// docs-currency reviewer kick died before it reported. The aggregate came
// back requires_human with "review perspectives did not unanimously approve"
// — but they did; the set was just short. The dispatcher then treated the
// head as settled, parked it on a human hold, and never asked for
// docs-currency again, so the PR could never self-merge.
func TestPartialApprovalIsIncompleteNotRequiresHuman(t *testing.T) {
	tests := []struct {
		name        string
		perspective []Perspective
		opts        AggregateOptions
		want        Verdict
		merge       bool
		wantReason  string
	}{
		{
			name:        "4 of 5 approve is incomplete",
			perspective: []Perspective{PerspectiveCorrectness, PerspectiveSecurity, PerspectiveIntentAlignment, PerspectiveStyle},
			want:        VerdictIncomplete,
			wantReason:  "review coverage incomplete: missing docs-currency",
		},
		{
			name:        "5 of 5 approve is approve",
			perspective: DefaultPerspectives,
			want:        VerdictApprove,
			merge:       true,
		},
		{
			name:        "2 of 5 approve names every missing perspective",
			perspective: []Perspective{PerspectiveSecurity, PerspectiveStyle},
			want:        VerdictIncomplete,
			wantReason:  "review coverage incomplete: missing correctness, intent-alignment, docs-currency",
		},
		{
			name:        "capped hive short of its cap says how many more",
			perspective: []Perspective{PerspectiveCorrectness},
			opts:        AggregateOptions{MaxPerspectivesPerPR: 2},
			want:        VerdictIncomplete,
			wantReason:  "review coverage incomplete: 1 more of security, intent-alignment, style, docs-currency",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reports []PerspectiveReport
			for _, p := range tt.perspective {
				reports = append(reports, approveReport(p))
			}
			got := AggregateReports(reports, tt.opts)
			if got.Verdict != tt.want {
				t.Fatalf("verdict = %s, want %s (reasons %v)", got.Verdict, tt.want, got.Reasons)
			}
			if got.MergeEligible != tt.merge {
				t.Fatalf("merge_eligible = %v, want %v", got.MergeEligible, tt.merge)
			}
			if got.RequiresHuman {
				t.Fatalf("an all-approve aggregate must never require a human: %+v", got)
			}
			if tt.wantReason == "" {
				if len(got.Reasons) != 0 {
					t.Fatalf("approve carried reasons: %v", got.Reasons)
				}
				return
			}
			if len(got.Reasons) != 1 || got.Reasons[0] != tt.wantReason {
				t.Fatalf("reasons = %v, want [%q]", got.Reasons, tt.wantReason)
			}
		})
	}
}

// A perspective report may say approve, changes_requested, requires_human
// or reject. "incomplete" is what the aggregate says about the set, and a
// reviewer must not be able to claim it.
func TestIncompleteIsNotAValidReportVerdict(t *testing.T) {
	if validVerdict(VerdictIncomplete) {
		t.Fatal("incomplete must be aggregate-only, not a report verdict")
	}
}

func incompleteAggregate(delivered ...Perspective) Aggregate {
	var reports []PerspectiveReport
	for _, p := range delivered {
		r := approveReport(p)
		r.Repo, r.Number, r.HeadSHA = "acme/hive", 7, "sha1"
		reports = append(reports, r)
	}
	return AggregateReports(reports, AggregateOptions{})
}

// The planner must treat an incomplete aggregate as a head still under
// review: no human hold, and a kick for exactly the perspectives that have
// not reported — never a re-ask of the ones whose reports already exist.
func TestPlanDispatchIncompleteAggregateDispatchesOnlyMissing(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	pr := dispatchPR("sha1")
	agg := incompleteAggregate(PerspectiveCorrectness, PerspectiveSecurity, PerspectiveIntentAlignment, PerspectiveStyle)
	if agg.Verdict != VerdictIncomplete {
		t.Fatalf("fixture aggregate = %s, want incomplete", agg.Verdict)
	}
	opts := DispatchOptions{RequireApproval: true, FanOut: true, ProjectOrg: "acme", Now: now,
		MaxParallelReviews: 5, Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}}

	t.Run("delivered perspectives drop off pending, missing one is kicked once its TTL expires", func(t *testing.T) {
		// All five were dispatched 2h ago; four reported, docs-currency never did.
		var pending []PendingReview
		for _, p := range DefaultPerspectives {
			pending = append(pending, PendingReview{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: p, Agent: "r1", Dispatched: now.Add(-2 * time.Hour)})
		}
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{Pending: pending}, opts)
		if len(plan.State.Human) != 0 {
			t.Fatalf("incomplete aggregate produced a human hold: %+v", plan.State.Human)
		}
		if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveDocsCurrency {
			t.Fatalf("kicks = %+v, want exactly one for docs-currency", plan.ReviewKicks)
		}
		if len(plan.State.Pending) != 1 || plan.State.Pending[0].Perspective != PerspectiveDocsCurrency || !plan.State.Pending[0].Dispatched.Equal(now) {
			t.Fatalf("pending = %+v, want only a fresh docs-currency entry", plan.State.Pending)
		}
		if len(plan.Expired) != 1 || plan.Expired[0].Perspective != PerspectiveDocsCurrency {
			t.Fatalf("expired = %+v, want only the undelivered perspective counted", plan.Expired)
		}
	})

	t.Run("missing perspective still in flight is not re-asked", func(t *testing.T) {
		pending := []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveDocsCurrency, Agent: "r1", Dispatched: now.Add(-time.Minute)}}
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{Pending: pending}, opts)
		if len(plan.ReviewKicks) != 0 || len(plan.State.Human) != 0 {
			t.Fatalf("re-kicked a perspective that is still pending: kicks=%+v human=%+v", plan.ReviewKicks, plan.State.Human)
		}
	})

	t.Run("no pending record at all asks only for what is missing", func(t *testing.T) {
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, opts)
		if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].Perspective != PerspectiveDocsCurrency {
			t.Fatalf("kicks = %+v, want exactly one for docs-currency", plan.ReviewKicks)
		}
		if len(plan.State.Human) != 0 {
			t.Fatalf("incomplete aggregate produced a human hold: %+v", plan.State.Human)
		}
	})

	t.Run("combined review asks only for the missing perspective", func(t *testing.T) {
		combined := opts
		combined.CombinedPerspectives = true
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, DispatchState{}, combined)
		if len(plan.ReviewKicks) != 1 {
			t.Fatalf("kicks = %+v, want one combined kick", plan.ReviewKicks)
		}
		if got := plan.ReviewKicks[0].Perspectives; len(got) != 1 || got[0] != PerspectiveDocsCurrency {
			t.Fatalf("combined kick perspectives = %v, want [docs-currency]", got)
		}
	})

	t.Run("max_redispatch still bounds the re-ask", func(t *testing.T) {
		exhausted := DispatchState{Human: []HumanReviewHold{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Reason: HoldReasonDispatchExhausted + ": 3 review dispatches for this head ended without a verdict"}}}
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{agg}}, exhausted, opts)
		if len(plan.ReviewKicks) != 0 {
			t.Fatalf("exhausted head was re-kicked: %+v", plan.ReviewKicks)
		}
		if len(plan.State.Human) != 1 || !strings.HasPrefix(plan.State.Human[0].Reason, HoldReasonDispatchExhausted) {
			t.Fatalf("exhaustion hold was not preserved: %+v", plan.State.Human)
		}
	})
}

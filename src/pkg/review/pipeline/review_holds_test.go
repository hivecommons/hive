package pipeline

import (
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func reviewedAgg(verdict review.Verdict, head string, at int) review.Aggregate {
	a := agg(verdict, head, t0.Add(time.Duration(at)*time.Hour))
	a.Perspectives = map[review.Perspective]review.Verdict{"security": verdict, "correctness": review.VerdictApprove}
	switch verdict {
	case review.VerdictRequiresHuman:
		a.RequiresHuman = true
	case review.VerdictApprove:
		a.MergeEligible = true
	}
	return a
}

// A requires_human verdict followed by a fix push keeps the PR in human_hold
// until an approving verdict lands (#12374): the self-merge sweep holds it the
// same way, so the board shows what blocks the merge.
func TestDerive_CarriedVerdictHold(t *testing.T) {
	in := Inputs{PR: basePR(), Verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old-head-sha", 0)}}
	card := Derive(in)
	if card.Stage != StageHumanHold || card.NextAction.Kind != ActionHuman {
		t.Fatalf("stage = %s, want human_hold: %+v", card.Stage, card)
	}
	if len(card.Reasons) == 0 || !strings.Contains(card.Reasons[0], "earlier head (old-hea)") {
		t.Fatalf("reasons = %v, want the carried verdict named", card.Reasons)
	}

	// changes_requested on the new head neither holds nor releases.
	in.Verdicts = append(in.Verdicts, reviewedAgg(review.VerdictChangesRequested, "head", 1))
	if card := Derive(in); card.Stage != StageHumanHold {
		t.Fatalf("changes_requested on the new head released the hold: %s", card.Stage)
	}

	// A fresh merge-eligible approve releases it.
	in.Verdicts = append(in.Verdicts, reviewedAgg(review.VerdictApprove, "head", 2))
	if card := Derive(in); card.Stage != StageApproved {
		t.Fatalf("approve on the new head: stage = %s, want approved", card.Stage)
	}
}

func TestReviewHolds(t *testing.T) {
	capped := agg(review.VerdictChangesRequested, "head", t1)
	capped.FixAttempts, capped.MaxFixAttempts = 3, 3
	cases := []struct {
		name string
		in   Inputs
		want []string
	}{
		{name: "no evidence", in: Inputs{PR: basePR()}},
		{name: "label-only hold is the caller's to decide", in: Inputs{PR: withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"needs-human"}; p.Held = true })}},
		{name: "reject", in: Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictReject, "head", t1)}}, want: []string{ReviewHoldReject}},
		{name: "requires_human", in: Inputs{PR: basePR(), Verdicts: []review.Aggregate{agg(review.VerdictRequiresHuman, "head", t1)}}, want: []string{ReviewHoldRequiresHuman}},
		{name: "carried requires_human", in: Inputs{PR: basePR(), Verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old", 0)}}, want: []string{ReviewHoldRequiresHuman}},
		{name: "dispatch hold plus a label", in: Inputs{
			PR:       withPR(func(p *ghpkg.ReviewQueueEntry) { p.Labels = []string{"hold"} }),
			Dispatch: review.DispatchState{Human: []review.HumanReviewHold{{Repo: testRepo, Number: 7, HeadSHA: "head", Reason: review.HoldReasonDispatchExhausted + ": 3"}}},
		}, want: []string{ReviewHoldDispatch}},
		{name: "dispatch hold on an older head", in: Inputs{PR: basePR(), Dispatch: review.DispatchState{Human: []review.HumanReviewHold{{Repo: testRepo, Number: 7, HeadSHA: "old", Reason: "review fix cap reached (6/6)"}}}}},
		{name: "loop cap", in: Inputs{PR: basePR(), Verdicts: []review.Aggregate{capped}}, want: []string{ReviewHoldLoopCap}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReviewHolds(tc.in)
			var kinds []string
			for _, h := range got {
				if h.Reason == "" {
					t.Fatalf("hold %+v has no reason", h)
				}
				kinds = append(kinds, h.Kind)
			}
			if strings.Join(kinds, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("hold kinds = %v, want %v", kinds, tc.want)
			}
			// Every review hold is a human_hold card: the two never disagree.
			if len(got) > 0 && Derive(tc.in).Stage != StageHumanHold {
				t.Fatalf("ReviewHolds = %+v but Derive stage = %s", got, Derive(tc.in).Stage)
			}
		})
	}
}

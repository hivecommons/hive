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

func personApproval12962(at time.Time) *ghpkg.ReviewHoldRelease {
	return &ghpkg.ReviewHoldRelease{Via: ghpkg.ReviewHoldReleaseViaReview, State: "APPROVED", By: "owner", Person: true, At: at}
}

// baseMergeRelease13086 is a release the sweep recorded against the hold on
// holdHead after finding that the verdict on "old" (t0) judged the same
// non-merge commits, so the hold began at t0 (#13086).
func baseMergeRelease13086(holdHead string) *ghpkg.ReviewHoldRelease {
	r := personApproval12962(t0.Add(time.Hour))
	r.HoldHeadSHA, r.HeldSince = holdHead, t0
	return r
}

// A person's approving review or `/hive approve` reply after the verdict
// releases the review-swarm hold, as the self-merge sweep does (#12962). An
// earlier release, a bot's approval, or a plain comment does not, and a newer
// blocking verdict holds the PR again.
func TestReviewHolds_PersonRelease12962(t *testing.T) {
	onHead := reviewedAgg(review.VerdictRequiresHuman, "head", 1)
	after := onHead.RecordedAt.Add(30 * time.Minute)
	before := onHead.RecordedAt.Add(-30 * time.Minute)
	verdictDispatch := review.DispatchState{Human: []review.HumanReviewHold{{Repo: testRepo, Number: 7, HeadSHA: "head", Reason: "review verdict requires_human", UpdatedAt: after.Add(time.Hour)}}}
	// Collected again after the owner approved: RecordedAt moved, FirstRecordedAt did not.
	firstRecorded := onHead
	firstRecorded.FirstRecordedAt = onHead.RecordedAt.Add(-time.Hour)
	between := firstRecorded.FirstRecordedAt.Add(30 * time.Minute)
	cases := []struct {
		name      string
		verdicts  []review.Aggregate
		dispatch  review.DispatchState
		release   *ghpkg.ReviewHoldRelease
		want      []string
		wantStage Stage
	}{
		{name: "approving review after requires_human on the current head", verdicts: []review.Aggregate{onHead}, release: personApproval12962(after), wantStage: StageApproved},
		{name: "the verdict's dispatch hold is released with it", verdicts: []review.Aggregate{onHead}, dispatch: verdictDispatch, release: personApproval12962(after), wantStage: StageApproved},
		{name: "/hive approve reply after the verdict", verdicts: []review.Aggregate{onHead},
			release: &ghpkg.ReviewHoldRelease{Via: ghpkg.ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: after}, wantStage: StageApproved},
		{name: "reject released by a person", verdicts: []review.Aggregate{reviewedAgg(review.VerdictReject, "head", 1)}, release: personApproval12962(after), wantStage: StageApproved},
		{name: "carried requires_human released by a person", verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old", 1)}, release: personApproval12962(after), wantStage: StageUnreviewed},
		{name: "approval before the verdict", verdicts: []review.Aggregate{onHead}, release: personApproval12962(before), want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "/hive approve before the verdict", verdicts: []review.Aggregate{onHead},
			release: &ghpkg.ReviewHoldRelease{Via: ghpkg.ReviewHoldReleaseViaComment, Body: "/hive approve", By: "owner", Person: true, At: before}, want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "a bot's approval", verdicts: []review.Aggregate{onHead},
			release: &ghpkg.ReviewHoldRelease{Via: ghpkg.ReviewHoldReleaseViaReview, State: "APPROVED", By: "hive[bot]", At: after}, want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "a plain comment", verdicts: []review.Aggregate{onHead},
			release: &ghpkg.ReviewHoldRelease{Via: ghpkg.ReviewHoldReleaseViaComment, Body: "do not merge this", By: "owner", Person: true, At: after}, want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "a newer requires_human on a later head holds again",
			verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old", 0), reviewedAgg(review.VerdictRequiresHuman, "head", 2)},
			release:  personApproval12962(t0.Add(time.Hour)), want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "a release the sweep found before a base-branch merge redrew the verdict",
			verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old", 0), reviewedAgg(review.VerdictRequiresHuman, "head", 2)},
			release:  baseMergeRelease13086("head"), wantStage: StageApproved},
		{name: "a recorded release against another verdict's head does not move the start",
			verdicts: []review.Aggregate{reviewedAgg(review.VerdictRequiresHuman, "old", 0), reviewedAgg(review.VerdictRequiresHuman, "head", 2)},
			release:  baseMergeRelease13086("old"), want: []string{ReviewHoldRequiresHuman}, wantStage: StageHumanHold},
		{name: "release between the first recording and a later collect of the same verdict", verdicts: []review.Aggregate{firstRecorded}, release: personApproval12962(between), wantStage: StageApproved},
		{name: "a release does not lift a dispatch hold the verdict did not place",
			dispatch: review.DispatchState{Human: []review.HumanReviewHold{{Repo: testRepo, Number: 7, HeadSHA: "head", Reason: "review fix cap reached (6/6)"}}},
			release:  personApproval12962(after), want: []string{ReviewHoldDispatch}, wantStage: StageHumanHold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{PR: basePR(), Verdicts: tc.verdicts, Dispatch: tc.dispatch, HoldRelease: tc.release}
			var kinds []string
			for _, h := range ReviewHolds(in) {
				kinds = append(kinds, h.Kind)
			}
			if strings.Join(kinds, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("hold kinds = %v, want %v", kinds, tc.want)
			}
			if card := Derive(in); card.Stage != tc.wantStage {
				t.Fatalf("stage = %s, want %s: %+v", card.Stage, tc.wantStage, card)
			}
		})
	}
}

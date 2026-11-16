package review

import (
	"strings"
	"testing"
)

// A supersede adoption opens the hive's own copy (hive/adopt-<n>) and still
// labels the original stage/074-adopted-by-hive. Only an in-place adoption
// makes the original's branch the hive's (hivecommons/hive#12987); a
// superseded original's branch stays its author's, and the copy is where
// fixes go. projectbluefin/chairlift#787, a Renovate PR superseded by #796,
// took a review fix on its own branch because the label alone opened the
// gate, and was then left open as "moved on" when #796 merged.

func supersededOriginal(pr PullRequest, copyPR int) PullRequest {
	pr.Labels = []string{"stage/074-adopted-by-hive", "author/clanker"}
	pr.SupersededByHive, pr.SupersedingPR = true, copyPR
	return pr
}

func TestSupersededOtherBotOriginalFixIsWithheld(t *testing.T) {
	pr := supersededOriginal(otherBotPR("sha1", "mergeraptor[bot]"), 796)
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, otherBotOpts())

	if len(plan.FixKicks) != 0 || len(plan.State.Fixes) != 0 || len(plan.State.FixAttempts) != 0 {
		t.Fatalf("fix kicked onto a superseded original's branch: kicks=%+v fixes=%+v attempts=%+v", plan.FixKicks, plan.State.Fixes, plan.State.FixAttempts)
	}
	if len(plan.State.Human) != 0 {
		t.Fatalf("withholding the push is not a human hold: %+v", plan.State.Human)
	}
	if len(plan.WithheldFixes) != 1 {
		t.Fatalf("withheld = %d, want 1", len(plan.WithheldFixes))
	}
	if w := plan.WithheldFixes[0]; w.Author != "mergeraptor[bot]" || !strings.Contains(w.Reason, "supersession") || !strings.Contains(w.Reason, "#796") {
		t.Fatalf("withheld entry = %+v, want the supersession reason naming #796", w)
	}
	if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedSupersededByHive || plan.FixExclusions[0].Attempts != 0 {
		t.Fatalf("exclusions = %+v, want one %s with no attempts", plan.FixExclusions, FixDispatchExcludedSupersededByHive)
	}

	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, plan.State, otherBotOpts())
	if len(again.FixKicks) != 0 || len(again.WithheldFixes) != 0 {
		t.Fatalf("second cycle: kicks=%d withheld=%d, want 0 / 0", len(again.FixKicks), len(again.WithheldFixes))
	}
}

// The other-bot rule stays in force for the superseded original's review:
// the reviewer is told nobody pushes there and findings belong on the base.
func TestSupersededOtherBotOriginalReviewKeepsOtherBotInstruction(t *testing.T) {
	pr := supersededOriginal(otherBotPR("sha1", "mergeraptor[bot]"), 796)
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, otherBotOpts())
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("review kicks = %d, want 1 (reviews of the original are unchanged)", len(plan.ReviewKicks))
	}
	if msg := plan.ReviewKicks[0].Message; !strings.Contains(msg, "FINDINGS GO TO THE BASE BRANCH, NOT THIS PR.") || !strings.Contains(msg, "opened by mergeraptor[bot]") {
		t.Fatalf("review prompt lacks the other-bot instruction:\n%s", msg)
	}
}

// No regression of hivecommons/hive#12987: an in-place adoption (074, no
// supersession on record) still gets its fix pushed, for a person's PR and
// for another bot's.
func TestInPlaceAdoptedPRStillGetsFix(t *testing.T) {
	person := humanPR("sha1")
	person.Labels = []string{"stage/074-adopted-by-hive", "author/clanker"}
	bot := otherBotPR("sha1", "mergeraptor[bot]")
	bot.Labels = []string{"stage/074-adopted-by-hive"}
	for name, pr := range map[string]PullRequest{"person": person, "other bot": bot} {
		t.Run(name, func(t *testing.T) {
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, otherBotOpts())
			if len(plan.FixKicks) != 1 || len(plan.WithheldFixes) != 0 || len(plan.FixExclusions) != 0 {
				t.Fatalf("kicks=%d withheld=%+v exclusions=%+v, want the fixer dispatched", len(plan.FixKicks), plan.WithheldFixes, plan.FixExclusions)
			}
		})
	}
}

// A contributor's superseded PR is not pushed to either, even with
// review.fix_human_prs on; the reason is the supersession, not the login.
func TestSupersededContributorOriginalFixIsWithheld(t *testing.T) {
	for _, fixHuman := range []bool{false, true} {
		pr := supersededOriginal(humanPR("sha1"), 0)
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(fixHuman))
		if len(plan.FixKicks) != 0 || len(plan.WithheldFixes) != 1 {
			t.Fatalf("fix_human_prs=%t: kicks=%d withheld=%d, want 0 / 1", fixHuman, len(plan.FixKicks), len(plan.WithheldFixes))
		}
		if w := plan.WithheldFixes[0]; !strings.Contains(w.Reason, "the hive's superseding copy,") {
			t.Fatalf("fix_human_prs=%t: reason %q, want the copy named without a number", fixHuman, w.Reason)
		}
		if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedSupersededByHive {
			t.Fatalf("fix_human_prs=%t: exclusions = %+v", fixHuman, plan.FixExclusions)
		}
	}
}

// The gate itself: a supersession closes it whoever the login is, and the
// 074 exception (and the pending-fix re-kick that rides on it) needs an
// in-place adoption.
func TestFixPushAllowedSupersession(t *testing.T) {
	opts := otherBotOpts()
	own := supersededOriginal(dispatchPR("sha1"), 90)
	if fixPushAllowed(own, opts) {
		t.Fatal("superseded original under the hive's own login is push-allowed")
	}
	inPlace := humanPR("sha1")
	inPlace.Labels = []string{"stage/074-adopted-by-hive"}
	inPlace.HiveAttributed = true
	if !fixPushAllowed(inPlace, opts) || !adoptedHiveOwnedFix(inPlace, opts) {
		t.Fatal("in-place adoption lost its fix push")
	}
	superseded := inPlace
	superseded.SupersededByHive = true
	if adoptedOwnsBranch(superseded) || adoptedHiveOwnedFix(superseded, opts) {
		t.Fatal("superseded original reads as owning its branch")
	}
	if got := proposeFixesBotAuthor(supersededOriginal(otherBotPR("sha1", "renovate[bot]"), 90), opts); got != "renovate[bot]" {
		t.Fatalf("proposeFixesBotAuthor = %q, want the other bot named", got)
	}
}

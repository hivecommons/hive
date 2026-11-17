package automerge

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/website#1036 (11-17 1:55 PM EST): a 090 parked with
// `repo-auto-merge-disabled` in a repo with no Prow read "Blocked only by
// `role/trusted`" and offered `/lgtm` `/approve`. It now says auto-merge
// is off, in the dashboard's words, and asks for a GitHub approve and
// merge; a Prow-managed PR keeps the Prow close.
func TestReadyCommentRouteAutoMergeOffAndProw(t *testing.T) {
	base := stagelabels.Route{Stage: stagelabels.ReadyForHumanMerge, Summary: "verdict approved @6cc067b, 5/5 checks green, soak 30m0s elapsed"}
	_, offWords, _ := prstage.HumanMergeReasonWords(prstage.HumanMergeReasonAutoMergeOff)

	off := readyCommentRoute(base, stageObservation{Reason: "repo-auto-merge-disabled", Labels: []string{"role/trusted", "stage/090-ready-for-human-merge"}})
	if off.HumanMergeWhy != offWords || !off.NoProw {
		t.Fatalf("auto-merge-off route = why %q, noProw %v", off.HumanMergeWhy, off.NoProw)
	}
	got := stagelabels.ReadyComment(off, trustCause("repo-auto-merge-disabled", stagelabels.RoleTrusted))
	for _, want := range []string{"Auto-merge is off for this repo, so a person merges.", "approve and merge it on GitHub if you accept it."} {
		if !strings.Contains(got, want) {
			t.Fatalf("comment lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Blocked only by", "role/trusted", "/lgtm", "/approve", "tide"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("comment says %q:\n%s", unwanted, got)
		}
	}

	// Prow's plugin labels on the PR keep the Prow close.
	prow := readyCommentRoute(base, stageObservation{Reason: "repo-auto-merge-disabled", Labels: []string{"size/M", "role/trusted"}})
	if prow.NoProw {
		t.Fatal("a PR wearing size/M reads as a repo without Prow")
	}
	if got := stagelabels.ReadyComment(prow, ""); !strings.Contains(got, "`/lgtm` `/approve` (or merge) if you accept it.") || !strings.Contains(got, offWords+".") {
		t.Fatalf("prow comment:\n%s", got)
	}

	// A trust reason keeps trustCause; an unrecognised reason (policy)
	// keeps it too rather than guess at words.
	for _, reason := range []string{contributorLaneReasonClaimTier, "untrusted-author-role", "some-new-lane-reason", ""} {
		if r := readyCommentRoute(base, stageObservation{Reason: reason}); r.HumanMergeWhy != "" {
			t.Fatalf("%q: HumanMergeWhy = %q, want the trust clause kept", reason, r.HumanMergeWhy)
		}
	}
	got = stagelabels.ReadyComment(readyCommentRoute(base, stageObservation{Reason: contributorLaneReasonClaimTier, Labels: []string{"size/S"}}), trustCause(contributorLaneReasonClaimTier, stagelabels.RoleContributor))
	if !strings.Contains(got, "Blocked only by `role/contributor` (below require_tiers).") || !strings.Contains(got, "`/lgtm` `/approve` (or merge)") {
		t.Fatalf("prow trust comment changed:\n%s", got)
	}

	// Every non-trust code with words says them, never a role.
	for _, reason := range []string{prstage.AutoMergeReasonMergeRefusedMethod, prstage.AutoMergeReasonMergeRefusedReview} {
		r := readyCommentRoute(base, stageObservation{Reason: reason})
		_, words, _ := prstage.HumanMergeReasonWords(prstage.HumanMergeReason(reason))
		if r.HumanMergeWhy != words || words == "" {
			t.Fatalf("%q: HumanMergeWhy = %q, want %q", reason, r.HumanMergeWhy, words)
		}
	}
}

// projectbluefin/actions#752 (11-16 6:32 PM EST): the owner's approval
// released the workflow-path fence on a repo whose hive auto-merge is off,
// and the audit comment said "the hive merges this PR itself". It now says
// a person merges it; an auto-merge-on repo reads exactly as before.
func TestWorkflowApprovalCommentAutoMergeOff(t *testing.T) {
	for _, state := range []int{workflowApprovalVerdictNone, workflowApprovalVerdictAnswered, workflowApprovalVerdictOpen} {
		for _, approval := range []workflowApproval{{Login: "Danathar"}, {Login: "Danathar", CarriedFrom: "1234567890ab"}} {
			off := workflowApprovalComment("9fd007702e97", approval, state, true)
			if strings.Contains(off, "the hive merges this PR itself") || !strings.Contains(off, "a person merges it once the usual gates") ||
				!strings.Contains(off, "auto-merge is off for this repository, so the hive never merges it") {
				t.Fatalf("state %d, carried %q: auto-merge-off comment:\n%s", state, approval.CarriedFrom, off)
			}
			on := workflowApprovalComment("9fd007702e97", approval, state, false)
			if !strings.Contains(on, "the hive merges this PR itself once the usual gates") || strings.Contains(on, "auto-merge is off") {
				t.Fatalf("state %d, carried %q: auto-merge-on comment changed:\n%s", state, approval.CarriedFrom, on)
			}
			// The marker line is the same either way: the per-head dedupe
			// keys on it.
			if strings.SplitN(off, "\n", 2)[0] != strings.SplitN(on, "\n", 2)[0] {
				t.Fatalf("marker differs:\n%s\n%s", off, on)
			}
		}
	}
}

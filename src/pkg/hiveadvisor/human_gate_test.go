package hiveadvisor

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func hasSignal(sigs []Signal, name, value string) bool {
	for _, s := range sigs {
		if s.Name == name && s.Value == value {
			return true
		}
	}
	return false
}

// projectbluefinParkedReview is actions#675's ownership reason on
// 2026-10-26: every reviewer perspective's summary, with the same decision
// request five times. The advice printed all of it.
const projectbluefinParkedReview = "Hive's review is parked for a person: correctness requires human: Read the workflow at head: all four run blocks now reference quoted env vars; remaining ${{ inputs.* }} are in name:/env: only.; " +
	"maintainer decision requested: @castrojo should decide whether to supply/waive the consumer-validation evidence for this fork PR, and whether #544 or #675 lands first.; " +
	"maintainer decision requested: @castrojo should decide whether to supply/waive the consumer-validation evidence for this fork PR, and whether #544 or #675 lands first.; " +
	"maintainer decision requested: @castrojo should decide whether to supply/waive the consumer-validation evidence for this fork PR, and whether #544 or #675 lands first."

// humanGatePR is one your-turn PR in repo with the dashboard's reason code
// and text.
func humanGatePR(repo string, n int, verb, code, reason string) PRItem {
	return PRItem{Repo: repo, Number: n, Band: PRBandWaiting, Ownership: Ownership{YourTurn: true, Verb: verb, ReasonCode: code, Reason: reason}}
}

const approvingReview = "Waiting on a person's review: an approving review is required by branch protection (0 given)"

// The shape of the projectbluefin queue on 2026-10-26, scaled down: decisions
// parked in utah-packages, approvals owed in common and actions, one
// reviewer's requested changes, and one parked hive review whose reason runs
// to a paragraph.
func TestHumanGateAdviceSaysWhyAndHowToHelp(t *testing.T) {
	var prs []PRItem
	for i := 0; i < 4; i++ {
		prs = append(prs, humanGatePR("utah-packages", 600+i, "DECIDE", "parked_needs_decision", "Parked until a maintainer decides"))
	}
	prs = append(prs, humanGatePR("utah", 919, "DECIDE", "parked_needs_decision", "Parked until a maintainer decides"))
	for i := 0; i < 3; i++ {
		prs = append(prs, humanGatePR("common", 1550+i, "REVIEW", "review_decision_required", approvingReview))
	}
	prs = append(prs, humanGatePR("actions", 665, "REVIEW", "review_decision_required", approvingReview))
	prs = append(prs, humanGatePR("actions", 544, "REVIEW", "review_decision_required", "Waiting on a person's review: a reviewer requested changes"))
	prs = append(prs, humanGatePR("actions", 675, "REVIEW", "review_dispatch_hold", projectbluefinParkedReview))
	prs = append(prs, mkPRs(5, "open", nil)...)

	r := find(t, queueCandidates(Queue{PRs: prs}, Thresholds{}), "clear-human-gate-prs")
	if r.Rationale != "11 of 16 open PRs (68%) are waiting on a person; the hive cannot move them on its own." {
		t.Fatalf("rationale = %q", r.Rationale)
	}
	wantWhy := []string{
		"5 parked until a maintainer decides (4 in utah-packages, 1 in utah).",
		"4 waiting for an approving review that branch protection requires (3 in common, 1 in actions).",
		"1 with changes a reviewer requested (all in actions).",
		"1 with the hive's review parked for a person (all in actions).",
	}
	if !reflect.DeepEqual(r.Why, wantWhy) {
		t.Fatalf("why =\n%s", strings.Join(r.Why, "\n"))
	}
	if want := "answer the parked decisions in utah-packages and utah; approve PRs in common and actions; follow up on the requested changes in actions"; r.Help != want {
		t.Fatalf("help = %q\nwant %q", r.Help, want)
	}
	if !hasSignal(r.Signals, "verbs", "5 DECIDE, 6 REVIEW") {
		t.Fatalf("signals = %+v", r.Signals)
	}
	for _, it := range r.Items {
		if utf8.RuneCountInString(it.Note) > maxNoteRunes+utf8.RuneCountInString("REVIEW — ") {
			t.Fatalf("item note too long to read in a list: %q", it.Note)
		}
		if it.Number == 675 && it.Note != "REVIEW — Hive's review is parked for a person" {
			t.Fatalf("parked review note = %q", it.Note)
		}
		if it.Number == 544 && it.Note != "REVIEW — a reviewer requested changes" {
			t.Fatalf("the review-decision note drops the clause REVIEW already says: %q", it.Note)
		}
		if it.Number == 665 && it.Note != "REVIEW — an approving review is required by branch protection (0 given)" {
			t.Fatalf("approval note = %q", it.Note)
		}
	}
	if got := NeedsHumanItems(Queue{PRs: prs}); got[len(got)-1].Note != "REVIEW — "+projectbluefinParkedReview {
		t.Fatalf("the inbox list keeps the full reason: %q", got[len(got)-1].Note)
	}
}

// Past five groups the smallest fold into one line, so the list stays short.
func TestHumanGateWhyFoldsSmallGroups(t *testing.T) {
	codes := []string{"parked_needs_decision", "parked_needs_human", "person_pr_needs_human", "review_requires_human", "sentinel_flagged", "hand_merge_required", "review_rejected"}
	var owns []Ownership
	var repos []string
	for i, code := range codes {
		for j := 0; j < len(codes)-i; j++ {
			owns = append(owns, Ownership{YourTurn: true, Verb: "REVIEW", ReasonCode: code})
			repos = append(repos, "octo/demo")
		}
	}
	why, help := humanGateWhy(owns, repos)
	if len(why) != maxWhyGroups+1 || why[maxWhyGroups] != "3 more for other reasons; the full list has each one." {
		t.Fatalf("why =\n%s", strings.Join(why, "\n"))
	}
	if strings.Count(help, ";") != maxHelpActions-1 {
		t.Fatalf("help names %d actions, want %d: %q", strings.Count(help, ";")+1, maxHelpActions, help)
	}
}

func TestHumanGateWhyWordsUnknownCodesFromTheirReason(t *testing.T) {
	why, _ := humanGateWhy([]Ownership{{YourTurn: true, Verb: "REVIEW", ReasonCode: "brand_new_code", Reason: "Waiting on something new: details"}}, []string{"a", "b"}[:1])
	if !reflect.DeepEqual(why, []string{"1 waiting for a person: waiting on something new (all in a)."}) {
		t.Fatalf("why = %q", why)
	}
}

// Each wording github.BranchProtectionBlockReason gives a review decision
// lands in its own group: "approve PRs" is the wrong advice for a PR whose
// reviewer asked for changes, named or not.
func TestHumanGateWhySplitsEveryReviewDecisionWording(t *testing.T) {
	reasons := []string{
		"changes requested by @alice",
		"changes requested by @alice, @bob",
		"a reviewer requested changes",
		"code-owner review required by branch protection",
		"blocked by a branch rule Hive does not read (ruleset requires code-owner review)",
		"an approving review is required by branch protection (0 given)",
	}
	var owns []Ownership
	var repos []string
	for _, reason := range reasons {
		owns = append(owns, Ownership{YourTurn: true, Verb: "REVIEW", ReasonCode: "review_decision_required", Reason: "Waiting on a person's review: " + reason})
		repos = append(repos, "octo/demo")
	}
	why, help := humanGateWhy(owns, repos)
	want := []string{
		"3 with changes a reviewer requested (all in octo/demo).",
		"2 waiting for a code owner's review (all in octo/demo).",
		"1 waiting for an approving review that branch protection requires (all in octo/demo).",
	}
	if !reflect.DeepEqual(why, want) {
		t.Fatalf("why =\n%s", strings.Join(why, "\n"))
	}
	if want := "follow up on the requested changes in octo/demo; code owners: review PRs in octo/demo; approve PRs in octo/demo"; help != want {
		t.Fatalf("help = %q\nwant %q", help, want)
	}
	if got := shortOwnershipNote(owns[0]); got != "REVIEW — changes requested by @alice" {
		t.Fatalf("note = %q", got)
	}
}

func TestWhereTextNamesTheLargestRepos(t *testing.T) {
	got := whereText(map[string]int{"a": 5, "b": 3, "c": 2, "d": 1, "e": 1}, 12)
	if got != "5 in a, 3 in b, 2 in c, 2 elsewhere" {
		t.Fatalf("where = %q", got)
	}
}

func TestShortOwnershipNoteCutsAClauselessReason(t *testing.T) {
	note := shortOwnershipNote(Ownership{Verb: "REVIEW", Reason: strings.Repeat("x", 150)})
	if utf8.RuneCountInString(note) != utf8.RuneCountInString("REVIEW — ")+maxNoteRunes || !strings.HasSuffix(note, "…") {
		t.Fatalf("note = %q (%d runes)", note, utf8.RuneCountInString(note))
	}
	if got := shortOwnershipNote(Ownership{Verb: "DECIDE", Reason: "Parked until a maintainer decides"}); got != "DECIDE — Parked until a maintainer decides" {
		t.Fatalf("a short reason is kept whole: %q", got)
	}
}

// One group left over is shown, not folded into "1 more".
func TestHumanGateWhyShowsASingleLeftoverGroup(t *testing.T) {
	var owns []Ownership
	for _, code := range []string{"parked_needs_decision", "parked_needs_human", "person_pr_needs_human", "review_requires_human", "sentinel_flagged", "hand_merge_required"} {
		owns = append(owns, Ownership{YourTurn: true, ReasonCode: code})
	}
	why, _ := humanGateWhy(owns, make([]string, len(owns)))
	if len(why) != 6 || strings.Contains(strings.Join(why, "\n"), "more for other reasons") {
		t.Fatalf("why =\n%s", strings.Join(why, "\n"))
	}
}

// A spread-out group's action names no repositories: two of six is not where
// to start.
func TestHumanGateHelpNamesReposOnlyWhereMostItemsAre(t *testing.T) {
	owns := make([]Ownership, 6)
	for i := range owns {
		owns[i] = Ownership{YourTurn: true, ReasonCode: "review_requires_human"}
	}
	_, help := humanGateWhy(owns, []string{"common", "common", "chairlift", "dakota-iso", "utah", "utah-packages"})
	if help != "read the hive's review and decide" {
		t.Fatalf("help = %q", help)
	}
	_, help = humanGateWhy(owns, []string{"common", "common", "common", "chairlift", "utah", "utah-packages"})
	if help != "read the hive's review and decide in common and chairlift" {
		t.Fatalf("help = %q", help)
	}
}

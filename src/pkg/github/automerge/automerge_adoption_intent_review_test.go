package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#943: the PR marks two packages wontfix (2
// lines in config/factory-build-backlog.toml) and adds 18 lines of guidance.
// The review asked for both entries to stay pending. The hive adopted the PR
// for those asks and pushed eed6933, which drops both entries and rewrites
// the guidance, keeping 2 of the PR's 20 changed lines.
func utahPackages943Files(fixed bool) []map[string]string {
	if fixed {
		return []map[string]string{{"filename": "docs/skills/factory-build-backlog.md", "status": "modified",
			"patch": "@@ -355,0 +355,6 @@\n+### Packages removed from Fedora\n+\n" + strings.Repeat("+defer to the kernel-module gate\n", 3) + "+import the negativo17 recipe"}}
	}
	return []map[string]string{
		{"filename": "config/factory-build-backlog.toml", "status": "modified",
			"patch": "@@ -912,0 +913,2 @@\n+intel-vaapi-driver = \"wontfix: dropped from Fedora\"\n+kmod-v4l2loopback = \"wontfix: upstream gone\""},
		{"filename": "docs/skills/factory-build-backlog.md", "status": "modified",
			"patch": "@@ -355,0 +355,18 @@\n+### Packages removed from Fedora\n+\n" + strings.Repeat("+removed packages with unreachable upstream go to wontfix\n", 16)},
	}
}

// adopt943ForReview adopts PR #7 in place at head aaa for the review swarm's
// requested changes, CI green: the utah-packages#943 adoption.
func adopt943ForReview(t *testing.T) (*adoptHarness, func(), []string) {
	t.Helper()
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.comparePatches = map[string][]map[string]string{"aaa": utahPackages943Files(false)}
	h.observe(adoptPR(7, "aaa"), contributorLaneReasonChangesRequested)
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonChangesRequested)
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonChangesRequested)
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptedInPlace() || rec.AdoptedHead != "aaa" {
		done()
		t.Fatalf("not adopted in place: %+v", rec)
	}
	if nudges := nudgeComments(h.api); len(nudges) != 1 || !strings.Contains(nudges[0], "the review swarm requested changes") {
		done()
		t.Fatalf("PR was not returned for the review asks: %v", h.api.comments)
	}
	return h, done, []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
}

// reviewerVerdicts stands in for the review swarm: approve for the heads in
// approved, changes requested for every other head.
func reviewerVerdicts(h *adoptHarness, approved ...string) {
	h.e.contributorLaneVerdict = func(_ string, _ int, head string) ContributorLaneVerdict {
		for _, a := range approved {
			if head == a {
				return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
			}
		}
		return ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}
	}
}

// The utah-packages#943 order: the push is judged at once and parked (no
// verdict on it yet), then the reviewer approves that head. The approve
// answers the park: it is released, the lane admits the head, and no second
// note is posted.
func TestAdoptedReviewFixThatDropsChangeIsReleasedByApprove(t *testing.T) {
	h, done, adopted := adopt943ForReview(t)
	defer done()
	reviewerVerdicts(h)
	h.adopt.comparePatches["eed6933"] = utahPackages943Files(true)
	h.adopt.setCommitAuthor("eed6933", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "eed6933", adopted...), "ci-pending")
	if notes := fixerRevertsComments(h.api); len(notes) != 1 || !strings.Contains(notes[0], "leaves 2 of the 20 lines") {
		t.Fatalf("before the verdict: notes = %v (all comments %v)", notes, h.api.comments)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "eed6933"); got != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("merge gate before the verdict = %q, want the refusal", got)
	}

	// The reviewer approves eed6933.
	reviewerVerdicts(h, "eed6933")
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "eed6933"); got != "" {
		t.Fatalf("merge gate after the approve = %q, want none", got)
	}
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "eed6933", "stage/085-needs-decision", "role/contributor", "author/clanker"), "ci-pending")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptionEscalated("eed6933") || rec.Stage == stagelabels.NeedsDecision || rec.AdoptedHead != "eed6933" {
		t.Fatalf("park not released after the approve: %+v", rec)
	}
	if len(fixerRevertsComments(h.api)) != 0 || strings.Contains(strings.Join(h.addedLabels(), ","), "stage/085") {
		t.Fatalf("after the approve: comments=%v added=%v", h.api.comments, h.api.added)
	}
}

// The approve is already on the head when the push is first judged: no
// park and no note.
func TestAdoptedReviewFixApprovedBeforeCheckIsNotParked(t *testing.T) {
	h, done, adopted := adopt943ForReview(t)
	defer done()
	reviewerVerdicts(h, "eed6933")
	h.adopt.comparePatches["eed6933"] = utahPackages943Files(true)
	h.adopt.setCommitAuthor("eed6933", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "eed6933", adopted...), "ci-pending")
	if len(fixerRevertsComments(h.api)) != 0 || strings.Contains(strings.Join(h.addedLabels(), ","), "stage/085") {
		t.Fatalf("approved review fix parked: comments=%v added=%v", h.api.comments, h.api.added)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptionEscalated("eed6933") || rec.AdoptedHead != "eed6933" {
		t.Fatalf("record = %+v", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "eed6933"); got != "" {
		t.Fatalf("merge gate = %q, want none", got)
	}
}

// What the reviewer actually said on eed6933: the old asks are resolved but
// it still requests changes. That is no approve, so the park holds.
func TestAdoptedReviewFixWithoutApproveStaysParked(t *testing.T) {
	h, done, adopted := adopt943ForReview(t)
	defer done()
	reviewerVerdicts(h)
	h.adopt.comparePatches["eed6933"] = utahPackages943Files(true)
	h.adopt.setCommitAuthor("eed6933", testHiveAppBotLogin)
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "eed6933", adopted...), "ci-pending")
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "eed6933", "stage/085-needs-decision", "role/contributor", "author/clanker"), "ci-pending")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptionEscalated("eed6933") || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("record = %+v, want the park held", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "eed6933"); got != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("merge gate = %q, want the refusal", got)
	}
}

// The hole hivecommons/hive#16816 closed stays closed: an adoption for red
// CI (utah#1091) whose fixer undid the PR is refused even when the reviewer
// approves the narrowed head, before or after the push is judged.
func TestAdoptedCIFixThatRevertsPRStaysRefusedWhenApproved(t *testing.T) {
	h, done, adopted := adoptUtah1091InPlace(t)
	defer done()
	reviewerVerdicts(h, "a75a567")
	h.adopt.comparePatches["a75a567"] = utah1091Files(false, "")
	h.adopt.setCommitAuthor("a75a567", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", adopted...), "ci-pending")
	if notes := fixerRevertsComments(h.api); len(notes) != 1 {
		t.Fatalf("notes = %v (all comments %v)", notes, h.api.comments)
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "a75a567", "stage/085-needs-decision", "role/contributor", "author/clanker"), "ci-pending")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.AdoptionEscalated("a75a567") || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("record = %+v, want the refusal held", rec)
	}
	if got := h.e.adoptionIntentGate("acme", "widget", 7, "a75a567"); got != stagelabels.LaneReasonAdoptionFixerReverts {
		t.Fatalf("merge gate = %q, want the refusal", got)
	}
}

// adoptedForReview reads the adoption's cause: review asks with CI not red.
func TestAdoptedForReview(t *testing.T) {
	asked := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	for name, tc := range map[string]struct {
		obs  stageObservation
		want bool
	}{
		"review asks, CI green":        {stageObservation{Reason: contributorLaneReasonChangesRequested}, true},
		"blocking verdict, CI pending": {stageObservation{Reason: "ci-pending", Detail: asked}, true},
		"red CI":                       {stageObservation{Reason: "check-failing:unit"}, false},
		"red CI and review asks":       {stageObservation{Reason: "check-failing:unit", Detail: asked}, false},
		"conflict":                     {stageObservation{Reason: "conflicting"}, false},
	} {
		if got := adoptedForReview(tc.obs); got != tc.want {
			t.Errorf("%s: adoptedForReview = %v, want %v", name, got, tc.want)
		}
	}
}

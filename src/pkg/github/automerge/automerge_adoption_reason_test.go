package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A contributor PR nudged for the review swarm's requested changes with CI
// green (Danathar/tududi#415): the adoption note used to say "CI stayed red
// for 2h", the superseding PR body "adopted after CI went red", and the
// in-place note promised to "fix CI". Each now names what the nudge asked for.
func TestAdoptionNotesNameReviewAsks(t *testing.T) {
	review := stageObservation{
		Number: 7, HeadSHA: "aaa", Author: "mallory", BaseRef: "main",
		Reason: contributorLaneReasonChangesRequested, LaneReason: contributorLaneReasonChangesRequested,
		Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}},
	}
	red := stageObservation{Number: 7, HeadSHA: "aaa", Author: "mallory", BaseRef: "main", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}
	both := red
	both.Detail = review.Detail

	for name, tc := range map[string]struct {
		obs              stageObservation
		cause, body, fix string
		reviewNote       bool
	}{
		"review only": {review, "the review asks stayed open (the review swarm requested changes) for 2h after the nudge",
			"adopted after review asks went unaddressed and the contributor was unavailable", "rebase this branch on `main` here", true},
		"red only": {red, "CI stayed red for 2h after the nudge",
			"adopted after CI went red and the contributor was unavailable", "rebase this branch on `main` and fix CI here", false},
		"red and review": {both, "CI stayed red and the review asks stayed open (the review swarm requested changes) for 2h",
			"adopted after CI went red and review asks went unaddressed", "rebase this branch on `main` and fix CI here", true},
	} {
		t.Run(name, func(t *testing.T) {
			inPlace := AdoptionInPlaceComment(tc.obs, "acme-hive", AdoptReasonGrace, 2*time.Hour, true)
			for _, want := range []string{tc.cause, "relay nudge went unanswered", tc.fix} {
				if !strings.Contains(inPlace, want) {
					t.Fatalf("in-place note missing %q:\n%s", want, inPlace)
				}
			}
			if got := strings.Contains(inPlace, "repairs CI only"); got != tc.reviewNote {
				t.Fatalf("in-place review note=%v want %v:\n%s", got, tc.reviewNote, inPlace)
			}
			super := AdoptionComment(tc.obs, "acme-hive", 90, AdoptReasonGrace, 2*time.Hour, true)
			if !strings.Contains(super, tc.cause) {
				t.Fatalf("supersede note missing %q:\n%s", tc.cause, super)
			}
			if body := AdoptionPRBody(tc.obs, stagelabels.Record{AdoptReason: AdoptReasonGrace}, false, ""); !strings.Contains(body, "Supersedes #7 ("+tc.body) {
				t.Fatalf("superseding body missing %q:\n%s", tc.body, body)
			}
		})
	}

	// Review only: neither note claims CI was red or is being fixed.
	for _, note := range []string{
		AdoptionInPlaceComment(review, "acme-hive", AdoptReasonGrace, 2*time.Hour, false),
		AdoptionComment(review, "acme-hive", 90, AdoptReasonGrace, 2*time.Hour, false),
		AdoptionPRBody(review, stagelabels.Record{AdoptReason: AdoptReasonGrace}, false, ""),
	} {
		for _, unwanted := range []string{"CI stayed red", "fix CI", "CI went red"} {
			if strings.Contains(note, unwanted) {
				t.Fatalf("review-only note says %q:\n%s", unwanted, note)
			}
		}
	}
	if got := AdoptionComment(review, "acme-hive", 90, AdoptReasonGrace, 2*time.Hour, false); !strings.Contains(got, "the review asks stayed open (the review swarm requested changes) for 2h after the nudge and no relay session was reachable") ||
		!strings.Contains(got, "the hive will finish it there") {
		t.Fatalf("offline supersede note:\n%s", got)
	}
}

// The same through the sweep: a PR returned to its author for the review
// swarm's requested changes, CI green, is adopted after the grace with a note
// naming the review asks.
func TestAdoptionSweepNamesReviewAsks(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
	defer done()

	h.observe(adoptPR(7, "aaa"), contributorLaneReasonChangesRequested)
	if len(nudgeComments(h.api)) != 1 || !strings.Contains(nudgeComments(h.api)[0], "the review swarm requested changes") {
		t.Fatalf("nudge: %v", h.api.comments)
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), contributorLaneReasonChangesRequested)
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "the review asks stayed open (the review swarm requested changes) for 2h after the nudge") ||
		strings.Contains(comments[0], "CI stayed red") || strings.Contains(comments[0], "fix CI") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	if body, _ := h.adopt.pulls[0]["body"].(string); !strings.Contains(body, "adopted after review asks went unaddressed") {
		t.Fatalf("superseding body:\n%s", body)
	}
}

package automerge

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// projectbluefin/utah-packages#862 / #869: the fork-guard park only knew the
// head the last CI-enriched enumeration saw, so every new head of a fork PR
// red only on its `Canary` fork guard was handed back to its author first —
// a "fix Canary" nudge and, after the grace, an adoption — before the next
// enumeration parked it at 085 again. A head red on the guard alone now goes
// straight to the park; a head with any other red check keeps the
// author-facing route unchanged.
func TestForkGuardHeadNeitherNudgedNorAdopted(t *testing.T) {
	cases := []struct {
		name string
		// red is the check digest of the second head, which the
		// enumeration has not classified yet.
		red       []string
		wantNudge bool
	}{
		{name: "fork guard only", red: []string{"Canary"}},
		{name: "fork guard and a real failure", red: []string{"Canary", "unit"}, wantNudge: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var guard []string
			p := adoptPolicy()
			p.ForkGuard = func(string, int, string) []string { return guard }
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			h.e.checkSummary = func(_ context.Context, _, _, sha string) (hgithub.CommitChecksSummary, error) {
				if sha == "aaa" {
					return hgithub.CommitChecksSummary{Total: 3, Green: 2, Red: []string{"Canary"}, DCO: 1}, nil
				}
				return hgithub.CommitChecksSummary{Total: 3, Green: 3 - len(tc.red), Red: tc.red, DCO: 1}, nil
			}
			key := stageKey("acme", "widget", 7)

			// The enumeration classified head aaa: parked, never nudged.
			guard = []string{"Canary"}
			h.observe(adoptPR(7, "aaa"), "required-check-failing:Canary")
			parked := []string{"stage/085-needs-decision", "role/contributor", "author/clanker"}
			h.observe(adoptPR(7, "aaa", parked...), "required-check-failing:Canary")
			if got := nudgeComments(h.api); len(got) != 0 || len(h.sink.nudges) != 0 {
				t.Fatalf("fork-guard head nudged: %v", got)
			}
			if rec, _ := h.e.stageStoreOrShared().Get(key); !slices.Equal(rec.ForkGuardChecks, []string{"Canary"}) {
				t.Fatalf("fork guard not remembered: %+v", rec.ForkGuardChecks)
			}

			// A new head the enumeration has not seen: the policy knows nothing.
			guard = nil
			h.api.reset()
			h.observe(adoptPR(7, "bbb", parked...), "required-check-failing:Canary")
			nudges := nudgeComments(h.api)
			if !tc.wantNudge {
				if len(nudges) != 0 || len(h.sink.nudges) != 0 {
					t.Fatalf("new fork-guard head nudged: %v", nudges)
				}
			} else if len(nudges) != 1 || !strings.Contains(nudges[0], "head=bbb n=1") || !strings.Contains(nudges[0], "`Canary`") {
				t.Fatalf("new head with a real failure not nudged: %v", h.api.comments)
			}

			// Past every grace: adopted only when a real failure is red.
			returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			labels := parked
			if tc.wantNudge {
				labels = returned
			}
			h.api.reset()
			h.clock.advance(3 * time.Hour)
			h.observe(adoptPR(7, "bbb", labels...), "required-check-failing:Canary")
			adopted := len(h.adopt.pulls) > 0 || len(adoptionComments(h.api)) > 0
			if adopted != tc.wantNudge {
				t.Fatalf("adopted = %v, want %v: pulls=%d comments=%v", adopted, tc.wantNudge, len(h.adopt.pulls), h.api.comments)
			}
		})
	}
}

// A nudge never lists a fork-guard check; adoption refuses a head whose
// only blocker is the guard but not one with a review or DCO ask.
func TestForkGuardNudgeChecksAndAdoptionPredicate(t *testing.T) {
	guard := []string{"Canary"}
	if got := withoutForkGuard([]string{"Canary"}, guard); len(got) != 0 {
		t.Fatalf("guard-only checks = %v", got)
	}
	if got := withoutForkGuard([]string{"Canary", "unit"}, guard); !reflect.DeepEqual(got, []string{"unit"}) {
		t.Fatalf("guard plus real failure = %v", got)
	}
	if got := withoutForkGuard([]string{"Canary"}, nil); !reflect.DeepEqual(got, []string{"Canary"}) {
		t.Fatalf("no guard evidence = %v", got)
	}

	cases := []struct {
		name string
		obs  stageObservation
		want bool
	}{
		{name: "parked on the guard", obs: stageObservation{Reason: stageReasonForkGuard + "Canary", ForkGuardChecks: guard}, want: true},
		{name: "guard is the red required check", obs: stageObservation{Reason: "required-check-failing:Canary", ForkGuardChecks: guard}, want: true},
		{name: "guard is the red optional check", obs: stageObservation{Reason: stageReasonOptionalCheckFailing + "Canary", ForkGuardChecks: guard}, want: true},
		{name: "review ask beside the guard", obs: stageObservation{Reason: "reviewer-verdict:changes_requested", LaneReason: "required-check-failing:Canary", ForkGuardChecks: guard}},
		{name: "real failure, no guard evidence", obs: stageObservation{Reason: "required-check-failing:Canary"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoptionForkGuardOnly(tc.obs); got != tc.want {
				t.Fatalf("adoptionForkGuardOnly = %v, want %v", got, tc.want)
			}
		})
	}
}

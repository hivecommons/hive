package automerge

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#18872: a contributor got "this PR is back in your court"
// and 16-39 minutes later the hive adopted the same PR. The nudge and the
// adoption now agree on one outcome per head: a head the hive adopts anyway
// (author relay offline or unregistered) is nudged on the record only, so
// the adoption clock runs and the adoption note is the one notification; a
// head with a real chance (relay online) keeps its nudge and is not adopted
// before the grace.

// withholdPatterns are the four pairs observed on the hive's spoke:
// #18561 contributor-lane, #18628 fork-not-editable, #18514 check-failing,
// #18510 conflicting.
var withholdPatterns = []struct{ name, reason string }{
	{"contributor-lane", contributorLaneReasonChangesRequested},
	{"fork-not-editable", autoMergeReasonForkNotEditable},
	{"check-failing", "required-check-failing:changelog-fragment-guard"},
	{"conflicting", "conflicting"},
}

var withholdLabels = []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

func withholdPolicy() StageLabelPolicy {
	p := adoptPolicy()
	p.NudgeWithholdWhenAdopting = true
	return p
}

func TestNudgeWithheldWhenHiveAdoptsAnyway(t *testing.T) {
	for _, pat := range withholdPatterns {
		for _, registered := range []bool{true, false} {
			name := pat.name + "/unregistered"
			if registered {
				name = pat.name + "/offline"
			}
			t.Run(name, func(t *testing.T) {
				h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, withholdPolicy())
				defer done()
				start := h.clock.at
				h.sink.presence = &ContributorPresence{Registered: registered}
				h.observe(adoptPR(7, "aaa"), pat.reason)
				// Invariant 1: no nudge comment for a head that is adopted anyway.
				if got := nudgeComments(h.api); len(got) != 0 {
					t.Fatalf("nudge posted for a head the hive adopts anyway: %v", got)
				}
				// The nudge is recorded: the adoption clock is running on it.
				rec, ok := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
				if !ok || rec.NudgeHead != "aaa" || rec.NudgeCount == 0 || rec.NudgeWithheldHead != "aaa" {
					t.Fatalf("withheld nudge not recorded: %+v", rec)
				}
				// The adoption follows on the AutoAdoption clock, with its note.
				h.api.reset()
				h.clock.at = start.Add(16 * time.Minute)
				h.observe(adoptPR(7, "aaa", withholdLabels...), pat.reason)
				if len(adoptionComments(h.api)) != 1 {
					t.Fatalf("no adoption note at +16m: %v", h.api.comments)
				}
				if got := nudgeComments(h.api); len(got) != 0 {
					t.Fatalf("a nudge was posted next to the adoption: %v", got)
				}
			})
		}
	}
}

func TestNudgeKeptAndNoEarlyAdoptionWhenAuthorHasARealChance(t *testing.T) {
	for _, pat := range withholdPatterns {
		t.Run(pat.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, withholdPolicy())
			defer done()
			start := h.clock.at
			h.sink.presence = &ContributorPresence{Registered: true, Online: true}
			h.observe(adoptPR(7, "aaa"), pat.reason)
			if len(nudgeComments(h.api)) != 1 {
				t.Fatalf("an online author is nudged: %v", h.api.comments)
			}
			// Invariant 2: not adopted before the online grace.
			for _, at := range []time.Duration{16 * time.Minute, 31 * time.Minute, time.Hour} {
				h.api.reset()
				h.clock.at = start.Add(at)
				h.observe(adoptPR(7, "aaa", withholdLabels...), pat.reason)
				if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 {
					t.Fatalf("adopted at +%v before the grace: %v", at, h.api.comments)
				}
			}
		})
	}
}

func TestWithheldNudgeIsPostedWhenTheAuthorReappears(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, withholdPolicy())
	defer done()
	start := h.clock.at
	h.sink.presence = &ContributorPresence{Registered: true}
	h.observe(adoptPR(7, "aaa"), "conflicting")
	if len(nudgeComments(h.api)) != 0 {
		t.Fatalf("nudge should be withheld: %v", h.api.comments)
	}
	// The relay reconnects before the adoption is due: a real chance now.
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.clock.at = start.Add(5 * time.Minute)
	h.observe(adoptPR(7, "aaa", withholdLabels...), "conflicting")
	if len(nudgeComments(h.api)) != 1 {
		t.Fatalf("the withheld nudge is owed once the author is reachable: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.NudgeWithheldHead != "" {
		t.Fatalf("withheld mark not cleared: %+v", rec)
	}
}

func TestNudgeNotWithheldWithoutCertainAdoption(t *testing.T) {
	t.Run("policy flag off", func(t *testing.T) {
		h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, adoptPolicy())
		defer done()
		h.sink.presence = &ContributorPresence{Registered: true}
		h.observe(adoptPR(7, "aaa"), "conflicting")
		if len(nudgeComments(h.api)) != 1 {
			t.Fatalf("withholding is off by default in the engine: %v", h.api.comments)
		}
	})
	t.Run("no presence source", func(t *testing.T) {
		fake := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "comment"}}
		h, done := newAdoptHarnessWith(t, fake, nudgeOnlySink{fake}, withholdPolicy())
		defer done()
		h.observe(adoptPR(7, "aaa"), "conflicting")
		if len(nudgeComments(h.api)) != 1 {
			t.Fatalf("without presence the adoption is not certain: %v", h.api.comments)
		}
	})
	t.Run("adoption disabled", func(t *testing.T) {
		p := withholdPolicy()
		p.Adoption = false
		rec := stagelabels.Record{}
		if adoptionBound(rec, stageObservation{HeadSHA: "aaa"}, p, ContributorPresence{Registered: true}, true) {
			t.Fatal("adoption off cannot be certain")
		}
	})
	t.Run("maintainer policy ask", func(t *testing.T) {
		p := withholdPolicy()
		p.AdoptionMaintainerPolicy = "ask"
		if adoptionBound(stagelabels.Record{}, stageObservation{HeadSHA: "aaa"}, p, ContributorPresence{Registered: true}, true) {
			t.Fatal("a held adoption cannot be certain")
		}
	})
}

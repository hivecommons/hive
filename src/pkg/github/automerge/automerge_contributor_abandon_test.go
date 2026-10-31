package automerge

import (
	"testing"
	"time"
)

func TestAbandonReasonMapsAdoptReasons(t *testing.T) {
	for adopt, want := range map[string]string{
		AdoptReasonOffline:        AbandonReasonOffline,
		AdoptReasonUnregistered:   AbandonReasonUnresponsive,
		AdoptReasonGrace:          AbandonReasonUnresponsive,
		AdoptReasonOperator:       AbandonReasonOperatorTakeover,
		AdoptReasonOperatorForced: AbandonReasonOperatorTakeover,
		AdoptReasonDCORehome:      AbandonReasonUnresponsive,
	} {
		if got := abandonReason(adopt); got != want {
			t.Errorf("abandonReason(%q) = %q, want %q", adopt, got, want)
		}
	}
}

// hivecommons/hive#15782: a supersession reports one abandon naming the
// hive's copy; later passes over the adopted PR report nothing more.
func TestAdoptionSupersedeRecordsAbandon(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(2 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("not adopted: pulls=%v", h.adopt.pulls)
	}
	if len(h.sink.abandons) != 1 {
		t.Fatalf("abandons = %+v, want one", h.sink.abandons)
	}
	a := h.sink.abandons[0]
	if a.Author != "mallory" || a.Repo != "acme/widget" || a.PR != 7 || a.AdoptedIntoPR != 90 || a.Reason != AbandonReasonUnresponsive || a.At.IsZero() {
		t.Fatalf("abandon = %+v", a)
	}
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
	if len(h.sink.abandons) != 1 {
		t.Fatalf("adopted PR re-reported: %+v", h.sink.abandons)
	}
}

func TestAdoptionInPlaceRecordsOfflineAbandon(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(30 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.sink.abandons) != 1 {
		t.Fatalf("abandons = %+v, want one", h.sink.abandons)
	}
	if a := h.sink.abandons[0]; a.PR != 7 || a.AdoptedIntoPR != 0 || a.Reason != AbandonReasonOffline {
		t.Fatalf("in-place abandon = %+v", a)
	}
}

func TestAdoptNowRecordsOperatorTakeoverAbandon(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.e.stageStoreOrShared().NoteAdoptionForced(stageKey("acme", "widget", 7), "clubanderson", h.clock.now())
	h.clock.advance(time.Second)
	h.observe(adoptPR(7, "aaa", append(labels, "adoption/supersede")...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 || len(h.sink.abandons) != 1 {
		t.Fatalf("pulls=%v abandons=%+v", h.adopt.pulls, h.sink.abandons)
	}
	if a := h.sink.abandons[0]; a.AdoptedIntoPR != 90 || a.Reason != AbandonReasonOperatorTakeover {
		t.Fatalf("operator takeover abandon = %+v", a)
	}
}

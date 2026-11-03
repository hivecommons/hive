package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Adopt now (hivecommons/hive#15038): the operator's forced mark on the
// stage record makes the next pass adopt at once, by supersession, with the
// author's relay online, inside the grace window and under
// maintainer_policy: never — the three things that otherwise hold adoption.
// The marker names the operator (by=@login reason=operator-forced) and the
// note tells an online author the maintainer took over.
func TestAdoptNowBypassesPresenceGraceAndMaintainerPolicy(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, maintainerPolicyWith(config.AdoptionMaintainerPolicyNever))
	defer done()
	h.adopt.permissions = map[string]string{"mallory": "admin"}
	h.adopt.allowMaintainerEdits(7, true)
	h.sink.presence = &ContributorPresence{Registered: true, Online: true}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	// Five minutes in, online, maintainer, never: the engine would not touch it.
	h.api.reset()
	h.clock.advance(5 * time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 {
		t.Fatalf("control: adopted without the forced mark: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}

	// The operator clicks Adopt now: the dashboard notes the forced mark and
	// nudges the sweep (adoption/supersede rides along as the request label).
	store := h.e.stageStoreOrShared()
	store.NoteAdoptionForced(key, "clubanderson", h.clock.now())
	h.api.reset()
	h.clock.advance(time.Second)
	res := h.observe(adoptPR(7, "aaa", append(labels, "adoption/supersede")...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("Adopt now did not supersede: pulls=%v added=%v", h.adopt.pulls, h.api.added)
	}
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "aaa" {
		t.Fatalf("adopt branch sha=%q (the author's commits must be carried unchanged)", got)
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 {
		t.Fatalf("adoption comments = %v", h.api.comments)
	}
	for _, want := range []string{
		"by=@clubanderson reason=operator-forced mode=supersede superseded_by=90 head=aaa",
		"@mallory", "a maintainer asked the hive to take it over now rather than wait for your hive, which is online",
		"carries your commits (and sign-off) unchanged", "closed automatically once #90 merges",
	} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("forced adoption comment missing %q:\n%s", want, comments[0])
		}
	}
	if res.Comments != 1 {
		t.Fatalf("res=%+v", res)
	}
	rec, _ := store.Get(key)
	if rec.AdoptedHead != "aaa" || rec.AdoptedBy != 90 || rec.AdoptReason != AdoptReasonOperatorForced {
		t.Fatalf("record after Adopt now: %+v", rec)
	}
	if _, forced := rec.AdoptionForced(); forced {
		t.Fatalf("the forced mark must be consumed by the adoption: %+v", rec)
	}
	if rec.AdoptionBlocked("aaa") != "" {
		t.Fatalf("a maintainer-policy block survived the operator's decision: %+v", rec)
	}

	// Later passes keep it: never's release rule yields to the operator.
	h.api.reset()
	h.clock.advance(maintainerPermTTL + time.Minute)
	h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker", "adoption/supersede"), "check-failing:unit")
	if rec, _ = store.Get(key); rec.AdoptedHead != "aaa" || rec.AdoptedBy != 90 {
		t.Fatalf("operator-forced adoption released under never: %+v", rec)
	}
}

// An offline author gets the offline wording; a hold label still stops it.
func TestAdoptNowOfflineWordingAndHoldStops(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	store := h.e.stageStoreOrShared()
	store.NoteAdoptionForced(key, "clubanderson", h.clock.now())

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", append(labels, "hold")...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatalf("hold did not stop Adopt now: pulls=%v", h.adopt.pulls)
	}
	if rec, _ := store.Get(key); rec.AdoptForcedBy != "clubanderson" {
		t.Fatalf("the forced mark must survive a held pass: %+v", rec)
	}

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	comments := adoptionComments(h.api)
	if len(h.adopt.pulls) != 1 || len(comments) != 1 {
		t.Fatalf("Adopt now after the hold lifted: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	if !strings.Contains(comments[0], "reason=operator-forced") || !strings.Contains(comments[0], "rather than wait out the offline grace window") {
		t.Fatalf("offline wording: %s", comments[0])
	}
}

package automerge

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// GitHub reports mergeable_state "unknown" while it recomputes a merge (it
// does after the base moves). The contributor lane stops there, before
// admission, review or CI, and records prstage.StageReasonMergeableUnknown.
// Read as "approved, nothing left for the author" that pass was a weak 080:
// projectbluefin/utah-packages#1222 (no linked issue, approved, green, head
// unchanged all night) went 090 → 080 → 090 twice in one evening. The pass
// now has no stage opinion, so the stage the evaluated passes set stands.

// mergeableUnknownApproved is the reviewer verdict the tracker holds for the
// head on every pass, unknown ones included.
func mergeableUnknownApproved() ContributorLaneVerdict {
	return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
}

// (a) A person's merge (no linked issue) at 090 stays 090 across unknown
// passes, with nothing written.
func TestMergeableUnknownPassKeepsHumanMerge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)}
	verdict := mergeableUnknownApproved()
	e, store := humanStageEngine(t, api.url, clock, &verdict, time.Hour)
	const number = 1222
	labels := []string{"role/contributor", "author/human"}
	pass := func(reason string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(number, labels...), reason, false, nil)
		e.noteStageVerdict("acme", "widget", number, mergeableUnknownApproved())
		if reason != prstage.StageReasonMergeableUnknown {
			// The lane notes the tier only once it reaches admission.
			e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		labels = stageLabelsAfter(labels, api)
	}
	pass(contributorLaneReasonNoLinkedIssue)
	clock.at = clock.at.Add(time.Hour) // past the soak
	pass(contributorLaneReasonNoLinkedIssue)
	if got := stageOf(t, store, number); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("settled stage = %v, want 090", got)
	}
	for i := 0; i < 6; i++ {
		reason := contributorLaneReasonNoLinkedIssue
		if i%2 == 0 {
			reason = prstage.StageReasonMergeableUnknown
		}
		clock.at = clock.at.Add(70 * time.Second)
		pass(reason)
		if got := stageOf(t, store, number); got != stagelabels.ReadyForHumanMerge || api.writes() != 0 {
			t.Fatalf("pass %d (%s): stage %v, added=%v removed=%v comments=%v", i+3, reason, got, api.added, api.removed, api.comments)
		}
	}
	if got := stageLabelOf(labels); got != stagelabels.ReadyForHumanMerge.Label() {
		t.Fatalf("label = %q, want %s", got, stagelabels.ReadyForHumanMerge.Label())
	}
}

// (b) A conflicting PR the lane would also refuse (the trusted-author lane
// says untrusted-author-role) sits at 072. An unknown pass hides the
// conflict from the contributor lane; the trust lane's weak 090 candidate
// must not stand in for it.
func TestMergeableUnknownPassKeepsConflictReturn(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, apiURLOf(t, api), clock)
	const number = 1223
	var labels []string
	pass := func(reason string) {
		t.Helper()
		api.reset()
		pr := nudgePR(number, "sha-conflict", labels...)
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageVerdict("acme", "widget", number, mergeableUnknownApproved())
		if reason != prstage.StageReasonMergeableUnknown {
			e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		labels = stageLabelsAfter(labels, api)
		clock.at = clock.at.Add(70 * time.Second)
	}
	pass("conflicting")
	settled := stageLabelOf(labels)
	if settled != stagelabels.ReturnedToContributor.Label() {
		t.Fatalf("conflicting pass wrote %q (labels %v), want %s", settled, labels, stagelabels.ReturnedToContributor.Label())
	}
	for i := 0; i < 6; i++ {
		reason := "conflicting"
		if i%2 == 0 {
			reason = prstage.StageReasonMergeableUnknown
		}
		pass(reason)
		if got := stageLabelOf(labels); got != settled || api.writes() != 0 {
			t.Fatalf("pass %d (%s): stage %s → %s, added=%v removed=%v", i+2, reason, settled, got, api.added, api.removed)
		}
	}
	if rec, ok := store.Get(stageKey("acme", "widget", number)); !ok || rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != "conflicting" {
		t.Fatalf("record = %+v, want 072 conflicting", rec)
	}
}

// (c) An admitted, approved PR at 095 keeps its stage across unknown passes
// instead of dropping to 080 for one.
func TestMergeableUnknownPassKeepsAdmittedStage(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 19, 21, 0, 0, 0, time.UTC)}
	e, _, _ := newStageEngine(t, apiURLOf(t, api), clock)
	const number = 1224
	labels := []string{"role/trusted", "author/human"}
	pass := func(reason string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", nudgePR(number, "sha-admitted", labels...), reason, false, nil)
		e.noteStageVerdict("acme", "widget", number, mergeableUnknownApproved())
		if reason != prstage.StageReasonMergeableUnknown {
			e.noteStageRole("acme", "widget", number, stagelabels.RoleTrusted)
		}
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		labels = stageLabelsAfter(labels, api)
		clock.at = clock.at.Add(70 * time.Second)
	}
	pass(contributorLaneReasonSoaking)
	settled := stageLabelOf(labels)
	if settled != stagelabels.PreMergeSoak.Label() {
		t.Fatalf("soaking pass wrote %q (labels %v), want %s", settled, labels, stagelabels.PreMergeSoak.Label())
	}
	for i := 0; i < 6; i++ {
		reason := contributorLaneReasonSoaking
		if i%2 == 0 {
			reason = prstage.StageReasonMergeableUnknown
		}
		pass(reason)
		if got := stageLabelOf(labels); got != settled || api.writes() != 0 {
			t.Fatalf("pass %d (%s): stage %s → %s, added=%v removed=%v", i+2, reason, settled, got, api.added, api.removed)
		}
	}
}

// An unknown pass evaluated nothing, so it keeps the lane's last
// merge-train verdict on the head: clearing #1222's no-linked-issue verdict
// let the train seat a person's merge until the next evaluated pass.
func TestMergeTrainUnknownOutcomeKeepsHumanMergeVerdict(t *testing.T) {
	c := &Engine{}
	c.noteMergeTrainOutcome(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf", prstage.ContributorLaneReasonNoLinkedIssue)
	c.noteMergeTrainOutcome(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf", prstage.StageReasonMergeableUnknown)
	if v, ok := c.mergeTrainVerdictFor(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf"); !ok || !v.humanMerge || v.reason != prstage.ContributorLaneReasonNoLinkedIssue {
		t.Fatalf("verdict after an unknown pass = %+v, %v; want the no-linked-issue human merge kept", v, ok)
	}
	if !c.mergeTrainHeadBlocked(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf") {
		t.Fatal("a person's merge must stay out of the train across an unknown pass")
	}
	// An unknown pass on a head the lane never judged records nothing.
	c.noteMergeTrainOutcome(stageLaneContributor, "acme", "widget", 1225, "abc", prstage.StageReasonMergeableUnknown)
	if v, ok := c.mergeTrainVerdictFor(stageLaneContributor, "acme", "widget", 1225, "abc"); ok {
		t.Fatalf("unknown pass recorded %+v", v)
	}
	// An evaluated pass still replaces the verdict.
	c.noteMergeTrainOutcome(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf", "")
	if v, ok := c.mergeTrainVerdictFor(stageLaneContributor, "acme", "widget", 1222, "1bb4e6bf"); !ok || v.humanMerge || v.readiness != mergeTrainReadinessReady {
		t.Fatalf("evaluated pass verdict = %+v, %v; want ready", v, ok)
	}
}

package automerge

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// nudgeLadderPatterns are the four nudge-then-adopt pairs of
// hivecommons/hive#18872 as the stage-072 observation the nudge sender
// sees: the lane reason the nudge named, plus the red checks routed beside
// it where the head carried them.
func nudgeLadderPatterns() []struct {
	name string
	obs  stageObservation
} {
	obs := func(number int, reason string, routed ...string) stageObservation {
		o := stageObservation{
			Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: number,
			HeadSHA: "aaa", Reason: reason, Author: "mallory", AuthorKind: authorkind.Clanker, BaseRef: "v5",
		}
		if len(routed) > 0 {
			o.Route = &stagelabels.Route{Reasons: routed}
		}
		return o
	}
	return []struct {
		name string
		obs  stageObservation
	}{
		{"contributor-lane", obs(18561, contributorLaneReasonChangesRequested, "red checks: unit")},
		{"fork-not-editable", obs(18628, autoMergeReasonForkNotEditable, "red checks: unit")},
		{"check-failing", obs(18514, "check-failing:unit")},
		{"conflicting", obs(18510, "conflicting")},
	}
}

// Invariant 1 (hivecommons/hive#18872): a head the contributor ladder
// supersedes at once gets no nudge. For each pattern the sender's verdict
// is the ladder's own, and it withholds only where the adoption engine
// really follows through.
func TestLadderSupersedesNudgeHead18872(t *testing.T) {
	offline := ContributorPresence{Registered: true}
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, Adoption: true}
	for _, p := range nudgeLadderPatterns() {
		t.Run(p.name, func(t *testing.T) {
			checks, _, _ := nudgeDiagnosisRouted(p.obs.Reason, p.obs.Detail, p.obs.Route)
			rec := stagelabels.Record{}
			if !ladderSupersedesNudgeHead(rec, p.obs, policy, checks, offline, true) {
				t.Fatalf("offline relay, %s head: nudge not withheld", p.name)
			}
			if !delegatedecision.SupersedesImmediately(nudgeLadderInput(rec, p.obs, checks)) {
				t.Fatalf("the ladder does not supersede the %s head the sender withholds", p.name)
			}
			for name, tc := range map[string]struct {
				rec         stagelabels.Record
				obs         stageObservation
				policy      StageLabelPolicy
				presence    ContributorPresence
				hasPresence bool
			}{
				"relay connected":        {obs: p.obs, policy: policy, presence: ContributorPresence{Registered: true, Online: true}, hasPresence: true},
				"no relay registered":    {obs: p.obs, policy: policy, hasPresence: true},
				"no presence source":     {obs: p.obs, policy: policy},
				"adoption off":           {obs: p.obs, policy: StageLabelPolicy{Enabled: true, ContributorNudge: true}, presence: offline, hasPresence: true},
				"adoption held by label": {obs: withLabels(p.obs, "hold"), policy: policy, presence: offline, hasPresence: true},
				"adoption blocked":       {rec: stagelabels.Record{AdoptBlockedHead: "aaa", AdoptBlockedBy: stagelabels.AdoptBlockedAuthorMaintainer}, obs: p.obs, policy: policy, presence: offline, hasPresence: true},
				"author pushed":          {rec: stagelabels.Record{NudgeHead: "zzz", NudgeCount: 1}, obs: p.obs, policy: policy, presence: offline, hasPresence: true},
			} {
				if ladderSupersedesNudgeHead(tc.rec, tc.obs, tc.policy, checks, tc.presence, tc.hasPresence) {
					t.Errorf("%s, %s head: nudge withheld, want it sent", name, p.name)
				}
			}
		})
	}
	// Without a red check, changes requested and a fork Hive cannot update
	// are mergeable as far as the ladder knows: nudged, never superseded at once.
	for _, reason := range []string{contributorLaneReasonChangesRequested, autoMergeReasonForkNotEditable} {
		o := nudgeLadderPatterns()[0].obs
		o.Reason, o.Route = reason, nil
		checks, _, _ := nudgeDiagnosisRouted(o.Reason, o.Detail, o.Route)
		if ladderSupersedesNudgeHead(stagelabels.Record{}, o, policy, checks, offline, true) {
			t.Errorf("%s with green checks: nudge withheld", reason)
		}
	}
}

func withLabels(o stageObservation, labels ...string) stageObservation {
	o.Labels = append(append([]string(nil), o.Labels...), labels...)
	return o
}

// Invariant 2 (hivecommons/hive#18872): a head the hive did nudge is not
// superseded before the shortest adoption wait. NudgeGracePending is what
// the dashboard hands the ladder; a withheld nudge gives no grace (the
// ladder's immediate supersede is the plan for that head).
func TestNudgeGracePending18872(t *testing.T) {
	clock := StageLabelPolicy{}.AdoptionClock()
	nudgedAt := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	rec := stagelabels.Record{NudgeHead: "aaa", NudgeCount: 1, FirstNudgeAt: nudgedAt, LastNudgeAt: nudgedAt}
	for _, p := range nudgeLadderPatterns() {
		t.Run(p.name, func(t *testing.T) {
			if !NudgeGracePending(rec, p.obs.HeadSHA, clock, nudgedAt.Add(time.Minute)) {
				t.Fatal("one minute after the nudge: grace not pending")
			}
			checks, _, _ := nudgeDiagnosisRouted(p.obs.Reason, p.obs.Detail, p.obs.Route)
			in := nudgeLadderInput(stagelabels.Record{}, p.obs, checks)
			in.NudgeGracePending = true
			if d := delegatedecision.Decide(in); d.Verdict == delegatedecision.VerdictAdoptSupersede || d.Rule != delegatedecision.RuleNudgeGrace {
				t.Fatalf("nudged %s head inside the grace decided %s/%s", p.name, d.Verdict, d.Rule)
			}
			if NudgeGracePending(rec, p.obs.HeadSHA, clock, nudgedAt.Add(clock.minimumWait())) {
				t.Fatal("grace still pending once the shortest wait ran out")
			}
			if NudgeGracePending(rec, "bbb", clock, nudgedAt.Add(time.Minute)) {
				t.Fatal("grace pending for a head that was never nudged")
			}
			withheld := rec
			withheld.NudgeWithheldHead = "aaa"
			if NudgeGracePending(withheld, p.obs.HeadSHA, clock, nudgedAt.Add(time.Minute)) {
				t.Fatal("a withheld nudge gave the head a grace")
			}
		})
	}
}

// The sweep end to end for the check-failing and conflicting patterns: an
// offline relay's head gets no nudge comment, the relay still gets the
// task, and the adoption note 15 minutes later is the only comment. A
// connected relay is nudged and keeps its grace.
func TestNudgeWithheldThenOneAdoptionNote18872(t *testing.T) {
	for _, reason := range []string{"check-failing:unit", "conflicting"} {
		t.Run(reason, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "queued"}, adoptPolicy())
			defer done()
			h.sink.presence = &ContributorPresence{Registered: true}
			h.observe(adoptPR(7, "aaa"), reason)
			if len(nudgeComments(h.api)) != 0 || len(h.sink.nudges) != 1 {
				t.Fatalf("first sweep: comments=%v sink=%d", h.api.comments, len(h.sink.nudges))
			}
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if !rec.NudgeWithheld("aaa") || rec.NudgeHead != "aaa" || rec.NudgeCount != 1 {
				t.Fatalf("withheld nudge not recorded: %+v", rec)
			}
			labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
			// The next sweeps neither nudge nor re-dispatch.
			h.api.reset()
			h.clock.advance(5 * time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), reason)
			if len(nudgeComments(h.api)) != 0 || len(h.sink.nudges) != 1 || len(h.adopt.pulls) != 0 {
				t.Fatalf("second sweep: comments=%v sink=%d pulls=%d", h.api.comments, len(h.sink.nudges), len(h.adopt.pulls))
			}
			h.api.reset()
			h.clock.advance(11 * time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), reason)
			if c := adoptionComments(h.api); len(c) != 1 || len(nudgeComments(h.api)) != 0 {
				t.Fatalf("adoption: want the one adoption note, got %v", h.api.comments)
			}
		})
		t.Run(reason+"/online", func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "spawn", Online: true}, adoptPolicy())
			defer done()
			h.observe(adoptPR(7, "aaa"), reason)
			if len(nudgeComments(h.api)) != 1 {
				t.Fatalf("connected relay not nudged: %v", h.api.comments)
			}
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.NudgeWithheld("aaa") || !NudgeGracePending(rec, "aaa", adoptPolicy().AdoptionClock(), h.clock.at.Add(time.Minute)) {
				t.Fatalf("nudged head has no grace: %+v", rec)
			}
		})
	}
}

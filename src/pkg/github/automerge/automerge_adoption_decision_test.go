package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A maintainer's decision on an adopted head parked at 085 is the answer to
// that escalation: the stage pass must not re-derive the same stall from the
// same head two minutes later (projectbluefin/utah-packages#1308, a fork the
// hive would not re-home, and #1309, adopted in place with a 16h-old head).
// Dwells are read from the policy and stage table, never assumed.

// recordAdoptionDecision stores a person's `/hive decision <direction>` on
// PR 7's head at the harness clock, as the dashboard or the un-park sweep
// records it.
func recordAdoptionDecision(t *testing.T, h *adoptHarness, head string) time.Time {
	t.Helper()
	at := h.clock.now()
	if err := h.e.humanChangeRequests.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: head, Source: review.HumanChangeRequestSourceDecision, SourceID: 4242, Actor: "octo", Body: "make CI green, nothing else", RecordedAt: at}); err != nil {
		t.Fatal(err)
	}
	return at
}

func decisionLapsedComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, AdoptionDecisionLapsedMarker) {
			out = append(out, c)
		}
	}
	return out
}

func TestAdoptionStalledReasonAfterDecision(t *testing.T) {
	policy := adoptPolicy()
	now := time.Date(2026, 11, 25, 20, 13, 0, 0, time.UTC)
	dwell072 := stagelabels.Dwell(stagelabels.ReturnedToContributor, policy.Dwell)
	dwell074 := stagelabels.Dwell(stagelabels.AdoptedByHive, policy.Dwell)
	headSince := now.Add(-16 * time.Hour)
	forkRefused := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptedAt: headSince, AdoptedHeadAt: headSince, ForkRehomeRefusedHead: "aaa"}
	inPlace := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeInPlace, AdoptedAt: headSince, AdoptedHeadAt: headSince}
	red := stageObservation{HeadSHA: "aaa", Reason: "check-failing:Canary", LaneReason: "check-failing:Canary"}
	changes := stageObservation{HeadSHA: "aaa", Reason: autoMergeReasonHumanChangesRequested, LaneReason: autoMergeReasonHumanChangesRequested}
	forkStall := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused
	changesStall := stagelabels.LaneReasonAdoptionStalledPrefix + autoMergeReasonHumanChangesRequested
	for _, tc := range []struct {
		name    string
		obs     stageObservation
		rec     stagelabels.Record
		decided time.Time
		want    string
	}{
		{"fork refused, no decision", red, forkRefused, time.Time{}, forkStall},
		{"fork refused, decided a minute ago", red, forkRefused, now.Add(-time.Minute), ""},
		{"fork refused, inside the 072 dwell from the decision", red, forkRefused, now.Add(-dwell072 + time.Minute), ""},
		{"fork refused, 072 dwell from the decision spent", red, forkRefused, now.Add(-dwell072), forkStall},
		{"in place 16h, no decision", changes, inPlace, time.Time{}, changesStall},
		{"in place 16h, decided a minute ago", changes, inPlace, now.Add(-time.Minute), ""},
		{"in place, 074 dwell from the decision spent", changes, inPlace, now.Add(-dwell074), changesStall},
		{"decision older than the head clock changes nothing", red, inPlace, now.Add(-20 * time.Hour), stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:Canary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := adoptionStalledReasonWith(tc.obs, tc.rec, policy, now, false, tc.decided); got != tc.want {
				t.Fatalf("adoptionStalledReasonWith=%q, want %q", got, tc.want)
			}
		})
	}
	// The wrapper every other caller uses carries no decision: unchanged.
	if got := adoptionStalledReason(red, forkRefused, policy, now); got != forkStall {
		t.Fatalf("adoptionStalledReason=%q, want %q", got, forkStall)
	}
}

// Only a return recorded on exactly this head after the hive took the PR
// over counts: a GitHub review is not one, a bare approval keeps today's
// clock, and a direction older than the adoption was answered by it.
func TestAdoptionDecisionOnHead(t *testing.T) {
	at := time.Date(2026, 11, 25, 20, 13, 0, 0, time.UTC)
	store := review.NewHumanChangeRequests("")
	for _, req := range []review.HumanChangeRequest{
		{Repo: "acme/widget", Number: 7, HeadSHA: "aaa", Source: review.HumanChangeRequestSourceDecision, SourceID: 1, Actor: "octo", Body: "make CI green", RecordedAt: at},
		{Repo: "acme/widget", Number: 8, HeadSHA: "ccc", Source: review.HumanChangeRequestSourceReview, SourceID: 2, Actor: "octo", Body: "please fix", RecordedAt: at},
		{Repo: "acme/widget", Number: 9, HeadSHA: "ddd", Source: review.HumanChangeRequestSourceDecision, SourceID: 3, Actor: "octo", Body: "approve", Kind: review.HumanChangeRequestKindApprove, RecordedAt: at},
		{Repo: "acme/widget", Number: 10, HeadSHA: "eee", Source: review.HumanChangeRequestSourceStageLabel, SourceID: 4, Actor: "octo", RecordedAt: at},
	} {
		if err := store.Record(req); err != nil {
			t.Fatal(err)
		}
	}
	c := &Engine{humanChangeRequests: store}
	obs := func(number int, head string) stageObservation {
		return stageObservation{Owner: "acme", Repo: "widget", Number: number, HeadSHA: head}
	}
	adoptedBefore := stagelabels.Record{AdoptedHead: "aaa", AdoptedAt: at.Add(-time.Hour)}
	adoptedAfter := stagelabels.Record{AdoptedHead: "aaa", AdoptedAt: at.Add(time.Minute)}
	for _, tc := range []struct {
		name string
		obs  stageObservation
		rec  stagelabels.Record
		want bool
	}{
		{"return on the head after the adoption", obs(7, "aaa"), adoptedBefore, true},
		{"no adoption on record", obs(7, "aaa"), stagelabels.Record{}, true},
		{"return older than the adoption", obs(7, "aaa"), adoptedAfter, false},
		{"new head after the decision", obs(7, "bbb"), adoptedBefore, false},
		{"review is not a decision", obs(8, "ccc"), adoptedBefore, false},
		{"approval is not a return", obs(9, "ddd"), adoptedBefore, false},
		{"hand-placed return label", obs(10, "eee"), adoptedBefore, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := c.adoptionDecisionOnHead(tc.obs, tc.rec)
			if ok != tc.want || (ok && !req.RecordedAt.Equal(at)) || (!ok && !req.RecordedAt.IsZero()) {
				t.Fatalf("adoptionDecisionOnHead=(%+v, %v), want ok=%v at %v", req, ok, tc.want, at)
			}
		})
	}
	if _, ok := (&Engine{}).adoptionDecisionOnHead(obs(7, "aaa"), adoptedBefore); ok {
		t.Fatal("no store must read as no decision")
	}
}

// projectbluefin/utah-packages#1308: the hive adopted a fork PR it could not
// re-home and parked it at 085. A maintainer sent it back to its author: it
// stays at 072 on later passes while the head stands, whatever the lane
// reads, the refusal is kept, and only the 072 dwell measured from the
// decision escalates it again, with one note saying the decision lapsed.
func TestAdoptionDecisionKeepsForkRehomeRefusedHeadWithAuthor(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	h.adopt.allowMaintainerEdits(7, false)
	h.observe(forkPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	store := h.e.stageStoreOrShared()
	h.clock.advance(time.Minute)
	h.observe(forkPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	h.api.reset()
	parked := forkPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker")
	assertForkRehomeParked(t, h, parked, "check-failing:unit")
	want := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused

	// No decision yet: the parked PR stays silent on later passes.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(forkPR(7, "aaa", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), "check-failing:unit")
	if len(stalledComments(h.api)) != 0 || len(decisionLapsedComments(h.api)) != 0 {
		t.Fatalf("parked PR without a decision posted: %v", h.api.comments)
	}

	// The decision lands: the un-park wrote 072 and every pass keeps it,
	// including one whose lane reads a stage other than 072.
	decided := recordAdoptionDecision(t, h, "aaa")
	dwell := stagelabels.Dwell(stagelabels.ReturnedToContributor, p.Dwell)
	nudgesBefore := len(h.sink.nudges)
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	for _, step := range []struct {
		wait   time.Duration
		reason string
	}{
		{2 * time.Minute, "check-failing:unit"},
		{time.Minute, "check-pending"},
		{time.Minute, "check-failing:unit"},
		{time.Minute, contributorLaneReasonNoVerdict},
		{time.Minute, "check-pending"},
		{dwell / 2, "check-failing:unit"},
	} {
		h.api.reset()
		h.clock.advance(step.wait)
		h.observe(forkPR(7, "aaa", returned...), step.reason)
		rec, _ := store.Get(key)
		if rec.Stage != stagelabels.ReturnedToContributor || rec.AdoptionEscalated("aaa") || !rec.ForkRehomeRefused("aaa") {
			t.Fatalf("%s after %v: decision undone, record %+v", step.reason, h.clock.at.Sub(decided), rec)
		}
		if hasStageLabel(h.addedLabels(), stagelabels.NeedsDecision.Label()) || hasStageLabel(h.addedLabels(), stagelabels.AdoptedByHive.Label()) || len(h.api.removed) != 0 || len(stalledComments(h.api)) != 0 {
			t.Fatalf("%s: re-staged: added=%v removed=%v comments=%v", step.reason, h.api.added, h.api.removed, h.api.comments)
		}
		if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
			t.Fatalf("%s: fork re-homed: pulls=%v refs=%v", step.reason, h.adopt.pulls, h.adopt.refs)
		}
		if rec.Reason != autoMergeReasonHumanChangesRequested {
			t.Fatalf("%s: hold reason %q, want the decision's %q", step.reason, rec.Reason, autoMergeReasonHumanChangesRequested)
		}
		if step.wait == time.Minute && len(h.sink.nudges) > nudgesBefore+1 {
			// The lane's reason flipped every pass while CI reran; the
			// nudge's class is the decision's, so at most the one nudge
			// for the new class went out.
			t.Fatalf("%s: nudge churned with the lane reason: %d nudges", step.reason, len(h.sink.nudges)-nudgesBefore)
		}
	}

	// The 072 dwell from the decision passes on the same head: a person's
	// again, with one note that the decision lapsed and no repeat of the
	// stall note it already read.
	h.api.reset()
	h.clock.at = decided.Add(dwell + time.Minute)
	h.observe(forkPR(7, "aaa", returned...), "check-failing:unit")
	rec, _ := store.Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != want || !rec.AdoptionEscalated("aaa") {
		t.Fatalf("072 dwell from the decision spent: record %+v, want 085 on %s", rec, want)
	}
	lapsed := decisionLapsedComments(h.api)
	if len(lapsed) != 1 || len(stalledComments(h.api)) != 0 {
		t.Fatalf("want one lapse note and no repeated stall note: %v", h.api.comments)
	}
	for _, s := range []string{"head=aaa reason=" + want, "octo", "had its full " + formatSat(dwell+time.Minute) + " with no new commit", "`" + stagelabels.NeedsDecision.Label() + "`"} {
		if !strings.Contains(lapsed[0], s) {
			t.Fatalf("lapse note missing %q:\n%s", s, lapsed[0])
		}
	}
	if strings.Contains(lapsed[0], "@") {
		t.Fatalf("lapse note mentions the person:\n%s", lapsed[0])
	}
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("fork re-homed after the dwell: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}

	// Once per decision: the next pass on the same head is silent.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(forkPR(7, "aaa", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), "check-failing:unit")
	if rec, _ = store.Get(key); rec.Stage != stagelabels.NeedsDecision || len(decisionLapsedComments(h.api)) != 0 || len(stalledComments(h.api)) != 0 {
		t.Fatalf("lapse noted twice or released: record %+v comments=%v", rec, h.api.comments)
	}
}

// A direction given before the maintainer asked the hive to adopt the PR was
// answered by the adoption: when the supersede is refused for the fork, the
// PR goes to 085 with the maintainer ask as it did before.
func TestAdoptionDecisionBeforeAdoptionDoesNotHoldForkRefusal(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	h.adopt.allowMaintainerEdits(7, false)
	h.observe(forkPR(7, "aaa"), "check-failing:unit")
	recordAdoptionDecision(t, h, "aaa")
	h.clock.advance(time.Minute)
	h.observe(forkPR(7, "aaa", "stage/072-returned-to-contributor", "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if !rec.Adopted("aaa") || !rec.ForkRehomeRefused("aaa") {
		t.Fatalf("delegated fork adoption not recorded: %+v", rec)
	}
	h.api.reset()
	assertForkRehomeParked(t, h, forkPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
}

// projectbluefin/utah-packages#1309: adopted in place, head pushed 16h ago,
// parked for a stall. The maintainer's direction is the hive's fixer's to
// carry out (074); its own changes request is not the stall, and the 074
// dwell runs from the decision. A new head runs the stall logic as before.
func TestAdoptionDecisionOnInPlaceHeadRestartsStallClock(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	key := adoptInPlaceAt(h, "aaa", "check-failing:unit")
	store := h.e.stageStoreOrShared()
	dwell := stagelabels.Dwell(stagelabels.AdoptedByHive, p.Dwell)
	parked := []string{stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"}
	changesStall := stagelabels.LaneReasonAdoptionStalledPrefix + autoMergeReasonHumanChangesRequested

	// No decision: the 16h-old head escalates as today.
	h.clock.advance(16 * time.Hour)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-failing:unit")
	if rec, _ := store.Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") {
		t.Fatalf("no decision: record %+v, want the 085 escalation", rec)
	}

	decided := recordAdoptionDecision(t, h, "aaa")
	h.api.reset()
	h.clock.advance(2 * time.Minute)
	h.observe(adoptPR(7, "aaa", parked...), autoMergeReasonHumanChangesRequested)
	rec, _ := store.Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("aaa") || rec.Reason == changesStall {
		t.Fatalf("decision re-parked as its own stall: record %+v", rec)
	}
	if hasStageLabel(h.addedLabels(), stagelabels.NeedsDecision.Label()) || len(stalledComments(h.api)) != 0 {
		t.Fatalf("decision re-parked: added=%v comments=%v", h.api.added, h.api.comments)
	}

	h.api.reset()
	h.clock.at = decided.Add(dwell - time.Minute)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), autoMergeReasonHumanChangesRequested)
	if rec, _ = store.Get(key); rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("aaa") {
		t.Fatalf("inside the 074 dwell from the decision: record %+v", rec)
	}

	// The 074 dwell from the decision passes with nothing pushed: back to a
	// person with exactly one comment over this pass and the next, saying
	// whose decision lapsed, whether the stall reason is new to the head
	// (one stall note carrying the lapse) or already noted (the lapse note).
	h.api.reset()
	h.clock.at = decided.Add(dwell + time.Minute)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), autoMergeReasonHumanChangesRequested)
	if rec, _ = store.Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") {
		t.Fatalf("074 dwell from the decision spent: record %+v, want 085", rec)
	}
	notes := append(stalledComments(h.api), decisionLapsedComments(h.api)...)
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", parked...), autoMergeReasonHumanChangesRequested)
	notes = append(notes, stalledComments(h.api)...)
	notes = append(notes, decisionLapsedComments(h.api)...)
	if len(notes) != 1 || !strings.Contains(notes[0], AdoptionDecisionLapsedMarker) || !strings.Contains(notes[0], "octo") {
		t.Fatalf("want one comment naming the lapsed decision over two passes, got %d: %v", len(notes), notes)
	}
	if rec, _ = store.Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionDecisionLapseNoted("aaa", decided) {
		t.Fatalf("lapse not recorded: %+v", rec)
	}

	// The hive pushes a new head: the decision on aaa does not carry, the
	// clock restarts at the push and the stall runs as before.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", parked...), "check-pending")
	if rec, _ = store.Get(key); rec.Stage != stagelabels.AdoptedByHive || rec.AdoptedHead != "bbb" || rec.AdoptionEscalated("bbb") {
		t.Fatalf("new hive head: %+v", rec)
	}
	h.api.reset()
	h.clock.advance(dwell + time.Minute)
	h.observe(adoptPR(7, "bbb", adoptedLabels...), "check-failing:unit")
	rec, _ = store.Get(key)
	if want := stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:unit"; rec.Stage != stagelabels.NeedsDecision || rec.Reason != want || !rec.AdoptionEscalated("bbb") {
		t.Fatalf("new head past the dwell: record %+v, want 085 on %s", rec, want)
	}
	if len(decisionLapsedComments(h.api)) != 0 {
		t.Fatalf("a decision on another head noted as lapsed: %v", h.api.comments)
	}
}

// An old in-place head with a blocking review verdict would re-home on the
// pass after the maintainer's direction; the direction gives the hive's
// fixer the 074 dwell first, measured from the decision.
func TestAdoptionDecisionDefersInPlaceRehome(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	key := adoptInPlaceAt(h, "5665cda", contributorLaneReasonNoVerdict)
	store := h.e.stageStoreOrShared()
	dwell := stagelabels.Dwell(stagelabels.AdoptedByHive, p.Dwell)
	finding := func() {
		h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{
			Configured: true, Verdict: "changes_requested",
			HumanSummaries: []string{"rewritten source citations do not match the head tree"},
		})
	}

	h.clock.advance(16 * time.Hour)
	decided := recordAdoptionDecision(t, h, "5665cda")
	h.clock.advance(2 * time.Minute)
	finding()
	h.observe(adoptPR(7, "5665cda", adoptedLabels...), contributorLaneReasonChangesRequested)
	if len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("re-homed right after the decision: pulls=%v refs=%v", h.adopt.pulls, h.adopt.refs)
	}
	if rec, _ := store.Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("after the decision: %+v", rec)
	}

	// The fixer had its dwell from the decision: the re-home runs as before.
	h.api.reset()
	h.clock.at = decided.Add(dwell + time.Minute)
	finding()
	h.observe(adoptPR(7, "5665cda", adoptedLabels...), contributorLaneReasonChangesRequested)
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("re-home after the dwell from the decision: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
}

// A workflow-path refusal is a verdict on the head itself: a decision on
// that head does not release it, as before.
func TestAdoptionDecisionDoesNotReleaseWorkflowRefusal(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	key := adoptInPlaceAt(h, "aaa", "check-failing:unit")
	store := h.e.stageStoreOrShared()
	store.MarkAdoptionEscalated(key, "aaa", adoptionWorkflowRefusedReason)
	store.MarkAdoptionEscalationNoted(key, "aaa", adoptionWorkflowRefusedReason)
	h.clock.advance(time.Minute)
	recordAdoptionDecision(t, h, "aaa")

	h.api.reset()
	h.clock.advance(2 * time.Minute)
	h.observe(adoptPR(7, "aaa", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), autoMergeReasonHumanChangesRequested)
	rec, _ := store.Get(key)
	if rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") || rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason {
		t.Fatalf("workflow-path refusal released by a decision: record %+v", rec)
	}
	if len(workflowRefusedComments(h.api)) != 0 || len(decisionLapsedComments(h.api)) != 0 || len(h.adopt.pulls) != 0 {
		t.Fatalf("refusal re-noted or re-homed: comments=%v pulls=%v", h.api.comments, h.adopt.pulls)
	}
}

func TestAdoptionDecisionHolds(t *testing.T) {
	now := time.Date(2026, 11, 25, 20, 13, 0, 0, time.UTC)
	policy := adoptPolicy()
	dwell := stagelabels.Dwell(stagelabels.ReturnedToContributor, policy.Dwell)
	if adoptionDecisionHolds(time.Time{}, policy, now) {
		t.Fatal("no decision held")
	}
	if !adoptionDecisionHolds(now.Add(-time.Hour), policy, now) {
		t.Fatal("fresh decision did not hold")
	}
	if adoptionDecisionHolds(now.Add(-dwell), policy, now) {
		t.Fatal("decision held past the 072 dwell")
	}
	// A zero 072 dwell never stalls, as escalateUnadoptableFork reads it.
	policy.Dwell = map[string]time.Duration{stagelabels.ReturnedToContributor.Slug(): 0}
	if !adoptionDecisionHolds(now.Add(-100*time.Hour), policy, now) {
		t.Fatal("zero 072 dwell ended the author's turn")
	}
}

// The lapse note is owed only for a stall the decision held, after its
// dwell; never for a stall it does not hold.
func TestAdoptionDecisionLapseDwell(t *testing.T) {
	policy := adoptPolicy()
	for _, tc := range []struct {
		reason string
		want   time.Duration
		ok     bool
	}{
		{stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused, stagelabels.Dwell(stagelabels.ReturnedToContributor, policy.Dwell), true},
		{stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:unit", stagelabels.Dwell(stagelabels.AdoptedByHive, policy.Dwell), true},
		{stagelabels.LaneReasonAdoptionStalledPrefix + autoMergeReasonHumanChangesRequested, stagelabels.Dwell(stagelabels.AdoptedByHive, policy.Dwell), true},
		{stagelabels.LaneReasonAdoptionStalledPrefix + "supersede-failed:dco-rehome", 0, false},
		{stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonHiveSignoffAttribution, 0, false},
		{stagelabels.LaneReasonAdoptionStalledPrefix + stagelabels.LaneReasonRebaseFailedTwice, 0, false},
		{"check-failing:unit", 0, false},
	} {
		if got, ok := adoptionDecisionLapseDwell(tc.reason, policy); got != tc.want || ok != tc.ok {
			t.Errorf("%s: (%v, %v), want (%v, %v)", tc.reason, got, ok, tc.want, tc.ok)
		}
	}
}

// The lapse note names the person without a mention, says how long the
// decision had and carries no en or em dash.
func TestAdoptionDecisionLapsedComment(t *testing.T) {
	at := time.Date(2026, 11, 25, 20, 13, 0, 0, time.UTC)
	reason := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused
	body := adoptionDecisionLapsedComment(stageObservation{HeadSHA: "a25e8a591fc5a1dfc85f022a8aeb8438b73930b8"}, reason, review.HumanChangeRequest{Actor: "@Danathar", RecordedAt: at}, 24*time.Hour+time.Minute)
	for _, want := range []string{AdoptionDecisionLapsedMarker + " head=a25e8a591fc5a1dfc85f022a8aeb8438b73930b8 reason=" + reason, "The decision Danathar recorded on 2026-11-25 20:13 UTC for head `a25e8a591fc5`", "had its full 24h1m with no new commit", "Moving to `" + stagelabels.NeedsDecision.Label() + "`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("lapse note missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@") || strings.ContainsAny(body, "\u2013\u2014") {
		t.Fatalf("lapse note mentions the person or uses a dash:\n%s", body)
	}
}

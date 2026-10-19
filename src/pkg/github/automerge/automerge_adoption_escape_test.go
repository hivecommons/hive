package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stage 074 escape (hivecommons/hive#12858).

const adoptedDwell = 3 * time.Hour

var adoptedLabels = []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}

// adoptInPlaceAt puts PR 7 at head into an in-place adoption at the clock,
// as the grace path does, without the red-CI history.
func adoptInPlaceAt(h *adoptHarness, head, reason string) string {
	h.adopt.allowMaintainerEdits(7, true)
	h.observe(adoptPR(7, head), reason)
	key := stageKey("acme", "widget", 7)
	h.e.stageStoreOrShared().MarkAdoptedInPlace(key, head, AdoptReasonGrace, h.clock.at)
	h.api.reset()
	return key
}

func stalledComments(api *stageAPI) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, AdoptionStalledMarker) {
			out = append(out, c)
		}
	}
	return out
}

// hivecommons/hive#12858 exactly: adopted in place at 5665cda after the
// hive's own signed fix was stripped for sign-off attribution, head
// unchanged since, CI green, the swarm's docs-currency finding blocking
// (changes_requested) on that head, no fix in flight. Before the dwell the
// hive keeps 074; once the dwell passes it re-homes the PR onto its own
// branch so the review-fix route can carry the finding.
func TestAdoptedInPlaceStalledFindingSupersedesAfterDwell(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "5665cda", contributorLaneReasonNoVerdict)
	// The sweep re-notes the swarm's verdict every pass; the tracker
	// drains it, so each observation notes it again.
	finding := func() {
		h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{
			Configured: true, Verdict: "changes_requested",
			HumanSummaries: []string{"rewritten source citations do not match the head tree"},
		})
	}

	// Inside the dwell: still the hive's, nothing written.
	finding()
	h.clock.advance(adoptedDwell - time.Minute)
	res := h.observe(adoptPR(7, "5665cda", adoptedLabels...), contributorLaneReasonChangesRequested)
	if res.Comments != 0 || len(h.adopt.pulls) != 0 || h.api.writes() != 0 {
		t.Fatalf("inside dwell churn: res=%+v pulls=%v added=%v removed=%v comments=%v", res, h.adopt.pulls, h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("inside dwell: %+v", rec)
	}

	// Dwell elapsed on the same head: supersede, once.
	h.api.reset()
	finding()
	h.clock.advance(2 * time.Minute)
	res = h.observe(adoptPR(7, "5665cda", adoptedLabels...), contributorLaneReasonChangesRequested)
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "5665cda" {
		t.Fatalf("re-home branch sha=%q refs=%v", got, h.adopt.refs)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	for _, want := range []string{"Supersedes #7", "review finding on the adopted head sat unaddressed", "**Open review finding carried over from #7:**", "(`changes_requested`)", "Green CI alone is not done"} {
		if !strings.Contains(body, want) {
			t.Fatalf("superseding body missing %q:\n%s", want, body)
		}
	}
	comments := adoptionComments(h.api)
	if len(comments) != 1 || !strings.Contains(comments[0], "reason="+AdoptReasonInPlaceStalled+" mode=supersede superseded_by=90 head=5665cda") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	if !strings.Contains(comments[0], "review swarm's finding on this head went unaddressed") {
		t.Fatalf("adoption comment does not name the finding: %v", comments[0])
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("superseded PR must not also be escalated: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedInPlace() || rec.AdoptedBy != 90 || rec.AdoptMode != stagelabels.AdoptModeSupersede || rec.AdoptReason != AdoptReasonInPlaceStalled || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("record after re-home: %+v", rec)
	}
	if res.Comments != 1 {
		t.Fatalf("comments=%d, want the one adoption note", res.Comments)
	}

	// Next sweep: the superseder carries it; nothing more happens here.
	h.api.reset()
	finding()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "5665cda", adoptedLabels...), contributorLaneReasonChangesRequested)
	if len(h.adopt.pulls) != 1 || h.api.writes() != 0 {
		t.Fatalf("re-home repeated: pulls=%v added=%v removed=%v comments=%v", h.adopt.pulls, h.api.added, h.api.removed, h.api.comments)
	}
}

// CI still running on the adopted head means a fix may be in flight: the
// dwell does not fire.
func TestAdoptedInPlaceFindingWithPendingCIStays074(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "aaa", "check-pending")
	h.e.noteStageVerdict("acme", "widget", 7, ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"})
	h.clock.advance(adoptedDwell + time.Hour)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-pending")
	if len(h.adopt.pulls) != 0 || h.api.writes() != 0 {
		t.Fatalf("pending CI must hold 074: pulls=%v added=%v removed=%v comments=%v", h.adopt.pulls, h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("record: %+v", rec)
	}
}

// The hive pushed a fix to the contributor branch signed as the App: the
// squash would land it as the contributor with the App's sign-off, which
// the sign-off gate rejects and the scanner strips. The hive cannot
// deliver on that branch at all, so the adoption re-homes at once.
func TestAdoptedInPlaceSquashUnsafeHiveCommitSupersedesImmediately(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.api.commitDetails = map[int][]stageCommitFixture{7: {
		{message: "fix: report failed kick delivery\n\nSigned-off-by: Mallory <mallory@example.com>", authorLogin: "mallory", authorName: "Mallory", authorEmail: "mallory@example.com"},
		{message: "docs: refresh citations\n\nSigned-off-by: hive[bot] <12345+hive[bot]@users.noreply.github.com>", authorLogin: testHiveAppBotLogin, authorName: "hive[bot]", authorEmail: "12345+hive[bot]@users.noreply.github.com"},
	}}
	key := adoptInPlaceAt(h, "f72fa44", contributorLaneReasonNoVerdict)
	h.adopt.setCommitAuthor("f72fa44", testHiveAppBotLogin)

	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "f72fa44", adoptedLabels...), contributorLaneReasonNoVerdict)
	if got := h.adopt.refs["refs/heads/hive/adopt-7"]; got != "f72fa44" {
		t.Fatalf("re-home branch sha=%q refs=%v comments=%v", got, h.adopt.refs, h.api.comments)
	}
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%v", h.adopt.pulls)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	if !strings.Contains(body, "cannot carry a squash-safe sign-off") {
		t.Fatalf("body does not say why:\n%s", body)
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "reason="+AdoptReasonSignoffRehome+" mode=supersede") || !strings.Contains(c[0], "the hive signs as the App, the squash lands as you") {
		t.Fatalf("adoption comment: %v", h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedInPlace() || rec.AdoptedBy != 90 || rec.AdoptReason != AdoptReasonSignoffRehome {
		t.Fatalf("record: %+v", rec)
	}
}

// Nothing the hive can act on but the head sat past the dwell with CI
// idle: a person gets it as 085 with the lane reason, one comment, once
// per head; a new head starts over.
func TestAdoptedStalledWithoutFindingEscalatesOncePerHead(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "aaa", "check-failing:unit")

	h.clock.advance(adoptedDwell + time.Minute)
	res := h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-failing:unit")
	if len(h.adopt.pulls) != 0 {
		t.Fatalf("red CI without a finding is not a re-home: %v", h.adopt.pulls)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != stagelabels.NeedsDecision.Label() || len(h.api.removed) != 1 || h.api.removed[0] != stagelabels.AdoptedByHive.Label() {
		t.Fatalf("074 → 085: added=%v removed=%v", h.api.added, h.api.removed)
	}
	notes := stalledComments(h.api)
	if len(notes) != 1 || res.Comments != 1 {
		t.Fatalf("stalled note: res=%+v comments=%v", res, h.api.comments)
	}
	wantReason := stagelabels.LaneReasonAdoptionStalledPrefix + "check-failing:unit"
	for _, want := range []string{"head=aaa reason=" + wantReason, "has not moved for 3h", "`check-failing:unit` on this head and no fix landed", "`" + stagelabels.NeedsDecision.Label() + "`", "not a merge-approval wait"} {
		if !strings.Contains(notes[0], want) {
			t.Fatalf("stalled note missing %q:\n%s", want, notes[0])
		}
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != wantReason || !rec.AdoptionEscalated("aaa") || !rec.AdoptedInPlace() {
		t.Fatalf("record after escalation: %+v", rec)
	}

	// Sticky and silent on the same head.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), "check-failing:unit")
	if h.api.writes() != 0 {
		t.Fatalf("escalation repeated: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision {
		t.Fatalf("not sticky: %+v", rec)
	}

	// The hive pushes a new head: 074 again, the clock restarts.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), "check-pending")
	rec, _ = h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("bbb") || rec.AdoptedHead != "bbb" || !rec.AdoptedHeadSince().Equal(h.clock.at) {
		t.Fatalf("new hive head: %+v", rec)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("new head escalated at once: %v", h.api.comments)
	}
}

// A re-home that could not open its PR leaves nobody working the head: it
// is a person's at the next sweep, with the failed reason named, and the
// push is not retried.
func TestAdoptedSupersedeFailureEscalatesInsteadOfSittingAt074(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	p.DCORequired = func(string) (bool, bool) { return true, true }
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.adopt.failRefs = true
	h.api.commits = map[int][]string{7: {"fix: unsigned contributor commit"}}
	key := adoptInPlaceAt(h, "aaa", "check-failing:DCO")
	labels := []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker", stagelabels.NoDCOLabel}

	h.clock.advance(time.Minute)
	h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", adoptPR(7, "aaa", labels...), "check-failing:DCO", false, nil)
	h.e.noteStageRole("acme", "widget", 7, stagelabels.RoleContributor)
	if _, err := h.e.ApplyStageLabels(context.Background()); err == nil {
		t.Fatal("ApplyStageLabels should surface the failed re-home push")
	}
	if h.adopt.refAttempts != 1 {
		t.Fatalf("ref attempts=%d, want 1", h.adopt.refAttempts)
	}

	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:DCO")
	if h.adopt.refAttempts != 1 || len(h.adopt.pulls) != 0 {
		t.Fatalf("failed re-home retried: attempts=%d pulls=%v", h.adopt.refAttempts, h.adopt.pulls)
	}
	wantReason := stagelabels.LaneReasonAdoptionStalledPrefix + "supersede-failed:" + AdoptReasonDCORehome
	notes := stalledComments(h.api)
	if len(notes) != 1 || !strings.Contains(notes[0], "reason="+wantReason) || !strings.Contains(notes[0], "could not open the superseding PR") {
		t.Fatalf("stalled note: %v", h.api.comments)
	}
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || rec.Reason != wantReason {
		t.Fatalf("record: %+v", rec)
	}
}

func TestAdoptionEscapeHelpers(t *testing.T) {
	policy := adoptPolicy()
	now := time.Date(2026, 10, 18, 14, 26, 0, 0, time.UTC)
	inPlace := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeInPlace, AdoptedAt: now, AdoptedHeadAt: now}
	blocking := stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}
	tests := []struct {
		name       string
		obs        stageObservation
		rec        stagelabels.Record
		at         time.Time
		wantRehome string
		wantStall  string
	}{
		{"fresh in-place with finding stays", stageObservation{HeadSHA: "aaa", Reason: "reviewer-verdict:changes_requested", LaneReason: contributorLaneReasonChangesRequested, Detail: blocking}, inPlace, now.Add(time.Hour), "", ""},
		{"stale in-place with finding re-homes", stageObservation{HeadSHA: "aaa", Reason: "reviewer-verdict:changes_requested", LaneReason: contributorLaneReasonChangesRequested, Detail: blocking}, inPlace, now.Add(adoptedDwell), AdoptReasonInPlaceStalled, "adoption-stalled:reviewer-verdict:changes_requested"},
		{"stale in-place finding but CI pending waits", stageObservation{HeadSHA: "aaa", Reason: "reviewer-verdict:changes_requested", LaneReason: "required-check-pending:unit", Detail: blocking}, inPlace, now.Add(adoptedDwell), "", ""},
		{"squash-unsafe hive commit re-homes at once", stageObservation{HeadSHA: "aaa", Reason: stagelabels.LaneReasonHiveSignoffAttribution, Detail: stageDetail{DCOSquashUnsafeHive: 1}}, inPlace, now.Add(time.Minute), AdoptReasonSignoffRehome, ""},
		{"dco-red head is the DCO re-home's", stageObservation{HeadSHA: "aaa", Reason: stagelabels.LaneReasonDCOMissing, Detail: stageDetail{DCOSquashUnsafeHive: 1, DCO: stagelabels.DCONo, DCOUnsigned: 1}}, inPlace, now.Add(time.Minute), "", ""},
		{"other head is not stale", stageObservation{HeadSHA: "bbb", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, inPlace, now.Add(adoptedDwell), "", ""},
		{"stale red head escalates", stageObservation{HeadSHA: "aaa", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, inPlace, now.Add(adoptedDwell), "", "adoption-stalled:check-failing:unit"},
		{"live superseder is progress", stageObservation{HeadSHA: "aaa", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptedBy: 90, AdoptedAt: now}, now.Add(adoptedDwell), "", ""},
		{"supersede that opened nothing escalates now", stageObservation{HeadSHA: "aaa", Reason: "check-failing:DCO"}, stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptReason: AdoptReasonDCORehome, AdoptedAt: now}, now.Add(time.Minute), "", "adoption-stalled:supersede-failed:dco-rehome"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inPlaceRehomeReason(tc.obs, tc.rec, policy, tc.at); got != tc.wantRehome {
				t.Fatalf("inPlaceRehomeReason=%q, want %q", got, tc.wantRehome)
			}
			if got := adoptionStalledReason(tc.obs, tc.rec, policy, tc.at); got != tc.wantStall {
				t.Fatalf("adoptionStalledReason=%q, want %q", got, tc.wantStall)
			}
		})
	}
}

func TestAdoptionBodyCauseAndBlockerText(t *testing.T) {
	for reason, want := range map[string]string{
		AdoptReasonOffline:        "adopted after CI went red",
		AdoptReasonDCORehome:      "re-homed for DCO repair",
		AdoptReasonSignoffRehome:  "squash-safe sign-off",
		AdoptReasonInPlaceStalled: "sat unaddressed past the stage dwell",
	} {
		if got := adoptionBodyCause(reason); !strings.Contains(got, want) {
			t.Fatalf("adoptionBodyCause(%q)=%q, want %q", reason, got, want)
		}
	}
	obs := stageObservation{HeadSHA: "5665cda", LaneReason: "required-check-failing:docs"}
	if got := adoptionBlockerText(obs, stagelabels.Record{}); !strings.Contains(got, "`required-check-failing:docs` on this head") {
		t.Fatalf("red lane reason: %q", got)
	}
	obs.Detail = stageDetail{RedChecks: []string{"docs"}, RedChecksHead: "5665cda"}
	if got := adoptionBlockerText(obs, stagelabels.Record{}); !strings.Contains(got, "check `docs` is red") {
		t.Fatalf("red check: %q", got)
	}
	obs.Detail = stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionQuestion: "ship without the migration?"}}
	if got := adoptionBlockerText(obs, stagelabels.Record{}); !strings.Contains(got, "ship without the migration?") {
		t.Fatalf("needs-human: %q", got)
	}
	obs.Detail = stageDetail{}
	obs.LaneReason = ""
	obs.Reason = "something-else"
	if got := adoptionBlockerText(obs, stagelabels.Record{}); !strings.Contains(got, "`something-else`") {
		t.Fatalf("fallback: %q", got)
	}
}

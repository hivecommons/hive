package automerge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
// squash would land it as the contributor with the App's sign-off. An
// App-authored superseder changes the squash attribution, not the commits.
func TestAdoptedInPlaceSquashUnsafeHiveCommitSupersedes(t *testing.T) {
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
	if h.adopt.refs["refs/heads/hive/adopt-7"] != "f72fa44" || len(h.adopt.pulls) != 1 {
		t.Fatalf("expected unchanged head on superseder: refs=%v pulls=%v", h.adopt.refs, h.adopt.pulls)
	}
	body, _ := h.adopt.pulls[0]["body"].(string)
	if !strings.Contains(body, "**Sign-off attribution:**") || strings.Contains(body, "**Open review finding") {
		t.Fatalf("signoff explanation invents a review finding: %s", body)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedInPlace() || rec.AdoptedBy != 90 || rec.Stage != stagelabels.AdoptedByHive || rec.AdoptReason != AdoptReasonSignoffRehome || len(stalledComments(h.api)) != 0 {
		t.Fatalf("record: %+v comments=%v", rec, h.api.comments)
	}

	// Next sweep on the same head: no second superseder or stall note.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "f72fa44", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if len(h.adopt.pulls) != 1 || len(stalledComments(h.api)) != 0 {
		t.Fatalf("repeated: pulls=%v comments=%v", h.adopt.pulls, h.api.comments)
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
	for _, want := range []string{"head=aaa reason=" + wantReason, "has not moved for 3h1m and", "`check-failing:unit` on this head and no fix landed", "`" + stagelabels.NeedsDecision.Label() + "`", "not a merge-approval wait"} {
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
		{"squash-unsafe hive commit re-homes at once", stageObservation{HeadSHA: "aaa", Reason: stagelabels.LaneReasonHiveSignoffAttribution, Detail: stageDetail{DCOSquashUnsafeHive: 1}}, inPlace, now.Add(time.Minute), AdoptReasonSignoffRehome, "adoption-stalled:hive-signoff-attribution"},
		{"squash-unsafe hive commit with finding re-homes at once", stageObservation{HeadSHA: "aaa", Reason: stagelabels.LaneReasonHiveSignoffAttribution, LaneReason: contributorLaneReasonChangesRequested, Detail: stageDetail{DCOSquashUnsafeHive: 1, VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}}}, inPlace, now.Add(adoptedDwell), AdoptReasonSignoffRehome, "adoption-stalled:hive-signoff-attribution"},
		{"dco-red head is the DCO re-home's", stageObservation{HeadSHA: "aaa", Reason: stagelabels.LaneReasonDCOMissing, Detail: stageDetail{DCOSquashUnsafeHive: 1, DCO: stagelabels.DCONo, DCOUnsigned: 1}}, inPlace, now.Add(time.Minute), "", ""},
		{"other head is not stale", stageObservation{HeadSHA: "bbb", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, inPlace, now.Add(adoptedDwell), "", ""},
		{"stale red head escalates", stageObservation{HeadSHA: "aaa", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, inPlace, now.Add(adoptedDwell), "", "adoption-stalled:check-failing:unit"},
		{"stale required-red head escalates", stageObservation{HeadSHA: "aaa", Reason: "required-check-failing:unit", LaneReason: "required-check-failing:unit"}, inPlace, now.Add(adoptedDwell), "", "adoption-stalled:required-check-failing:unit"},
		{"stale optional-red head is not a blocker", stageObservation{HeadSHA: "aaa", Reason: stageReasonOptionalCheckFailing + "overlap", LaneReason: contributorLaneReasonNoVerdict, Detail: stageDetail{RedChecks: []string{"overlap"}, RedChecksHead: "aaa"}}, inPlace, now.Add(2 * adoptedDwell), "", ""},
		{"stale optional-red head with finding re-homes", stageObservation{HeadSHA: "aaa", Reason: stageReasonOptionalCheckFailing + "overlap", LaneReason: contributorLaneReasonChangesRequested, Detail: stageDetail{VerdictKnown: true, Verdict: ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"}, RedChecks: []string{"overlap"}, RedChecksHead: "aaa"}}, inPlace, now.Add(adoptedDwell), AdoptReasonInPlaceStalled, "adoption-stalled:optional-check-failing:overlap"},
		{"legacy record without a head clock is not stale", stageObservation{HeadSHA: "92fba9a", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, stagelabels.Record{AdoptedHead: "92fba9a", AdoptMode: stagelabels.AdoptModeInPlace, AdoptedAt: now, HivePushedHeads: []string{"92fba9a"}}, now.Add(28 * time.Hour), "", ""},
		{"live superseder is progress", stageObservation{HeadSHA: "aaa", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}, stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptedBy: 90, AdoptedAt: now}, now.Add(adoptedDwell), "", ""},
		{"supersede that opened nothing escalates now", stageObservation{HeadSHA: "aaa", Reason: "check-failing:DCO"}, stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptReason: AdoptReasonDCORehome, AdoptedAt: now}, now.Add(time.Minute), "", "adoption-stalled:supersede-failed:dco-rehome"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inPlaceRehomeReason(tc.obs, tc.rec, policy, tc.at, false); got != tc.wantRehome {
				t.Fatalf("inPlaceRehomeReason=%q, want %q", got, tc.wantRehome)
			}
			if got := adoptionStalledReason(tc.obs, tc.rec, policy, tc.at); got != tc.wantStall {
				t.Fatalf("adoptionStalledReason=%q, want %q", got, tc.wantStall)
			}
		})
	}
}

// hivecommons/hive#16023: an in-place adoption whose fixer rebase episode
// is exhausted re-homes to a superseding PR at once instead of waiting out
// the 074 dwell; a live episode, a non-conflicting head or a moved head
// does not.
func TestInPlaceRehomeOnExhaustedRebaseEpisode(t *testing.T) {
	policy := adoptPolicy()
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	inPlace := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeInPlace, AdoptedAt: now, AdoptedHeadAt: now}
	conflict := stageObservation{Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa", Reason: "conflicting", LaneReason: "conflicting", Labels: []string{"needs-rebase"}}
	red := stageObservation{Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa", Reason: "check-failing:unit", LaneReason: "check-failing:unit"}
	moved := conflict
	moved.HeadSHA = "bbb"
	at := now.Add(30 * time.Minute)
	tests := []struct {
		name      string
		obs       stageObservation
		exhausted bool
		want      string
	}{
		{"exhausted conflict re-homes before the dwell", conflict, true, AdoptReasonRebaseRehome},
		{"live episode waits for the dwell", conflict, false, ""},
		{"exhausted but not conflicting stays", red, true, ""},
		{"exhausted on a moved head stays", moved, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inPlaceRehomeReason(tc.obs, inPlace, policy, at, tc.exhausted); got != tc.want {
				t.Fatalf("inPlaceRehomeReason=%q, want %q", got, tc.want)
			}
		})
	}
	if got := inPlaceRehomeReason(conflict, inPlace, policy, now.Add(adoptedDwell), false); got != AdoptReasonRebaseRehome {
		t.Fatalf("dwell path still re-homes: %q", got)
	}

	episodes := map[int]RebaseEpisode{7: {Attempt: 3, MaxAttempts: 2, Exhausted: true}, 8: {Attempt: 1, MaxAttempts: 2}}
	e := &Engine{rebaseEpisode: func(repo string, number int) (RebaseEpisode, bool) {
		if repo != "acme/widget" {
			return RebaseEpisode{}, false
		}
		ep, ok := episodes[number]
		return ep, ok
	}}
	live := conflict
	live.Number = 8
	none := conflict
	none.Number = 9
	supersede := stagelabels.Record{AdoptedHead: "aaa", AdoptMode: stagelabels.AdoptModeSupersede, AdoptedBy: 90, AdoptedAt: now}
	if !e.inPlaceRebaseExhausted(conflict, inPlace) {
		t.Fatal("exhausted episode on the in-place adoption not reported")
	}
	if e.inPlaceRebaseExhausted(live, inPlace) || e.inPlaceRebaseExhausted(none, inPlace) || e.inPlaceRebaseExhausted(conflict, supersede) {
		t.Fatal("live, missing or supersede-mode episode reported exhausted")
	}
	if (&Engine{}).inPlaceRebaseExhausted(conflict, inPlace) {
		t.Fatal("no episode reader reports exhausted")
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
	signoff := stageObservation{Number: 13595, Author: "hanthor", HeadSHA: "64dd4be", Detail: stageDetail{DCOSquashUnsafeHive: 2}}
	inPlace := stagelabels.Record{AdoptedHead: "64dd4be", AdoptMode: stagelabels.AdoptModeInPlace}
	if got := adoptionBlockerText(signoff, inPlace); !strings.Contains(got, "`gh pr merge 13595 --merge`, not squash") || !strings.Contains(got, "2 commit(s)") || !strings.Contains(got, "@hanthor") {
		t.Fatalf("signoff: %q", got)
	}
	for _, mode := range []string{"hold", "human-verdict", "moved-head"} {
		blocked := signoff
		want := ""
		switch mode {
		case "hold":
			blocked.Labels = []string{"hold"}
			want = "`hold` label prevents superseding"
		case "human-verdict":
			blocked.Detail.VerdictKnown = true
			blocked.Detail.Verdict = ContributorLaneVerdict{Configured: true, Verdict: "requires_human"}
			want = "requires a human decision before superseding"
		case "moved-head":
			blocked.HeadSHA = "new-head"
			want = "settle ownership of the new head"
		}
		if got := adoptionBlockerText(blocked, inPlace); !strings.Contains(got, want) || strings.Contains(got, "restore branch/PR-create access") {
			t.Fatalf("%s stall must name actual blocker, not a failed write: %q", mode, got)
		}
	}
	obs.Detail = stageDetail{}
	obs.LaneReason = ""
	obs.Reason = "something-else"
	if got := adoptionBlockerText(obs, stagelabels.Record{}); !strings.Contains(got, "`something-else`") {
		t.Fatalf("fallback: %q", got)
	}
}

// legacyAdoptedHead rewrites PR 7's record as a pre-hivecommons/hive#13040
// binary left it: adopted in place at adoptedHead, advanced by a recorded
// hive push to head, with no adopted_head_at on disk.
func legacyAdoptedHead(t *testing.T, h *adoptHarness, key, adoptedHead, head, escalatedReason string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy-stage.json")
	store := stagelabels.NewStore(path, h.clock.now)
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	src := h.e.stageStoreOrShared()
	for _, k := range src.Keys() {
		r, _ := src.Get(k)
		store.Observe(k, r.Stage, r.Reason)
	}
	store.MarkAdoptedInPlace(key, adoptedHead, AdoptReasonGrace, h.clock.at)
	store.MarkHivePushedHead(key, head)
	store.MarkAdoptedHead(key, head)
	if escalatedReason != "" {
		store.Observe(key, stagelabels.NeedsDecision, escalatedReason)
		store.MarkAdoptionEscalated(key, head, escalatedReason)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var items map[string]map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatal(err)
	}
	delete(items[key], "adopted_head_at")
	delete(items[key], "adopt_escalation_noted_head")
	raw, _ = json.Marshal(items)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil {
		t.Fatal(err)
	}
	rec, _ := store.Get(key)
	if !rec.AdoptedHeadClockUnset() || rec.AdoptedHead != head || !rec.HivePushedHead(head) {
		t.Fatalf("legacy fixture: %+v", rec)
	}
	h.e.stageStore = store
}

// hivecommons/hive#13040, hivecommons/hive#12858 exactly: adopted in place
// at 5665cda on 2026-10-18 14:27, the hive pushed 92fba9a at 18:10 under a
// binary that stamped no head clock, and the new binary first saw the PR
// 15 minutes later with the optional `overlap` check red and no verdict on
// the head. The record is not stale (its clock starts now), an optional
// check is not a blocker, and the PR stays 074 with nothing written — past
// the dwell too.
func TestAdoptedLegacyHeadWithOptionalCheckRedStays074(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.clock.at = time.Date(2026, 10, 18, 14, 27, 0, 0, time.UTC)
	key := adoptInPlaceAt(h, "5665cda", contributorLaneReasonNoVerdict)
	h.clock.at = time.Date(2026, 10, 18, 18, 10, 33, 0, time.UTC)
	legacyAdoptedHead(t, h, key, "5665cda", "92fba9a", "")
	h.adopt.setCommitAuthor("92fba9a", testHiveAppBotLogin)
	overlapRed := func() { h.e.noteStageRedChecks("acme", "widget", 7, "92fba9a", []string{"overlap"}) }

	h.clock.at = time.Date(2026, 10, 18, 18, 25, 35, 0, time.UTC)
	overlapRed()
	res := h.observe(adoptPR(7, "92fba9a", adoptedLabels...), contributorLaneReasonNoVerdict)
	if res.Comments != 0 || len(h.adopt.pulls) != 0 || h.api.writes() != 0 {
		t.Fatalf("first sight escalated: res=%+v pulls=%v added=%v removed=%v comments=%v", res, h.adopt.pulls, h.api.added, h.api.removed, h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("92fba9a") || rec.AdoptedHeadClockUnset() || !rec.AdoptedHeadSince().Equal(h.clock.at) || rec.Reason != stageReasonOptionalCheckFailing+"overlap" {
		t.Fatalf("legacy record after first sight: %+v", rec)
	}

	// Past the dwell with only the optional check red: still the hive's
	// (the generic stalled marker may apply; 085 must not).
	h.clock.advance(adoptedDwell + time.Hour)
	overlapRed()
	h.observe(adoptPR(7, "92fba9a", adoptedLabels...), contributorLaneReasonNoVerdict)
	for _, l := range h.addedLabels() {
		if l == stagelabels.NeedsDecision.Label() {
			t.Fatalf("optional check escalated: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
		}
	}
	if len(stalledComments(h.api)) != 0 || len(h.api.removed) != 0 {
		t.Fatalf("optional check escalated: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec, _ = h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("92fba9a") {
		t.Fatalf("optional check escalated: %+v", rec)
	}

	// A required check going red on that head is a blocker: 085, with the
	// dwell text reading from the head clock, not the adoption.
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "92fba9a", adoptedLabels...), "required-check-failing:unit")
	notes := stalledComments(h.api)
	if len(notes) != 1 || !strings.Contains(notes[0], "has not moved for 4h1m and") || !strings.Contains(notes[0], "reason=adoption-stalled:required-check-failing:unit") {
		t.Fatalf("required red not escalated with the head clock: %v", h.api.comments)
	}
	if rec, _ = h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("92fba9a") {
		t.Fatalf("required red: %+v", rec)
	}
}

// hivecommons/hive#12858 as the buggy binary left it: the legacy record
// (no head clock) already escalated to 085 for the optional `overlap`
// check. The fixed binary's first sweep stamps the clock, finds no blocker,
// releases the escalation and puts the PR back at 074 without a comment.
func TestAdoptedLegacyEscalationForOptionalCheckReleases(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.clock.at = time.Date(2026, 10, 18, 14, 27, 0, 0, time.UTC)
	key := adoptInPlaceAt(h, "5665cda", contributorLaneReasonNoVerdict)
	h.clock.at = time.Date(2026, 10, 18, 18, 10, 33, 0, time.UTC)
	wasReason := stagelabels.LaneReasonAdoptionStalledPrefix + stageReasonOptionalCheckFailing + "overlap"
	legacyAdoptedHead(t, h, key, "5665cda", "92fba9a", wasReason)
	h.adopt.setCommitAuthor("92fba9a", testHiveAppBotLogin)

	h.clock.at = time.Date(2026, 10, 18, 18, 52, 0, 0, time.UTC)
	h.e.noteStageRedChecks("acme", "widget", 7, "92fba9a", []string{"overlap"})
	res := h.observe(adoptPR(7, "92fba9a", stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"), contributorLaneReasonNoVerdict)
	if added := h.addedLabels(); len(added) != 1 || added[0] != stagelabels.AdoptedByHive.Label() || len(h.api.removed) != 1 || h.api.removed[0] != stagelabels.NeedsDecision.Label() || res.Comments != 0 {
		t.Fatalf("085 → 074: res=%+v added=%v removed=%v comments=%v", res, h.api.added, h.api.removed, h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("92fba9a") || rec.AdoptEscalatedReason != "" || rec.AdoptedHeadClockUnset() || !rec.AdoptedHeadSince().Equal(h.clock.at) || !rec.AdoptedInPlace() {
		t.Fatalf("legacy escalation not released: %+v", rec)
	}
}

// hivecommons/hive#13040: an escalation holds only while its reason does.
// When the blocker clears on the same head the PR drops back to 074 and the
// lane carries on; if the blocker returns on that head the PR is 085 again
// without a second note.
func TestAdoptedEscalationReleasesWhenReasonClears(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "aaa", "check-failing:unit")
	escalated := []string{stagelabels.NeedsDecision.Label(), "role/contributor", "author/clanker"}

	h.clock.advance(adoptedDwell + time.Minute)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") || len(stalledComments(h.api)) != 1 {
		t.Fatalf("not escalated: %+v comments=%v", rec, h.api.comments)
	}

	// Somebody re-ran the check: CI is in flight on the head, so nothing
	// is stalled and the PR is the hive's again.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", escalated...), "check-pending")
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if added := h.addedLabels(); len(added) != 1 || added[0] != stagelabels.AdoptedByHive.Label() || len(h.api.removed) != 1 || h.api.removed[0] != stagelabels.NeedsDecision.Label() {
		t.Fatalf("085 → 074: added=%v removed=%v comments=%v", h.api.added, h.api.removed, h.api.comments)
	}
	if rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("aaa") || !rec.AdoptionEscalationNoted("aaa") || !rec.AdoptedInPlace() || rec.AdoptedHead != "aaa" {
		t.Fatalf("escalation not released: %+v", rec)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("release wrote a note: %v", h.api.comments)
	}

	// The same check goes red again on the same head: 085, silently.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != stagelabels.NeedsDecision.Label() {
		t.Fatalf("074 → 085 again: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("flapping blocker noted twice: %v", h.api.comments)
	}
	if rec, _ = h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || !rec.AdoptionEscalated("aaa") {
		t.Fatalf("not re-escalated: %+v", rec)
	}

	// The hive pushes a new head and it goes red past the dwell: a fresh
	// note for the fresh head.
	h.adopt.setCommitAuthor("bbb", testHiveAppBotLogin)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "bbb", escalated...), "check-failing:unit")
	if rec, _ = h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.AdoptedByHive || rec.AdoptionEscalated("bbb") || rec.AdoptionEscalationNoted("bbb") {
		t.Fatalf("new head: %+v", rec)
	}
	h.clock.advance(adoptedDwell + time.Minute)
	h.observe(adoptPR(7, "bbb", adoptedLabels...), "check-failing:unit")
	if notes := stalledComments(h.api); len(notes) != 1 || !strings.Contains(notes[0], "head=bbb ") {
		t.Fatalf("new head not noted: %v", h.api.comments)
	}
}

func TestFormatSat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		3 * time.Hour:                   "3h",
		3*time.Hour + time.Minute:       "3h1m",
		15*time.Minute + 20*time.Second: "15m",
		10 * time.Second:                "1m",
	} {
		if got := formatSat(d); got != want {
			t.Fatalf("formatSat(%v)=%q, want %q", d, got, want)
		}
	}
}

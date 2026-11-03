package automerge

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Orphaned adoptions (hivecommons/hive#15052): the hive's copy is closed
// without merging while the adoption stands.

func commentsWithPrefix(api *stageAPI, prefix string) []string {
	var out []string
	for _, c := range api.comments {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// adoptAt074 adopts #7 (head aaa, copy #90) and leaves it labelled 074.
func adoptAt074(t *testing.T) (*adoptHarness, func(), []string) {
	t.Helper()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("setup: pulls=%d", len(h.adopt.pulls))
	}
	return h, done, []string{"stage/074-adopted-by-hive", "role/contributor", "author/clanker"}
}

// Copy closed unmerged → the adoption is orphaned and re-run from the same
// head: a fresh copy, a fresh marker naming it, the original never closed,
// and the PR stays at 074.
func TestAdoptionSupersederClosedUnmergedReadopts(t *testing.T) {
	h, done, adopted := adoptAt074(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("original closed although the copy was abandoned: %v", h.adopt.prEdits)
	}
	if len(h.adopt.pulls) != 2 {
		t.Fatalf("orphaned adoption not re-run: pulls=%d", len(h.adopt.pulls))
	}
	markers := adoptionComments(h.api)
	if len(markers) != 1 || !strings.Contains(markers[0], "superseded_by=91") || !strings.Contains(markers[0], "head=aaa") || res.Comments < 1 {
		t.Fatalf("fresh adoption marker: %v res=%+v", h.api.comments, res)
	}
	if n := commentsWithPrefix(h.api, AdoptionRevertedMarker); len(n) != 0 {
		t.Fatalf("reverted although re-adoption worked: %v", n)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 91 || rec.AdoptedHead != "aaa" || rec.AdoptOrphanedBy != 90 || rec.AdoptOrphanCount != 1 {
		t.Fatalf("record after re-adoption = %+v", rec)
	}
	// Still 074; the fresh copy is polled, not re-opened.
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 2 {
		t.Fatalf("churn after re-adoption: added=%v removed=%v comments=%v pulls=%d", h.api.added, h.api.removed, h.api.comments, len(h.adopt.pulls))
	}
	// The fresh copy merges: the original closes through the usual path.
	h.adopt.setSuperseder(91, "closed", true)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed once the fresh copy merged: %v", h.adopt.prEdits)
	}
	if c := commentsWithPrefix(h.api, adoptionCloseMarker); len(c) != 1 || !strings.Contains(c[0], "#91") {
		t.Fatalf("close note: %v", h.api.comments)
	}
}

// The fresh copy is closed again within the hour: no third copy; the PR
// parks at 085 with reason adoption-child-closed-twice and stays there.
func TestAdoptionSupersederClosedTwiceParksForPerson(t *testing.T) {
	h, done, adopted := adoptAt074(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 2 {
		t.Fatalf("setup: pulls=%d", len(h.adopt.pulls))
	}
	h.adopt.setSuperseder(91, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 2 {
		t.Fatalf("a third copy was opened: pulls=%d", len(h.adopt.pulls))
	}
	notes := commentsWithPrefix(h.api, AdoptionChildClosedTwiceMarker)
	if len(notes) != 1 || !strings.Contains(notes[0], "#90, then #91") || !strings.Contains(notes[0], "needs a person") {
		t.Fatalf("loop-guard note: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 91 || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionChildClosedTwice || rec.AdoptOrphanCount != 2 {
		t.Fatalf("record after loop guard = %+v", rec)
	}
	// The escalation holds on the next pass: 085 with the reason named.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	rec, _ = h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stagelabels.LaneReasonAdoptionChildClosedTwice {
		t.Fatalf("stage after loop guard = %v reason=%q", rec.Stage, rec.Reason)
	}
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/085-needs-decision" {
		t.Fatalf("085 label: added=%v removed=%v", h.api.added, h.api.removed)
	}
	// Parked: nothing further happens on later passes.
	at085 := []string{"stage/085-needs-decision", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", at085...), "check-failing:unit")
	if h.api.writes() != 0 || len(h.adopt.pulls) != 2 || len(h.adopt.prEdits) != 0 {
		t.Fatalf("parked PR churned: added=%v removed=%v comments=%v pulls=%d", h.api.added, h.api.removed, h.api.comments, len(h.adopt.pulls))
	}
}

// A copy closed unmerged a safe distance after the first recovery is a new
// orphan, not a loop: re-adopted again.
func TestAdoptionSupersederClosedAgainAfterWindowReadopts(t *testing.T) {
	h, done, adopted := adoptAt074(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	h.adopt.setSuperseder(91, "closed", false)
	h.api.reset()
	h.clock.advance(stagelabels.AdoptionOrphanLoopWindow + time.Minute)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 3 || len(commentsWithPrefix(h.api, AdoptionChildClosedTwiceMarker)) != 0 {
		t.Fatalf("second orphan outside the window: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 92 || rec.AdoptOrphanCount != 2 {
		t.Fatalf("record = %+v", rec)
	}
}

// The head moved under the orphaned adoption: no copy of a stale commit;
// the adoption is reverted with a note and the PR rejoins its flow.
func TestAdoptionOrphanedHeadMovedReverts(t *testing.T) {
	h, done, adopted := adoptAt074(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("re-adopted a moved head: pulls=%d", len(h.adopt.pulls))
	}
	notes := commentsWithPrefix(h.api, AdoptionRevertedMarker)
	if len(notes) != 1 || !strings.Contains(notes[0], "why=head-moved") || !strings.Contains(notes[0], "#90, was closed without merging") || !strings.Contains(notes[0], "moved to a newer commit") {
		t.Fatalf("revert note: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 0 || rec.AdoptedHead != "" || rec.AdoptOrphanCount != 0 {
		t.Fatalf("adoption not cleared: %+v", rec)
	}
	h.api.reset()
	h.observe(adoptPR(7, "bbb", adopted...), "check-failing:unit")
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/072-returned-to-contributor" {
		t.Fatalf("back to 072: added=%v removed=%v", h.api.added, h.api.removed)
	}
}

// The fresh copy cannot be opened (the branch write fails): reverted with
// the plain reason rather than left pointing at a dead copy.
func TestAdoptionOrphanedReadoptFailureReverts(t *testing.T) {
	h, done, adopted := adoptAt074(t)
	defer done()
	h.adopt.setSuperseder(90, "closed", false)
	h.adopt.mu.Lock()
	h.adopt.failRefs = true
	h.adopt.mu.Unlock()
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	h.observe(adoptPR(7, "aaa", adopted...), "check-failing:unit")
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("a copy was opened without a branch: pulls=%d", len(h.adopt.pulls))
	}
	notes := commentsWithPrefix(h.api, AdoptionRevertedMarker)
	if len(notes) != 1 || !strings.Contains(notes[0], "why=readopt-failed") || !strings.Contains(notes[0], "could not open a fresh copy") {
		t.Fatalf("revert note: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 0 || rec.AdoptedHead != "" {
		t.Fatalf("adoption not cleared: %+v", rec)
	}
}

func adoptedMarkerComment(at time.Time, superseding int, head string) map[string]any {
	return map[string]any{
		"id":         int64(1000 + superseding),
		"body":       AdoptionMarkerPrefix + " by=hive reason=contributor-offline mode=supersede superseded_by=" + strconv.Itoa(superseding) + " head=" + head + " -->\n@mallory the hive has adopted this PR.",
		"created_at": at.Format(time.RFC3339),
		"user":       map[string]any{"login": "hive[bot]"},
	}
}

// requiresHumanAt085 is a PR the store has no adoption for, parked at 085 by
// the reviewer verdict — hivecommons/hive#14947 after the store forgot
// #14977.
func requiresHumanAt085(t *testing.T) (*adoptHarness, func(), func() StageLabelResult) {
	t.Helper()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	requiresHuman := ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true, DecisionQuestion: "close in favour of the superseding PR?"}
	h.e.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return requiresHuman }
	at085 := []string{"stage/085-needs-decision", "role/contributor", "author/clanker"}
	observe := func() StageLabelResult {
		h.e.noteStageVerdict("acme", "widget", 7, requiresHuman)
		return h.observe(adoptPR(7, "aaa", at085...), stageReasonDecisionRequested)
	}
	return h, done, observe
}

// The record forgot the adoption but the PR wears the marker, and the copy
// it names is closed unmerged: re-adopted from the marker in one pass.
func TestAdoptionMarkerWithClosedCopyReadopts(t *testing.T) {
	h, done, observe := requiresHumanAt085(t)
	defer done()
	h.adopt.mu.Lock()
	h.adopt.issueComments = map[int][]map[string]any{7: {adoptedMarkerComment(h.clock.at.Add(-3*time.Hour), 89, "aaa")}}
	h.adopt.superseders[89] = map[string]any{"number": 89, "state": "closed", "merged": false}
	h.adopt.mu.Unlock()
	res := observe()
	if h.adopt.commentReads != 1 {
		t.Fatalf("comment reads = %d", h.adopt.commentReads)
	}
	if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 0 {
		t.Fatalf("marker orphan not re-adopted: pulls=%d edits=%v", len(h.adopt.pulls), h.adopt.prEdits)
	}
	markers := adoptionComments(h.api)
	if len(markers) != 1 || !strings.Contains(markers[0], "superseded_by=90") || res.Comments < 1 {
		t.Fatalf("fresh marker: %v", h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 90 || rec.AdoptedHead != "aaa" || rec.AdoptOrphanedBy != 89 || rec.AdoptMarkerScanHead != "aaa" {
		t.Fatalf("record = %+v", rec)
	}
	// Next pass: the adoption is live again, so the PR is the hive's (074)
	// and the comments are not read twice at this head.
	h.api.reset()
	h.clock.advance(time.Minute)
	observe()
	if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/074-adopted-by-hive" {
		t.Fatalf("074 after marker recovery: added=%v removed=%v", h.api.added, h.api.removed)
	}
	if h.adopt.commentReads != 1 {
		t.Fatalf("comments re-read: %d", h.adopt.commentReads)
	}
}

// Marker names a merged copy the store forgot: the original closes through
// the standard "Adopted → merged" path.
func TestAdoptionMarkerWithMergedCopyClosesOriginal(t *testing.T) {
	h, done, observe := requiresHumanAt085(t)
	defer done()
	h.adopt.mu.Lock()
	h.adopt.issueComments = map[int][]map[string]any{7: {adoptedMarkerComment(h.clock.at.Add(-3*time.Hour), 89, "aaa")}}
	h.adopt.superseders[89] = map[string]any{"number": 89, "state": "closed", "merged": true}
	h.adopt.mu.Unlock()
	observe()
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" || len(h.adopt.pulls) != 0 {
		t.Fatalf("original not closed: edits=%v pulls=%d", h.adopt.prEdits, len(h.adopt.pulls))
	}
	if c := commentsWithPrefix(h.api, adoptionCloseMarker); len(c) != 1 || !strings.Contains(c[0], "#89") {
		t.Fatalf("close note: %v", h.api.comments)
	}
}

// Two markers in the last hour, the latest copy closed: the loop guard
// parks from the comment history alone.
func TestAdoptionMarkersClosedTwiceParksFromHistory(t *testing.T) {
	h, done, observe := requiresHumanAt085(t)
	defer done()
	h.adopt.mu.Lock()
	h.adopt.issueComments = map[int][]map[string]any{7: {
		adoptedMarkerComment(h.clock.at.Add(-50*time.Minute), 88, "aaa"),
		adoptedMarkerComment(h.clock.at.Add(-20*time.Minute), 89, "aaa"),
	}}
	h.adopt.superseders[89] = map[string]any{"number": 89, "state": "closed", "merged": false}
	h.adopt.mu.Unlock()
	observe()
	if len(h.adopt.pulls) != 0 || len(commentsWithPrefix(h.api, AdoptionChildClosedTwiceMarker)) != 1 {
		t.Fatalf("loop guard from markers: pulls=%d comments=%v", len(h.adopt.pulls), h.api.comments)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 89 || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionChildClosedTwice {
		t.Fatalf("record = %+v", rec)
	}
}

// A marker a later note settled (revert, close) is not an adoption, and a
// PR with no marker reads its comments once per head.
func TestAdoptionMarkerSettledOrAbsentIsNotRecovered(t *testing.T) {
	h, done, observe := requiresHumanAt085(t)
	defer done()
	h.adopt.mu.Lock()
	h.adopt.issueComments = map[int][]map[string]any{7: {
		adoptedMarkerComment(h.clock.at.Add(-3*time.Hour), 89, "aaa"),
		{"id": int64(2), "body": AdoptionRevertedMarker + " superseded_by=89 head=aaa why=head-moved -->\nundone", "created_at": h.clock.at.Add(-2 * time.Hour).Format(time.RFC3339)},
	}}
	h.adopt.superseders[89] = map[string]any{"number": 89, "state": "closed", "merged": false}
	h.adopt.mu.Unlock()
	observe()
	observe()
	if len(h.adopt.pulls) != 0 || h.adopt.commentReads != 1 {
		t.Fatalf("settled marker recovered: pulls=%d reads=%d", len(h.adopt.pulls), h.adopt.commentReads)
	}
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	if rec.AdoptedBy != 0 || rec.AdoptMarkerScanHead != "aaa" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestScanAdoptionMarkers(t *testing.T) {
	if _, ok := ParseAdoptionMarker(AdoptionMarkerPrefix + " by=hive reason=x mode=in-place head=aaa -->"); ok {
		t.Fatal("in-place marker parsed as supersede")
	}
	m, ok := ParseAdoptionMarker("  " + AdoptionMarkerPrefix + " by=hive reason=contributor-offline mode=supersede superseded_by=14977 head=bc3a6ea3ac36 -->\ntext")
	if !ok || m.SupersededBy != 14977 || m.Head != "bc3a6ea3ac36" {
		t.Fatalf("marker = %+v ok=%v", m, ok)
	}
	if _, ok := ParseAdoptionMarker("Superseded by #14977"); ok {
		t.Fatal("plain text parsed as a marker")
	}
}

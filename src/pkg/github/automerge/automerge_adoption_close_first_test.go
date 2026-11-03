package automerge

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Close-first supersession (hivecommons/hive#15135): the original closes as
// soon as the hive's copy exists, and is reopened if the copy fails.

func closeFirstPolicy() StageLabelPolicy {
	p := adoptPolicy()
	p.AdoptSupersedeCloseFirst = true
	return p
}

// closeFirstHarness is the adopt harness with every write to the fake API
// logged in order (method + path), across both the stage and adopt routes.
type closeFirstHarness struct {
	*adoptHarness
	writes []string
}

func newCloseFirstHarness(t *testing.T, policy StageLabelPolicy) (*closeFirstHarness, func()) {
	t.Helper()
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, policy)
	cf := &closeFirstHarness{adoptHarness: h}
	inner := h.api.extra
	h.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			cf.writes = append(cf.writes, r.Method+" "+r.URL.Path)
		}
		return inner(w, r)
	}
	return cf, done
}

// adoptCloseFirst runs #7 (head aaa) through nudge → grace → adoption and
// returns the result of the adopting pass.
func (h *closeFirstHarness) adoptCloseFirst() StageLabelResult {
	h.t.Helper()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.clock.advance(time.Hour)
	h.api.reset()
	h.writes = nil
	return h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
}

// sweep runs a pass with no observation of #7 — a closed original is in no
// lane's listing.
func (h *closeFirstHarness) sweep() StageLabelResult {
	h.t.Helper()
	res, err := h.e.ApplyStageLabels(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *closeFirstHarness) record() stagelabels.Record {
	rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
	return rec
}

func writesMatching(writes []string, want ...string) []string {
	var out []string
	for _, w := range writes {
		for _, m := range want {
			if strings.HasPrefix(w, m) {
				out = append(out, w)
				break
			}
		}
	}
	return out
}

func TestAdoptionCloseFirstOrdering(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	res := h.adoptCloseFirst()
	if len(h.adopt.pulls) != 1 {
		t.Fatalf("pulls=%d", len(h.adopt.pulls))
	}
	// push branch → open copy → note → label → close, in one pass.
	got := writesMatching(h.writes, "POST /repos/acme/widget/git/refs", "POST /repos/acme/widget/pulls", "POST /repos/acme/widget/issues/7/comments", "POST /repos/acme/widget/issues/7/labels", "PATCH /repos/acme/widget/pulls/7")
	want := []string{"POST /repos/acme/widget/git/refs", "POST /repos/acme/widget/pulls", "POST /repos/acme/widget/issues/7/comments", "POST /repos/acme/widget/issues/7/labels", "PATCH /repos/acme/widget/pulls/7"}
	// The lane's own 074 label on the original follows; nothing else does.
	if len(got) < len(want) || strings.Join(got[:len(want)], "\n") != strings.Join(want, "\n") || len(writesMatching(got[len(want):], "PATCH", "POST /repos/acme/widget/issues/7/comments")) != 0 {
		t.Fatalf("write order:\n%s\nall: %v", strings.Join(got, "\n"), h.writes)
	}
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed: %v", h.adopt.prEdits)
	}
	if res.Comments < 1 {
		t.Fatalf("res=%+v", res)
	}
	if added := h.addedLabels(); len(added) != 2 || added[0] != hgithub.SupersededLabel || added[1] != "stage/074-adopted-by-hive" {
		t.Fatalf("superseded label: %v", h.api.added)
	}
	rec := h.record()
	if !rec.SupersededClosedFirst || rec.SupersededBy != 90 || rec.AdoptedBy != 90 || rec.SupersededOpen || !rec.Superseded() {
		t.Fatalf("record = %+v", rec)
	}
	if !rec.CloseFirstPollDue(h.clock.now().Add(adoptCheckInterval), adoptCheckInterval) || rec.SupersederPollDue(h.clock.now().Add(adoptCheckInterval), adoptCheckInterval) {
		t.Fatalf("poll gates: %+v", rec)
	}
}

func TestAdoptionCloseFirstCommentText(t *testing.T) {
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa"}
	cases := []struct {
		name, reason, want string
	}{
		{"grace", AdoptReasonGrace, "because CI stayed red and the author was unavailable"},
		{"offline", AdoptReasonOffline, "because the author's clanker is offline"},
		{"unregistered", AdoptReasonUnregistered, "because the author has no registered clanker here and is not a trusted author"},
		{"operator", AdoptReasonOperator, "because an operator asked for it"},
		{"rehome", AdoptReasonDCORehome, "because its commits needed a sign-off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := AdoptionCloseFirstComment(obs, "acme-hive", 90, tc.reason)
			for _, want := range []string{AdoptionMarkerPrefix, "mode=supersede", "superseded_by=90", "head=aaa", "close_first=true", "Superseded by hive: re-authored as #90 with the same commits and sign-off ", tc.want, "This PR is closed; if #90 fails, hive reopens it."} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %q in\n%s", want, body)
				}
			}
			if strings.Contains(body, "closed automatically once") {
				t.Fatalf("keep-open wording in close-first note:\n%s", body)
			}
		})
	}
}

func TestAdoptionCloseFirstOffKeepsOriginalOpen(t *testing.T) {
	h, done := newCloseFirstHarness(t, adoptPolicy())
	defer done()
	h.adoptCloseFirst()
	if len(h.adopt.prEdits) != 0 {
		t.Fatalf("closed with close_first off: %v", h.adopt.prEdits)
	}
	if rec := h.record(); rec.SupersededClosedFirst || rec.Superseded() {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAdoptionCloseFirstCloseRefusedLeavesOpenNoted(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	h.adopt.failEditStatus = http.StatusForbidden
	h.adoptCloseFirst()
	rec := h.record()
	if !rec.SupersededClosedFirst || !rec.SupersededOpen || rec.SupersededBy != 90 {
		t.Fatalf("record = %+v", rec)
	}
	if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "close_first=true") {
		t.Fatalf("comments: %v", h.api.comments)
	}
}

func TestAdoptionCloseFirstCopyMergedSettles(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	h.adoptCloseFirst()
	h.adopt.setSuperseder(90, "closed", true)
	h.api.reset()
	h.writes = nil
	h.clock.advance(adoptCheckInterval)
	res := h.sweep()
	if h.api.writes() != 0 || len(h.adopt.prEdits) != 1 || res.Comments != 0 {
		t.Fatalf("writes on a merged copy: %v %v res=%+v", h.writes, h.api.comments, res)
	}
	rec := h.record()
	if rec.SupersededClosedFirst || !rec.Superseded() || rec.SupersededBy != 90 {
		t.Fatalf("record = %+v", rec)
	}
	h.clock.advance(adoptCheckInterval)
	h.sweep()
	if len(h.writes) != 0 {
		t.Fatalf("polled after settling: %v", h.writes)
	}
}

func TestAdoptionCloseFirstReopenOnFailure(t *testing.T) {
	cases := []struct {
		name string
		// fail arranges the copy's failure after adoption; it returns the
		// number of sweeps (adoptCheckInterval apart) that must run.
		fail     func(h *closeFirstHarness) int
		why      string
		wantNote string
	}{
		{
			name: "copy closed unmerged",
			fail: func(h *closeFirstHarness) int {
				h.adopt.setSuperseder(90, "closed", false)
				return 1
			},
			why:      adoptionCopyFailedClosed,
			wantNote: "closed without merging",
		},
		{
			name: "copy red at two heads",
			fail: func(h *closeFirstHarness) int {
				h.adopt.setSupersederHead(90, "open", false, "c1")
				h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", nudgePR(90, "c1"), "required-check-failing:unit", false, nil)
				h.sweep() // first red head: noted, no reopen
				if len(h.adopt.prEdits) != 1 {
					h.t.Fatalf("reopened at the first red head: %v", h.adopt.prEdits)
				}
				h.clock.advance(adoptCheckInterval)
				h.sweep() // same head again: still one
				if rec := h.record(); len(rec.AdoptCopyRedHeads) != 1 || len(h.adopt.prEdits) != 1 {
					h.t.Fatalf("same red head counted twice: %+v edits=%v", rec, h.adopt.prEdits)
				}
				h.clock.advance(adoptCheckInterval)
				h.adopt.setSupersederHead(90, "open", false, "c2")
				h.e.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", nudgePR(90, "c2"), "required-check-failing:unit", false, nil)
				return 1
			},
			why:      adoptionCopyFailedRed,
			wantNote: "required checks red on 2 heads",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newCloseFirstHarness(t, closeFirstPolicy())
			defer done()
			h.adoptCloseFirst()
			h.api.reset()
			h.writes = nil
			h.clock.advance(adoptCheckInterval)
			sweeps := tc.fail(h)
			h.api.reset()
			h.writes = nil
			var res StageLabelResult
			for i := 0; i < sweeps; i++ {
				res = h.sweep()
			}
			if len(h.adopt.prEdits) != 2 || h.adopt.prEdits[1] != "7:open" {
				t.Fatalf("original not reopened: %v", h.adopt.prEdits)
			}
			notes := commentsWithPrefix(h.api, AdoptionCopyFailedMarker)
			if len(notes) != 1 || res.Comments != 1 {
				t.Fatalf("reopen note: %v res=%+v", h.api.comments, res)
			}
			for _, want := range []string{"Hive's copy #90 failed (", tc.wantNote, "reopening this PR; needs a person", "why=" + tc.why} {
				if !strings.Contains(notes[0], want) {
					t.Fatalf("note missing %q:\n%s", want, notes[0])
				}
			}
			if len(h.api.removed) != 1 || h.api.removed[0] != hgithub.SupersededLabel {
				t.Fatalf("superseded label not removed: %v", h.api.removed)
			}
			// reopen → note → label off.
			got := writesMatching(h.writes, "PATCH /repos/acme/widget/pulls/7", "POST /repos/acme/widget/issues/7/comments", "DELETE /repos/acme/widget/issues/7/labels/")
			if len(got) != 3 || !strings.HasPrefix(got[0], "PATCH") || !strings.HasPrefix(got[1], "POST") || !strings.HasPrefix(got[2], "DELETE") {
				t.Fatalf("reopen write order: %v", got)
			}
			rec := h.record()
			if rec.Superseded() || rec.SupersededClosedFirst || !rec.AdoptionCopyFailed() || rec.AdoptReopenedFrom != 90 || rec.AdoptEscalatedReason != stagelabels.LaneReasonAdoptionCopyFailed || !rec.AdoptionEscalated("aaa") {
				t.Fatalf("record after reopen = %+v", rec)
			}
			// The reopened original comes back into the lane: parked at 085
			// with the copy-failed reason, no fresh copy, no second reopen.
			h.api.reset()
			h.writes = nil
			h.clock.advance(adoptCheckInterval)
			h.observe(adoptPR(7, "aaa", "stage/074-adopted-by-hive", "role/contributor", "author/clanker"), "check-failing:unit")
			if added := h.addedLabels(); len(added) != 1 || added[0] != "stage/085-needs-decision" {
				t.Fatalf("085 after reopen: added=%v removed=%v", h.api.added, h.api.removed)
			}
			if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 2 {
				t.Fatalf("re-adopted after reopen: pulls=%d edits=%v", len(h.adopt.pulls), h.adopt.prEdits)
			}
		})
	}
}

func TestAdoptionCloseFirstReopenPingPongGuard(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	h.adoptCloseFirst()
	h.adopt.setSuperseder(90, "closed", false)
	h.clock.advance(adoptCheckInterval)
	h.sweep()
	if len(h.adopt.prEdits) != 2 || h.adopt.prEdits[1] != "7:open" {
		t.Fatalf("first reopen: %v", h.adopt.prEdits)
	}
	// An operator asks again within the window: a fresh copy, closed
	// first; when that copy fails too the original stays closed and the
	// note says why.
	h.api.reset()
	h.clock.advance(time.Hour)
	h.observe(adoptPR(7, "aaa", "stage/085-needs-decision", "role/contributor", "author/clanker", delegatedecision.LabelAdoptSupersede), "check-failing:unit")
	if len(h.adopt.pulls) != 2 || len(h.adopt.prEdits) != 3 || h.adopt.prEdits[2] != "7:closed" {
		t.Fatalf("operator re-adoption: pulls=%d edits=%v comments=%v", len(h.adopt.pulls), h.adopt.prEdits, h.api.comments)
	}
	if rec := h.record(); !rec.SupersededClosedFirst || rec.SupersededBy != 91 || rec.AdoptReopenedAt.IsZero() {
		t.Fatalf("record after re-adoption = %+v", rec)
	}
	h.adopt.setSuperseder(91, "closed", false)
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.sweep()
	if len(h.adopt.prEdits) != 3 {
		t.Fatalf("reopened twice within %v: %v", stagelabels.AdoptionReopenWindow, h.adopt.prEdits)
	}
	notes := commentsWithPrefix(h.api, AdoptionCopyFailedMarker)
	if len(notes) != 1 || res.Comments != 1 || !strings.Contains(notes[0], "#91") || !strings.Contains(notes[0], "not reopening it again") || !strings.Contains(notes[0], "held=true") {
		t.Fatalf("held note: %v res=%+v", h.api.comments, res)
	}
	if len(h.api.removed) != 0 {
		t.Fatalf("label removed while held: %v", h.api.removed)
	}
	rec := h.record()
	if rec.SupersededClosedFirst || !rec.Superseded() || rec.SupersededBy != 91 {
		t.Fatalf("record after held = %+v", rec)
	}
	// Past the window the guard no longer applies.
	if !rec.AdoptionReopenedWithin(h.clock.now(), stagelabels.AdoptionReopenWindow) || rec.AdoptionReopenedWithin(h.clock.now().Add(stagelabels.AdoptionReopenWindow), stagelabels.AdoptionReopenWindow) {
		t.Fatalf("reopen window: %+v now=%v", rec, h.clock.now())
	}
}

// closeFirstGateState is what a test flips on the write-back gate it
// installs: the contributor_adopt kind refused (own-only) and an operator
// repo pause.
type closeFirstGateState struct {
	adoptOff bool
	paused   bool
	// requireLabel keeps the contributor_adopt kind's default opt-in label
	// requirement (the live hive's needs-opt-in-label refusal on #15526).
	requireLabel bool
}

// installWritebackGate puts a write-back gate on the harness's client at
// L6 with stage labels allowed unlabelled, so only the adoption kind (and
// the pause) decide.
func (h *closeFirstHarness) installWritebackGate(st *closeFirstGateState) *writeback.Gate {
	h.t.Helper()
	gate := writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor:   func(string) int { return 6 },
		RepoListed: func(string) bool { return true },
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "ops", Reason: "freeze"}, st.paused
		},
		Config: func() writeback.Config {
			one, no, yes, ownOnly, adoptLabel := 1, false, true, st.adoptOff, st.requireLabel
			return writeback.Config{
				Unsolicited: writeback.UnsolicitedConfig{Backfill: &yes},
				Kinds: map[string]writeback.KindPolicyOverride{
					string(writeback.KindStageLabel):       {MinLevel: &one, RequireLabel: &no},
					string(writeback.KindContributorAdopt): {MinLevel: &one, RequireLabel: &adoptLabel, OwnOnly: &ownOnly},
				},
			}
		},
		Now: h.clock.now,
	}, "")
	client, ok := h.e.transport.(*hgithub.Client)
	if !ok {
		h.t.Fatalf("transport %T is not *hgithub.Client", h.e.transport)
	}
	client.SetWritebackGate(gate)
	return gate
}

func suppressedFor(gate *writeback.Gate, number int, kind writeback.Kind) []writeback.Suppressed {
	var out []writeback.Suppressed
	for _, s := range gate.Recent() {
		if s.Number == number && s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// The close-first note, label and close on a contributor's original are
// unsolicited writes: the write-back gate decides them (#15272).
func TestAdoptionCloseFirstWritebackGate(t *testing.T) {
	t.Run("refused leaves the original open on the keep-open path", func(t *testing.T) {
		h, done := newCloseFirstHarness(t, closeFirstPolicy())
		defer done()
		gate := h.installWritebackGate(&closeFirstGateState{adoptOff: true})
		h.adoptCloseFirst()
		if len(h.adopt.pulls) != 1 {
			t.Fatalf("copy not opened: pulls=%d", len(h.adopt.pulls))
		}
		if len(h.adopt.prEdits) != 0 {
			t.Fatalf("original closed through a refusing gate: %v", h.adopt.prEdits)
		}
		for _, c := range h.api.comments {
			if strings.Contains(c, "close_first=true") {
				t.Fatalf("close-first note posted through a refusing gate: %v", h.api.comments)
			}
		}
		for _, l := range h.addedLabels() {
			if l == hgithub.SupersededLabel {
				t.Fatalf("superseded label added through a refusing gate: %v", h.api.added)
			}
		}
		rec := h.record()
		if rec.SupersededClosedFirst || rec.AdoptedBy != 90 {
			t.Fatalf("record = %+v", rec)
		}
		if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 1 || s[0].Reason != writeback.ReasonOwnOnly {
			t.Fatalf("suppressed = %+v", gate.Recent())
		}
	})
	t.Run("allowed closes the original first", func(t *testing.T) {
		h, done := newCloseFirstHarness(t, closeFirstPolicy())
		defer done()
		gate := h.installWritebackGate(&closeFirstGateState{})
		h.adoptCloseFirst()
		if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
			t.Fatalf("original not closed: %v", h.adopt.prEdits)
		}
		if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "close_first=true") {
			t.Fatalf("comments: %v", h.api.comments)
		}
		if rec := h.record(); !rec.SupersededClosedFirst || rec.SupersededBy != 90 {
			t.Fatalf("record = %+v", rec)
		}
		if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 0 {
			t.Fatalf("suppressed = %+v", s)
		}
	})
}

// hivecommons/hive#15526: with the contributor_adopt kind requiring a repo
// opt-in label (the default), an agent-authored original the hive has
// re-authored closes on adopt by policy (close_superseded: on_adopt,
// writeback.ReasonSupersededAgentPR); on_merge leaves it to the merge-time
// close. A person's original still needs the label (TestCloseFirstGateItem
// and the gate's own decision table).
func TestAdoptionCloseFirstSupersededAgentPolicy(t *testing.T) {
	run := func(t *testing.T, policy StageLabelPolicy) (*closeFirstHarness, *writeback.Gate, func()) {
		t.Helper()
		h, done := newCloseFirstHarness(t, policy)
		gate := h.installWritebackGate(&closeFirstGateState{requireLabel: true})
		h.adoptCloseFirst()
		return h, gate, done
	}
	t.Run("clanker original closes on adopt without the label", func(t *testing.T) {
		h, gate, done := run(t, closeFirstPolicy())
		defer done()
		if len(h.adopt.pulls) != 1 {
			t.Fatalf("copy not opened: pulls=%d", len(h.adopt.pulls))
		}
		if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
			t.Fatalf("original not closed: %v", h.adopt.prEdits)
		}
		if c := adoptionComments(h.api); len(c) != 1 || !strings.Contains(c[0], "close_first=true") || !strings.Contains(c[0], "same commits and sign-off") {
			t.Fatalf("comments: %v", h.api.comments)
		}
		if rec := h.record(); !rec.SupersededClosedFirst || rec.SupersededBy != 90 || rec.SupersededOpen {
			t.Fatalf("record = %+v", rec)
		}
		if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 0 {
			t.Fatalf("suppressed = %+v", s)
		}
	})
	t.Run("on_merge leaves a clanker original open until the copy merges", func(t *testing.T) {
		p := closeFirstPolicy()
		p.AdoptCloseSuperseded = config.AdoptionCloseSupersededOnMerge
		h, gate, done := run(t, p)
		defer done()
		if len(h.adopt.pulls) != 1 || len(h.adopt.prEdits) != 0 {
			t.Fatalf("pulls=%d edits=%v", len(h.adopt.pulls), h.adopt.prEdits)
		}
		if c := adoptionComments(h.api); len(c) != 0 {
			t.Fatalf("close-first note posted under on_merge: %v", c)
		}
		rec := h.record()
		if rec.SupersededClosedFirst || rec.AdoptedBy != 90 {
			t.Fatalf("record = %+v", rec)
		}
		// Not a gate refusal: nothing was asked of the gate.
		if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 0 {
			t.Fatalf("suppressed = %+v", s)
		}
	})
}

// closeFirstGateItem: only an agent-authored original names the copy for the
// gate; a person's never does, whatever close_superseded says.
func TestCloseFirstGateItem(t *testing.T) {
	obs := func(kind authorkind.Kind) stageObservation {
		return stageObservation{Number: 7, Author: "odttlnt", AuthorKind: kind, Labels: []string{"stage/072-returned-to-contributor"}}
	}
	onAdopt, onMerge := closeFirstPolicy(), closeFirstPolicy()
	onMerge.AdoptCloseSuperseded = config.AdoptionCloseSupersededOnMerge
	for _, tc := range []struct {
		name    string
		kind    authorkind.Kind
		policy  StageLabelPolicy
		agent   bool
		by      int
		onMerge bool
	}{
		{"clanker default", authorkind.Clanker, onAdopt, true, 90, false},
		{"hive default", authorkind.Hive, onAdopt, true, 90, false},
		{"clanker on_merge", authorkind.Clanker, onMerge, false, 0, true},
		{"human default", authorkind.Human, onAdopt, false, 0, false},
		{"human on_merge", authorkind.Human, onMerge, false, 0, false},
		{"unknown default", authorkind.Unknown, onAdopt, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item, onMerge := closeFirstGateItem(obs(tc.kind), tc.policy, 90)
			if item.AgentAuthored != tc.agent || item.SupersededBy != tc.by || onMerge != tc.onMerge || item.Number != 7 || item.Author != "odttlnt" || !item.IsPR {
				t.Fatalf("item=%+v onMerge=%v", item, onMerge)
			}
		})
	}
}

// A paused repo gets no reopen while paused; the record stays due its poll
// and the reopen lands once the pause lifts.
func TestAdoptionCloseFirstReopenWaitsForWritebackGate(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	st := &closeFirstGateState{}
	gate := h.installWritebackGate(st)
	h.adoptCloseFirst()
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed: %v", h.adopt.prEdits)
	}
	h.adopt.setSuperseder(90, "closed", false)
	st.paused = true
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.sweep()
	if len(h.adopt.prEdits) != 1 || res.Comments != 0 || len(commentsWithPrefix(h.api, AdoptionCopyFailedMarker)) != 0 || len(h.api.removed) != 0 {
		t.Fatalf("wrote while paused: edits=%v comments=%v removed=%v res=%+v", h.adopt.prEdits, h.api.comments, h.api.removed, res)
	}
	if rec := h.record(); !rec.SupersededClosedFirst || rec.AdoptionCopyFailed() {
		t.Fatalf("record settled while paused = %+v", rec)
	}
	if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 1 || s[0].Reason != writeback.ReasonRepoPaused {
		t.Fatalf("suppressed = %+v", gate.Recent())
	}

	st.paused = false
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res = h.sweep()
	if len(h.adopt.prEdits) != 2 || h.adopt.prEdits[1] != "7:open" {
		t.Fatalf("original not reopened after the pause lifted: %v", h.adopt.prEdits)
	}
	if notes := commentsWithPrefix(h.api, AdoptionCopyFailedMarker); len(notes) != 1 || res.Comments != 1 {
		t.Fatalf("reopen note: %v res=%+v", h.api.comments, res)
	}
}

// A policy refusal (not a pause) after the hive closed the original is
// warned, not merely logged at info: the PR the hive shut stays closed
// until the contributor_adopt policy allows the reopen or a person acts.
func TestAdoptionCloseFirstReopenPolicyRefusalWarns(t *testing.T) {
	h, done := newCloseFirstHarness(t, closeFirstPolicy())
	defer done()
	st := &closeFirstGateState{}
	gate := h.installWritebackGate(st)
	h.adoptCloseFirst()
	if len(h.adopt.prEdits) != 1 || h.adopt.prEdits[0] != "7:closed" {
		t.Fatalf("original not closed: %v", h.adopt.prEdits)
	}
	h.adopt.setSuperseder(90, "closed", false)
	st.adoptOff = true
	var logs bytes.Buffer
	h.e.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	h.api.reset()
	h.clock.advance(adoptCheckInterval)
	res := h.sweep()
	if len(h.adopt.prEdits) != 1 || res.Comments != 0 || len(commentsWithPrefix(h.api, AdoptionCopyFailedMarker)) != 0 {
		t.Fatalf("wrote through a refusing gate: edits=%v comments=%v res=%+v", h.adopt.prEdits, h.api.comments, res)
	}
	if rec := h.record(); !rec.SupersededClosedFirst || rec.AdoptionCopyFailed() {
		t.Fatalf("record settled through a refusing gate = %+v", rec)
	}
	if s := suppressedFor(gate, 7, writeback.KindContributorAdopt); len(s) != 1 || s[0].Reason != writeback.ReasonOwnOnly {
		t.Fatalf("suppressed = %+v", gate.Recent())
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "hive-closed PR stays closed") || !strings.Contains(out, "writeback_reason="+writeback.ReasonOwnOnly) {
		t.Fatalf("policy refusal not warned: %q", out)
	}
}

func TestAdoptionCopyFailedMarkerIsSettled(t *testing.T) {
	if !AdoptionSettledMarker(AdoptionCopyFailedComment(7, 90, "aaa", adoptionCopyFailedClosed, 0)) {
		t.Fatal("reopen note not a settled marker: the marker scan would re-adopt")
	}
}

func TestSplitStageKey(t *testing.T) {
	cases := []struct {
		key         string
		owner, repo string
		number      int
		ok          bool
	}{
		{"acme/widget#7", "acme", "widget", 7, true},
		{"acme/widget", "", "", 0, false},
		{"widget#7", "", "", 0, false},
		{"acme/widget#x", "", "", 0, false},
		{"acme/widget#0", "", "", 0, false},
	}
	for _, tc := range cases {
		owner, repo, number, ok := splitStageKey(tc.key)
		if owner != tc.owner || repo != tc.repo || number != tc.number || ok != tc.ok {
			t.Errorf("splitStageKey(%q) = %q %q %d %v", tc.key, owner, repo, number, ok)
		}
	}
}

func TestAdoptReasonOperatorText(t *testing.T) {
	if !strings.Contains(adoptReasonText(AdoptReasonOperator, time.Hour, false, adoptionWait{}), "maintainer asked") || !strings.Contains(adoptionBodyCause(AdoptReasonOperator, adoptionWait{}), "maintainer's request") {
		t.Fatalf("%q / %q", adoptReasonText(AdoptReasonOperator, time.Hour, false, adoptionWait{}), adoptionBodyCause(AdoptReasonOperator, adoptionWait{}))
	}
}

package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A pushable adopted fork first reaches the existing rebase fixer. If that
// window expires, re-home in the same sweep rather than parking it at 085.
func TestAdoptedNeedsRebaseSupersedesInsteadOfStalling(t *testing.T) {
	for _, parked := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "previously-stalled"}[parked], func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			key := adoptInPlaceAt(h, "aaa", "label:needs-rebase")
			labels := append(append([]string{}, adoptedLabels...), "needs-rebase")
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), "label:needs-rebase")
			if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.Stage != stagelabels.AdoptedByHive || len(h.adopt.pulls) != 0 {
				t.Fatalf("fresh adoption must stay available to rebase fixer: %+v", rec)
			}
			if parked {
				store := h.e.stageStoreOrShared()
				reason := stagelabels.LaneReasonAdoptionStalledPrefix + "label:needs-rebase"
				store.MarkAdoptionEscalated(key, "aaa", reason)
				store.Observe(key, stagelabels.NeedsDecision, reason)
				labels[0] = stagelabels.NeedsDecision.Label()
			}
			h.clock.advance(adoptedDwell)
			h.api.reset()
			h.observe(adoptPR(7, "aaa", labels...), "label:needs-rebase")
			rec, _ := h.e.stageStoreOrShared().Get(key)
			if rec.AdoptedBy != 90 || rec.AdoptReason != AdoptReasonRebaseRehome || rec.Stage != stagelabels.AdoptedByHive || len(h.adopt.pulls) != 1 || len(stalledComments(h.api)) != 0 {
				t.Fatalf("must supersede, not park: rec=%+v pulls=%v comments=%v", rec, h.adopt.pulls, h.api.comments)
			}
			if h.adopt.refs["refs/heads/hive/adopt-7"] != "aaa" {
				t.Fatalf("authorship/signoffs must survive unchanged: refs=%v", h.adopt.refs)
			}
			body, _ := h.adopt.pulls[0]["body"].(string)
			if !strings.Contains(body, "rebase this branch") || !strings.Contains(body, "Keep the original author's commits and their `Signed-off-by`") {
				t.Fatalf("missing rebase/preservation instructions: %s", body)
			}
		})
	}
}

func TestAdoptionRebaseRehomeRespectsEvidenceAndHold(t *testing.T) {
	for _, mode := range []string{"head-moved", "hold", "pending", "fence", "push-fence"} {
		t.Run(mode, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			key := adoptInPlaceAt(h, "aaa", "label:needs-rebase")
			labels := append([]string{}, adoptedLabels...)
			reason := "label:needs-rebase"
			switch mode {
			case "head-moved":
				h.adopt.liveHead = "bbb"
			case "hold":
				labels = append(labels, "hold")
			case "pending":
				labels = append(labels, "needs-rebase")
				reason = "check-pending"
			case "push-fence":
				reason = "check-pending"
				h.adopt.allowMaintainerEdits(7, false)
				fallthrough
			case "fence":
				h.adopt.prFiles = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1}}
			}
			h.clock.advance(adoptedDwell + time.Minute)
			h.observe(adoptPR(7, "aaa", labels...), reason)
			if h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.prEdits) != 0 || len(adoptionComments(h.api)) != 0 {
				t.Fatalf("unsafe re-home: refs=%v pulls=%v edits=%v comments=%v", h.adopt.refs, h.adopt.pulls, h.adopt.prEdits, h.api.comments)
			}
			rec, _ := h.e.stageStoreOrShared().Get(key)
			if !rec.AdoptedInPlace() || rec.AdoptReason != AdoptReasonGrace {
				t.Fatalf("refused re-home changed the adoption: %+v", rec)
			}
			if mode != "fence" && mode != "push-fence" {
				if rec.AdoptDiffFence != "" || len(workflowRefusedComments(h.api)) != 0 {
					t.Fatalf("deferred re-home recorded a verdict: rec=%+v comments=%v", rec, h.api.comments)
				}
				return
			}
			// A workflow-path refusal is terminal for the head: one note,
			// routed to 085, and the next sweep neither re-reads nor re-posts.
			if rec.AdoptDiffFence != contributorLaneReasonWorkflowPath || !rec.AdoptionEscalated("aaa") || rec.AdoptEscalatedReason != adoptionWorkflowRefusedReason || rec.Stage != stagelabels.NeedsDecision || len(workflowRefusedComments(h.api)) != 1 {
				t.Fatalf("workflow refusal not terminal: rec=%+v comments=%v", rec, h.api.comments)
			}
			h.api.reset()
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "aaa", append([]string{stagelabels.NeedsDecision.Label()}, labels[1:]...)...), reason)
			rec, _ = h.e.stageStoreOrShared().Get(key)
			if h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 || len(workflowRefusedComments(h.api)) != 0 || len(stalledComments(h.api)) != 0 || rec.Stage != stagelabels.NeedsDecision {
				t.Fatalf("refused re-home looped: rec=%+v refs=%d comments=%v", rec, h.adopt.refAttempts, h.api.comments)
			}
		})
	}
}

func TestAdoptedPushAccessUnknownOrWorkflowDriftStaysInPlace(t *testing.T) {
	for _, mode := range []string{"get-error", "compare-error", "workflow-drift", "missing-access-field"} {
		t.Run(mode, func(t *testing.T) {
			p := adoptPolicy()
			p.AdoptInPlace = true
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
			defer done()
			key := adoptInPlaceAt(h, "aaa", "label:needs-rebase")
			switch mode {
			case "get-error":
				h.adopt.failPRGet = true
			case "compare-error":
				h.adopt.failCompare = true
			case "workflow-drift":
				h.adopt.compareFiles = []string{".github/workflows/ci.yml"}
			case "missing-access-field":
				delete(h.adopt.superseders[7], "maintainer_can_modify")
			}
			h.clock.advance(time.Minute)
			h.observe(adoptPR(7, "aaa", adoptedLabels...), "check-pending")
			if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.AdoptedInPlace() || rec.AdoptedBy != 0 || h.adopt.refAttempts != 0 || len(h.adopt.pulls) != 0 {
				t.Fatalf("unknown access or base drift must not supersede: rec=%+v pulls=%v", rec, h.adopt.pulls)
			}
		})
	}
}

func TestAdoptedLostPushAccessSupersedesImmediately(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "aaa", "label:needs-rebase")
	h.adopt.allowMaintainerEdits(7, false)
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", adoptedLabels...), "label:needs-rebase")
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.AdoptedBy != 90 || rec.AdoptReason != AdoptReasonPushRehome || rec.Stage != stagelabels.AdoptedByHive {
		t.Fatalf("push denial must not wait for dwell or human: %+v", rec)
	}
	if len(stalledComments(h.api)) != 0 {
		t.Fatalf("unexpected stall: %v", h.api.comments)
	}
}

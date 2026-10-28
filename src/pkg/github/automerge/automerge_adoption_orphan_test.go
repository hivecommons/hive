package automerge

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestOrphanRecoversAt085AndParksSecondClosedCopy(t *testing.T) {
	h, done := adoptThenLeave074(t)
	defer done()
	key := stageKey("acme", "widget", 7)
	h.adopt.setSuperseder(90, "closed", false)
	h.api.reset()
	labels := []string{"stage/085-needs-decision", "needs-human", "role/contributor", "author/clanker"}
	h.observe(adoptPR(7, "aaa", labels...), stageReasonDecisionRequested)
	rec, _ := h.e.stageStoreOrShared().Get(key)
	if rec.AdoptedBy != 91 || len(h.adopt.pulls) != 2 {
		t.Fatalf("085 orphan not recovered: %+v", rec)
	}
	// No ten-minute polling delay; a closed replacement is noticed immediately.
	h.adopt.setSuperseder(91, "closed", false)
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), stageReasonDecisionRequested)
	rec, _ = h.e.stageStoreOrShared().Get(key)
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != adoptionChildClosedTwice || len(h.adopt.pulls) != 2 {
		t.Fatalf("loop not parked: %+v", rec)
	}
	h.api.reset()
	h.observe(adoptPR(7, "aaa", labels...), stageReasonDecisionRequested)
	if len(h.adopt.pulls) != 2 || len(h.api.comments) != 0 {
		t.Fatalf("park churned: pulls=%v notes=%v", h.adopt.pulls, h.api.comments)
	}
}

func TestOrphanRevertsChangedHeadOrFailedPublish(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{true: "head moved", false: "publish failed"}[changed], func(t *testing.T) {
			h, done := adoptThenLeave074(t)
			defer done()
			h.adopt.setSuperseder(90, "closed", false)
			head := "aaa"
			if changed {
				head = "bbb"
			} else {
				h.adopt.failRefs = true
			}
			h.api.reset()
			// Publish failure is returned, but must still release the original and
			// leave a durable, plain-language reversion marker.
			pr := adoptPR(7, head, "stage/074-adopted-by-hive", "author/clanker")
			h.adopt.heads[7] = head
			h.e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
			_, _ = h.e.ApplyStageLabels(t.Context())
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if rec.AdoptedBy != 0 || rec.AdoptOrphanRevertedHead != "aaa" {
				t.Fatalf("orphan not reverted: %+v", rec)
			}
			found := false
			for _, note := range h.api.comments {
				found = found || strings.HasPrefix(note, "<!-- hive:adoption-reverted -->")
			}
			if !found || len(h.adopt.pulls) != 1 {
				t.Fatalf("missing reversion / duplicate attempt: %v %v", h.api.comments, h.adopt.pulls)
			}
		})
	}
}

func TestOrphanMarkerRestoresLostStoreForClosedAndMergedCopy(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "merged"}[merged], func(t *testing.T) {
			h, done := adoptThenLeave074(t)
			defer done()
			h.e.stageStoreOrShared().ClearAdoption(stageKey("acme", "widget", 7))
			h.adopt.setSuperseder(90, "closed", merged)
			prior := h.api.extra
			h.api.extra = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments" {
					_ = json.NewEncoder(w).Encode([]any{map[string]any{"body": "<!-- hive:adopted by=acme-hive reason=contributor-offline mode=supersede superseded_by=90 head=aaa -->", "user": map[string]string{"login": testHiveAppBotLogin}}})
					return true
				}
				return prior(w, r)
			}
			pr := adoptPR(7, "aaa", "stage/085-needs-decision", "needs-human", "author/clanker")
			pr.Comments = gh.Ptr(1)
			h.api.reset()
			h.observe(pr, stageReasonDecisionRequested)
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			if merged {
				if rec.SupersededBy != 90 || len(h.adopt.prEdits) != 1 {
					t.Fatalf("merged marker not settled: %+v edits=%v", rec, h.adopt.prEdits)
				}
			} else if rec.AdoptedBy != 91 || len(h.adopt.pulls) != 2 {
				t.Fatalf("lost-store orphan not recovered: %+v", rec)
			}
		})
	}
}

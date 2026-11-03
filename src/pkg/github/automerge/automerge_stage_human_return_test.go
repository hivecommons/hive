package automerge

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func stageLabelEvent(event, label, actor, at string) map[string]any {
	return map[string]any{
		"id":         int64(len(label) + len(at)),
		"event":      event,
		"label":      map[string]string{"name": label},
		"actor":      map[string]string{"login": actor},
		"created_at": at,
	}
}

// TestApplyStageLabelsHonoursHandPlacedReturnOverSentinelPark is the
// hivecommons/hive#15159 log line: the contributor lane skipped
// `contributor-lane:sentinel-blocked`, the owner had swapped stage/085 for
// stage/072 by hand, and the reconciler logged `stage labels reconciled
// pr=15159 stage=085-needs-decision author_kind=clanker added=[stage/085]
// removed=[stage/072-returned-to-contributor]`. A trusted human's return
// label on the current head is that human's decision: the hive keeps 072,
// records it against the head, and only a new head re-arms the park. The
// hive's own label event and an untrusted actor change nothing.
func TestApplyStageLabelsHonoursHandPlacedReturnOverSentinelPark(t *testing.T) {
	const sentinel = "hive-sentinel"
	for _, tc := range []struct {
		name    string
		actor   string
		trusted bool
		held    bool
	}{
		{name: "trusted owner", actor: "clubanderson", trusted: true, held: true},
		{name: "untrusted actor", actor: "someone", trusted: false},
		{name: "the hive's own label", actor: testHiveAppBotLogin, trusted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			api.issueEvents = map[int][]map[string]any{15159: {
				stageLabelEvent("labeled", "stage/085-needs-decision", testHiveAppBotLogin, "2026-10-29T00:00:16Z"),
				stageLabelEvent("unlabeled", "stage/085-needs-decision", tc.actor, "2026-10-29T02:40:00Z"),
				stageLabelEvent("labeled", "stage/072-returned-to-contributor", tc.actor, "2026-10-29T02:40:05Z"),
			}}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 2, 48, 0, 0, time.UTC)}
			e, _, store := newStageEngine(t, apiURLOf(t, api), clock)
			requests := review.NewHumanChangeRequests("")
			e.humanChangeRequests = requests
			e.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })

			// Pass 1: the hive parks the PR at 085 on the sentinel alert.
			pr := nudgePR(15159, "sha7", sentinel, "role/trusted", "author/clanker")
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonSentinelBlocked, false, nil)
			e.noteStageRole("acme", "widget", 15159, stagelabels.RoleTrusted)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if rec, ok := store.Get(stageKey("acme", "widget", 15159)); !ok || rec.Stage != stagelabels.NeedsDecision {
				t.Fatalf("pass 1 record = %+v, want 085", rec)
			}

			// Pass 2: the operator swapped 085 for 072 by hand; the lane
			// still reports sentinel-blocked on the same head.
			api.reset()
			pr = nudgePR(15159, "sha7", sentinel, "role/trusted", "author/clanker", "stage/072-returned-to-contributor")
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonSentinelBlocked, false, nil)
			e.noteStageRole("acme", "widget", 15159, stagelabels.RoleTrusted)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec, _ := store.Get(stageKey("acme", "widget", 15159))
			req, recorded := requests.For("acme/widget", 15159, "sha7")
			if !tc.held {
				if !slices.Contains(api.removed, "stage/072-returned-to-contributor") || rec.Stage != stagelabels.NeedsDecision || recorded {
					t.Fatalf("%s: removed=%v record=%+v recorded=%v, want 085 re-asserted", tc.name, api.removed, rec, recorded)
				}
				return
			}
			if len(api.removed) != 0 || slices.ContainsFunc(api.added, func(l []string) bool { return slices.Contains(l, "stage/085-needs-decision") }) {
				t.Fatalf("hand-placed 072 overridden: added=%v removed=%v", api.added, api.removed)
			}
			if rec.Stage != stagelabels.ReturnedToContributor || rec.Reason != autoMergeReasonHumanChangesRequested {
				t.Fatalf("record = %+v, want 072 %s", rec, autoMergeReasonHumanChangesRequested)
			}
			if !recorded || req.Source != review.HumanChangeRequestSourceStageLabel || req.Actor != "clubanderson" || req.IsApproval() {
				t.Fatalf("recorded request = %+v ok=%v", req, recorded)
			}

			// Pass 3: same head again — held from the store, no event read.
			api.reset()
			api.issueEvents = nil
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonSentinelBlocked, false, nil)
			e.noteStageRole("acme", "widget", 15159, stagelabels.RoleTrusted)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(api.removed) != 0 || len(api.added) != 0 {
				t.Fatalf("same head flapped: added=%v removed=%v", api.added, api.removed)
			}

			// Pass 4: a new head re-arms the sentinel park.
			api.reset()
			pr = nudgePR(15159, "sha8", sentinel, "role/trusted", "author/clanker", "stage/072-returned-to-contributor")
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, contributorLaneReasonSentinelBlocked, false, nil)
			e.noteStageRole("acme", "widget", 15159, stagelabels.RoleTrusted)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			rec, _ = store.Get(stageKey("acme", "widget", 15159))
			if !slices.Contains(api.removed, "stage/072-returned-to-contributor") || rec.Stage != stagelabels.NeedsDecision {
				t.Fatalf("new head: removed=%v record=%+v, want 085 again", api.removed, rec)
			}
		})
	}
}

package automerge

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestApplyStageLabelsKeepsSupersessionParkOverConflictAndUnsignedHead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, apiURLOf(t, api), clock)
	api.commits = map[int][]string{14795: {"unsigned commit"}}
	pr := nudgePR(14795, "head", "needs-human", prstage.SupersessionPendingLabel, "needs-rebase")
	e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, "check-failing:unit", false, nil)
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "conflicting", false, nil)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 14795))
	if !ok || rec.Stage != stagelabels.NeedsDecision || rec.Reason != prstage.ReasonSupersessionPending {
		t.Fatalf("record=%+v", rec)
	}
	for _, added := range api.added {
		if slices.Contains(added, stagelabels.ReturnedToContributor.Label()) || slices.Contains(added, stagelabels.NeedsAuthor.Label()) {
			t.Fatalf("sent to author: %v", api.added)
		}
	}
	if api.commitReads != 0 {
		t.Fatalf("superseded head read for repair: %d", api.commitReads)
	}
}

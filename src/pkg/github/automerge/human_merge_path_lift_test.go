package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The automerge sweep holds a PR that touches an auto_merge.human_merge_paths
// path. When a later head drops that file (a dependency bot moving its
// workflow update to a separate PR, a contributor reverting it), the hold's
// reason is gone: the stage-label pass lifts the hive's own hold once per
// head, says so, and never touches a person's hold or a hold with a second
// source. While the hold stays, the row no longer claims a human-merge path.

const humanMergePathLiftHoldAt = "2026-11-10T16:00:00Z"

var humanMergePathLiftCleanFiles = []string{"package.json", "package-lock.json", "renovate.json", "src/index.ts"}

func newHumanMergePathLiftAPI(t *testing.T, number int, files []string, comments, events []map[string]any) *stageAPI {
	t.Helper()
	api := &stageAPI{labels: map[string]string{}, statefulComments: true}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	api.prFiles = map[int][]string{number: files}
	api.issueComments = map[int][]map[string]any{number: comments}
	api.issueEvents = map[int][]map[string]any{number: events}
	srv := newStageAPI(t, api)
	t.Cleanup(srv.Close)
	return api
}

func newHumanMergePathLiftEngine(t *testing.T, api *stageAPI) (*Engine, *stagelabels.Store) {
	t.Helper()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, api.url, clock)
	e.SetHumanMergePaths(func(string) []string { return []string{".github/workflows/**"} })
	return e, store
}

func recordHumanMergePathHeldPR(e *Engine, number int, extra ...string) {
	labels := append([]string{"hold", "needs-human", "author/human", "role/contributor", "stage/050-review-pending"}, extra...)
	pr := stagePR(number, labels...)
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
}

func appHumanMergePathHold() []map[string]any {
	return []map[string]any{holdEvent("labeled", testHiveAppBotLogin, humanMergePathLiftHoldAt)}
}

func appHumanMergePathNotice() []map[string]any {
	return []map[string]any{humanMergePathNoticeComment(testHiveAppBotLogin, "2026-11-10T16:00:03Z")}
}

func humanMergePathLiftNotices(api *stageAPI) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []string
	for _, body := range api.comments {
		if strings.HasPrefix(body, humanMergePathLiftMarker) {
			out = append(out, body)
		}
	}
	return out
}

func removedHold(api *stageAPI) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	n := 0
	for _, label := range api.removed {
		if label == "hold" {
			n++
		}
	}
	return n
}

func TestApplyStageLabels_LiftsHumanMergePathHoldWhenHeadDropsThePath(t *testing.T) {
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), appHumanMergePathHold())
	e, store := newHumanMergePathLiftEngine(t, api)
	ctx := context.Background()

	recordHumanMergePathHeldPR(e, 21)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 1 {
		t.Fatalf("hold removed %d times (removed=%v), want once", n, api.removed)
	}
	notices := humanMergePathLiftNotices(api)
	if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergePathLiftMarker+"sha -->") ||
		!strings.Contains(notices[0], "no longer touches a path in `"+hgithub.HumanMergePathsConfigKey+"` at sha") {
		t.Fatalf("lift notices = %q, want one naming the head", notices)
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 21)); ok && rec.Hold != nil {
		t.Fatalf("stored hold provenance survived the lift: %+v", rec.Hold)
	}

	// The same head seen held again on a later pass (the label read is
	// stale, or something re-held it): the notice is not repeated and the
	// hold is not taken off a second time.
	api.reset()
	recordHumanMergePathHeldPR(e, 21)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold removed again on the same head: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice repeated on the same head: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold == nil || rec.Hold.HumanMergePath() || rec.Hold.Reason != humanMergePathStaleReason {
		t.Fatalf("record hold = %+v (ok=%v), want the stale reason, not a human-merge path", rec.Hold, ok)
	}
}

func TestApplyStageLabels_KeepsHumanMergePathHoldWhileHeadTouchesThePath(t *testing.T) {
	files := append([]string{".github/workflows/release.yml"}, humanMergePathLiftCleanFiles...)
	api := newHumanMergePathLiftAPI(t, 21, files, appHumanMergePathNotice(), appHumanMergePathHold())
	e, store := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathHeldPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold removed while the head still touches a human-merge path: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted for a head that still touches the path: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || !rec.Hold.SelfImposed() || !rec.Hold.HumanMergePath() {
		t.Fatalf("record hold = %+v (ok=%v), want the hive's human-merge-path hold", rec.Hold, ok)
	}
}

// A person who took the hold over (removed the hive's and set their own) owns
// it, even though the head no longer touches a human-merge path.
func TestApplyStageLabels_HumanMergePathLiftNeverTouchesAPersonsHold(t *testing.T) {
	events := append(appHumanMergePathHold(),
		holdEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
		holdEvent("labeled", "maintainer", "2026-11-10T17:00:05Z"),
	)
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), events)
	e, store := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathHeldPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("a person's hold was lifted: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted on a person's hold: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold == nil || rec.Hold.By != "maintainer" {
		t.Fatalf("record hold = %+v (ok=%v), want attributed to the maintainer", rec.Hold, ok)
	}
}

// The hive's hold also carries the reporter-trust gate's notice (posted well
// before the hold, so the human-merge-path notice explains the label event):
// only the human-merge-path reason drops, the hold stays.
func TestApplyStageLabels_HumanMergePathLiftKeepsHoldWithASecondSource(t *testing.T) {
	comments := append([]map[string]any{reporterTrustNoticeComment("2026-11-10T12:00:00Z")}, appHumanMergePathNotice()...)
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, comments, appHumanMergePathHold())
	e, store := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathHeldPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold lifted although the reporter-trust gate still holds it: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted while another source holds the PR: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || !rec.Hold.SelfImposed() || rec.Hold.HumanMergePath() || rec.Hold.Reason != humanMergePathStaleReason {
		t.Fatalf("record hold = %+v (ok=%v), want the hive's hold without the human-merge-path claim", rec.Hold, ok)
	}
}

// The lift is an unsolicited write on a PR the hive did not open: the
// write-back gate's human_merge_path_release policy decides it.
func TestApplyStageLabels_HumanMergePathLiftHonoursTheWritebackGate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string
		lifted bool
	}{
		{name: "not opted in", lifted: false},
		{name: "opted in", labels: []string{writeback.DefaultOptInLabel}, lifted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), appHumanMergePathHold())
			e, store := newHumanMergePathLiftEngine(t, api)
			client, ok := e.transport.(*hgithub.Client)
			if !ok {
				t.Fatalf("transport is %T, want *github.Client", e.transport)
			}
			client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, ""))

			recordHumanMergePathHeldPR(e, 21, tc.labels...)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := removedHold(api) == 1 && len(humanMergePathLiftNotices(api)) == 1; got != tc.lifted {
				t.Fatalf("lifted = %v (removed=%v), want %v", got, api.removed, tc.lifted)
			}
			if !tc.lifted {
				rec, ok := store.Get(stageKey("acme", "widget", 21))
				if !ok || rec.Hold == nil || rec.Hold.HumanMergePath() || rec.Hold.Reason != humanMergePathStaleReason {
					t.Fatalf("record hold = %+v (ok=%v), want the stale reason while the gate keeps the hold", rec.Hold, ok)
				}
			}
		})
	}
}

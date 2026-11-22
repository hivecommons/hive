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

// The automerge sweep labels a PR that touches an
// auto_merge.human_merge_paths path needs-owner-merge (never `hold`: the
// PR's fixer keeps working). When a later head drops that file (a dependency
// bot moving its workflow update to a separate PR, a contributor reverting
// it), the label's reason is gone: the stage-label pass removes the hive's
// own label once per head, says so, and never touches a label a person set,
// a `hold`, or a PR another source holds.

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

// recordHumanMergePathLabelledPR records PR number wearing the
// human-merge-path label, as the contributor lane sees it: the lane stops on
// the label (auto_merge.human_merge_labels) before its gate.
func recordHumanMergePathLabelledPR(e *Engine, number int, extra ...string) {
	labels := append([]string{hgithub.HumanMergePathLabel, "author/human", "role/contributor", "stage/050-review-pending"}, extra...)
	pr := stagePR(number, labels...)
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonHumanMergeRequired, false, nil)
	e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
}

// pathLabelEvent is a label event for the human-merge-path label.
func pathLabelEvent(event, actor, at string) map[string]any {
	return labelEventFor(hgithub.HumanMergePathLabel, event, actor, at)
}

func appHumanMergePathLabel() []map[string]any {
	return []map[string]any{pathLabelEvent("labeled", testHiveAppBotLogin, humanMergePathLiftHoldAt)}
}

// appHumanMergePathHold is the App's `hold` from before the rule used its own
// label (a PR the boot migration has not reached yet).
func appHumanMergePathHold() []map[string]any {
	return []map[string]any{holdEvent("labeled", testHiveAppBotLogin, humanMergePathLiftHoldAt)}
}

// legacyHumanMergePathNoticeComment is the notice the rule posted while it
// applied `hold`.
func legacyHumanMergePathNoticeComment(login, at string) map[string]any {
	return map[string]any{
		"body":       hgithub.HumanMergePathMarker + "\nHive will not merge this PR: it changes path(s) listed in `" + hgithub.HumanMergePathsConfigKey + "` for this repo, so a person must merge it.\n\n- `.github/workflows/build.yml`\n\nHive added the `hold` label. A maintainer should review and merge this PR by hand.\n",
		"user":       map[string]string{"login": login},
		"created_at": at,
	}
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

func removedLabelCount(api *stageAPI, name string) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	n := 0
	for _, label := range api.removed {
		if label == name {
			n++
		}
	}
	return n
}

func removedHold(api *stageAPI) int { return removedLabelCount(api, "hold") }

func removedPathLabel(api *stageAPI) int { return removedLabelCount(api, hgithub.HumanMergePathLabel) }

func TestApplyStageLabels_LiftsHumanMergePathLabelWhenHeadDropsThePath(t *testing.T) {
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), appHumanMergePathLabel())
	e, store := newHumanMergePathLiftEngine(t, api)
	ctx := context.Background()

	recordHumanMergePathLabelledPR(e, 21)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 1 {
		t.Fatalf("label removed %d times (removed=%v), want once", n, api.removed)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold removed: %v; the lift never touches hold", api.removed)
	}
	notices := humanMergePathLiftNotices(api)
	if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergePathLiftMarker+"sha -->") ||
		!strings.Contains(notices[0], "no longer touches a path in `"+hgithub.HumanMergePathsConfigKey+"` at sha") ||
		!strings.Contains(notices[0], "`"+hgithub.HumanMergePathLabel+"` removed") {
		t.Fatalf("lift notices = %q, want one naming the label and the head", notices)
	}
	if rec, ok := store.Get(stageKey("acme", "widget", 21)); ok && rec.Hold != nil {
		t.Fatalf("the label was recorded as a hold: %+v", rec.Hold)
	}

	// The same head seen labelled again on a later pass (the label read is
	// stale, or the gate put it back): the notice is not repeated and the
	// label is not taken off a second time.
	api.reset()
	recordHumanMergePathLabelledPR(e, 21)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 0 {
		t.Fatalf("label removed again on the same head: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice repeated on the same head: %q", notices)
	}
}

func TestApplyStageLabels_KeepsHumanMergePathLabelWhileHeadTouchesThePath(t *testing.T) {
	files := append([]string{".github/workflows/release.yml"}, humanMergePathLiftCleanFiles...)
	api := newHumanMergePathLiftAPI(t, 21, files, appHumanMergePathNotice(), appHumanMergePathLabel())
	e, store := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathLabelledPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 0 {
		t.Fatalf("label removed while the head still touches a human-merge path: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted for a head that still touches the path: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold != nil {
		t.Fatalf("record = %+v (ok=%v), want no hold: the label is not one", rec, ok)
	}
}

// A person who took the label over (removed the hive's and set their own)
// owns it, even though the head no longer touches a human-merge path.
func TestApplyStageLabels_HumanMergePathLiftNeverTouchesAPersonsLabel(t *testing.T) {
	events := append(appHumanMergePathLabel(),
		pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
		pathLabelEvent("labeled", "maintainer", "2026-11-10T17:00:05Z"),
	)
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), events)
	e, _ := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathLabelledPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 0 {
		t.Fatalf("a person's label was removed: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted on a person's label: %q", notices)
	}
}

// A person's `hold` beside the hive's label is another source: the label
// stays (it would not unblock anything), and the `hold` is never touched.
func TestApplyStageLabels_HumanMergePathLiftKeepsTheLabelBesideAPersonsHold(t *testing.T) {
	events := append(appHumanMergePathLabel(), holdEvent("labeled", "maintainer", "2026-11-10T17:00:00Z"))
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), events)
	e, store := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathLabelledPR(e, 21, "hold")
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, h := removedPathLabel(api), removedHold(api); n != 0 || h != 0 {
		t.Fatalf("removed=%v; neither the label nor a person's hold may come off while the person holds the PR", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted while a person holds the PR: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold == nil || rec.Hold.By != "maintainer" {
		t.Fatalf("record hold = %+v (ok=%v), want the maintainer's hold", rec.Hold, ok)
	}
}

// The reporter-trust gate's notice is a second source: the label stays.
func TestApplyStageLabels_HumanMergePathLiftKeepsLabelWithASecondSource(t *testing.T) {
	comments := append([]map[string]any{reporterTrustNoticeComment("2026-11-10T12:00:00Z")}, appHumanMergePathNotice()...)
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, comments, appHumanMergePathLabel())
	e, _ := newHumanMergePathLiftEngine(t, api)

	recordHumanMergePathLabelledPR(e, 21)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedPathLabel(api); n != 0 {
		t.Fatalf("label removed although the reporter-trust gate still holds the PR: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted while another source holds the PR: %q", notices)
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
			api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), appHumanMergePathLabel())
			e, _ := newHumanMergePathLiftEngine(t, api)
			client, ok := e.transport.(*hgithub.Client)
			if !ok {
				t.Fatalf("transport is %T, want *github.Client", e.transport)
			}
			client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{LevelFor: func(string) int { return 5 }}, ""))

			recordHumanMergePathLabelledPR(e, 21, tc.labels...)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := removedPathLabel(api) == 1 && len(humanMergePathLiftNotices(api)) == 1; got != tc.lifted {
				t.Fatalf("lifted = %v (removed=%v), want %v", got, api.removed, tc.lifted)
			}
		})
	}
}

// The App's `hold` from before the rule used its own label is no longer a
// human-merge-path hold: the stage pass never lifts a `hold` (the boot
// migration swaps it), whatever the head touches.
func TestApplyStageLabels_NeverLiftsALegacyHumanMergePathHold(t *testing.T) {
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, appHumanMergePathNotice(), appHumanMergePathHold())
	e, store := newHumanMergePathLiftEngine(t, api)

	pr := stagePR(21, "hold", "author/human", "role/contributor", "stage/050-review-pending")
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold removed: %v", api.removed)
	}
	if notices := humanMergePathLiftNotices(api); len(notices) != 0 {
		t.Fatalf("lift notice posted for a hold: %q", notices)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold == nil || !rec.Hold.SelfImposed() || rec.Hold.HumanMergePath() {
		t.Fatalf("record hold = %+v (ok=%v), want the hive's hold with no human-merge-path claim", rec.Hold, ok)
	}
}

// The same legacy `hold` beside the notice from when the rule applied `hold`
// still reads as the rule (a person's merge) until the boot migration swaps
// it, and the stage pass still never removes it.
func TestApplyStageLabels_LegacyHumanMergePathHoldKeepsItsProvenance(t *testing.T) {
	notice := []map[string]any{legacyHumanMergePathNoticeComment(testHiveAppBotLogin, "2026-11-10T16:00:03Z")}
	api := newHumanMergePathLiftAPI(t, 21, humanMergePathLiftCleanFiles, notice, appHumanMergePathHold())
	e, store := newHumanMergePathLiftEngine(t, api)

	pr := stagePR(21, "hold", "author/human", "role/contributor", "stage/050-review-pending")
	pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "excluded-label:hold", false, nil)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := removedHold(api); n != 0 {
		t.Fatalf("hold removed: %v", api.removed)
	}
	rec, ok := store.Get(stageKey("acme", "widget", 21))
	if !ok || rec.Hold == nil || !rec.Hold.SelfImposed() || !rec.Hold.HumanMergePath() {
		t.Fatalf("record hold = %+v (ok=%v), want the rule's legacy hold", rec.Hold, ok)
	}
}

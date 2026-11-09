package automerge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// forkCanaryAnnotation is the Canary annotation on
// projectbluefin/utah-packages#972's head d24a12f (check run 129228619506).
const forkCanaryAnnotation = "a fork pull request cannot publish the canary image, and no maintainer-run Canary has passed on d24a12f72ab0bacca915f12099aa7fa566839887, or on an earlier head that this one only merged main into without main changing a pipeline path (the notices above say why each does not carry over). This is expected, not something the author can fix: a maintainer reviews the change, then runs the 'Canary for a fork pull request' workflow (Actions tab) with this pull request's number and head commit. This check turns green by itself when that run passes (#369)"

// projectbluefin/utah-packages#972 (2026-11-09): a PR opened from
// Karibhou's fork at 14:41:50Z failed only its Canary fork guard at
// 14:42:24Z. Neither the fork-guard index (no enumeration had read the
// annotation yet) nor the stage record (the PR had never been parked on a
// guard) knew it, so at 14:45:32Z the stage pass nudged Karibhou with
// required-check-failing:Canary and queued relay rework; the PR reached 085
// only at 14:50:57Z. The head's own annotations now classify it on the
// first pass: parked for a maintainer, nobody nudged. A head red on a real
// failure is still the author's, and each head is read once.
func TestForkGuardFirstHeadReadFromAnnotations(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	runs := map[string]string{
		// head → check-runs list
		"d24a12f": `{"total_count":2,"check_runs":[{"id":11,"name":"Canary","status":"completed","conclusion":"failure"},{"id":12,"name":"Unit tests","status":"completed","conclusion":"success"}]}`,
		"unitred": `{"total_count":2,"check_runs":[{"id":21,"name":"Unit tests","status":"completed","conclusion":"failure"},{"id":22,"name":"Canary","status":"completed","conclusion":"success"}]}`,
	}
	annotations := map[string]string{
		"11": fmt.Sprintf(`[{"annotation_level":"failure","message":"Process completed with exit code 1."},{"annotation_level":"failure","message":%q}]`, forkCanaryAnnotation),
		"21": `[{"annotation_level":"failure","message":"FAIL: TestWidget (0.01s)"}]`,
	}
	var mu sync.Mutex
	annotationReads := map[string]int{}
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/commits/") && strings.HasSuffix(path, "/check-runs"):
			head := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/commits/"), "/check-runs")
			body, ok := runs[head]
			if !ok {
				return false
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return true
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/acme/widget/check-runs/") && strings.HasSuffix(path, "/annotations"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/check-runs/"), "/annotations")
			mu.Lock()
			annotationReads[id]++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(annotations[id]))
			return true
		}
		return false
	}
	reads := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return annotationReads[id]
	}

	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 14, 45, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued", Online: true}}
	// The index has read nothing for these heads.
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, ForkGuard: func(string, int, string) []string { return nil }}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	e.checkSummary = func(_ context.Context, _, _, sha string) (hgithub.CommitChecksSummary, error) {
		if sha == "unitred" {
			return hgithub.CommitChecksSummary{Total: 2, Green: 1, Red: []string{"Unit tests"}, DCO: 1}, nil
		}
		return hgithub.CommitChecksSummary{Total: 2, Green: 1, Red: []string{"Canary"}, DCO: 1}, nil
	}
	ctx := context.Background()
	pass := func(number int, head, reason string) stagelabels.Record {
		t.Helper()
		pr := nudgePR(number, head)
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("karibhou/widget")}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", number))
		return rec
	}

	// The fork's first red head, red only on the Canary fork guard.
	rec := pass(972, "d24a12f", "required-check-failing:Canary")
	if rec.Stage != stagelabels.NeedsDecision || rec.Reason != stageReasonForkGuard+"Canary" {
		t.Fatalf("first fork-guard head: record %s %q, want 085 %q", rec.Stage, rec.Reason, stageReasonForkGuard+"Canary")
	}
	if len(sink.nudges) != 0 || len(nudgeComments(api)) != 0 {
		t.Fatalf("author nudged (and relay rework queued) for a check only a maintainer can run: sink=%+v comments=%v", sink.nudges, nudgeComments(api))
	}
	clock.advance(time.Minute)
	if rec := pass(972, "d24a12f", "required-check-failing:Canary"); rec.Stage != stagelabels.NeedsDecision || len(sink.nudges) != 0 {
		t.Fatalf("second pass: stage %s nudges %d, want still 085 and no nudge", rec.Stage, len(sink.nudges))
	}
	if got := reads("11"); got != 1 {
		t.Fatalf("Canary annotations read %d times, want once per head", got)
	}

	// A fork head red on a real failure stays with its author, and is read once.
	rec = pass(973, "unitred", "required-check-failing:Unit tests")
	if rec.Stage != stagelabels.ReturnedToContributor || len(sink.nudges) != 1 {
		t.Fatalf("real failure on a fork head: stage %s nudges %d, want 072 and one nudge", rec.Stage, len(sink.nudges))
	}
	clock.advance(time.Minute)
	pass(973, "unitred", "required-check-failing:Unit tests")
	if got := reads("21"); got != 1 {
		t.Fatalf("unit-test annotations read %d times, want once per head", got)
	}
}

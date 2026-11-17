package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/chairlift#811: main was red on `Unit Tests` all afternoon.
// Each new head the enumeration had not read yet (130c2547 at 4:29 PM,
// ead00404 at 4:46 PM) went to 072 with "Failing checks: Unit Tests" and a
// relay fix queued, then back to 050 minutes later. A head whose every red
// check is remembered red on its base now stays at 050 with no nudge; any
// other red check, or a CI-owning path, still goes to the author.
func TestBaseRedCarriesToHeadsTheIndexHasNotRead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	files := map[int][]string{
		811: {"internal/widget/widget.go"},
		812: {".github/workflows/test.yml"},
	}
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		path := r.URL.Path
		if r.Method != http.MethodGet || !strings.HasPrefix(path, "/repos/acme/widget/pulls/") || !strings.HasSuffix(path, "/files") {
			return false
		}
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/repos/acme/widget/pulls/"), "/files"))
		list, ok := files[n]
		if !ok {
			return false
		}
		out := []map[string]any{}
		for _, f := range list {
			out = append(out, map[string]any{"filename": f, "status": "modified"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return true
	}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 17, 21, 29, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued", Online: true}}
	policy := StageLabelPolicy{
		Enabled:          true,
		ContributorNudge: true,
		// The index has read none of these heads.
		BaseBranchRed: func(string, int, string) []string { return nil },
		BaseRedRemembered: func(repo, baseRef string) []string {
			if repo == "acme/widget" && baseRef == "main" {
				return []string{"Race Detection", "Tests Passed", "Unit Tests"}
			}
			return nil
		},
	}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	e.checkSummary = func(_ context.Context, _, _, sha string) (hgithub.CommitChecksSummary, error) {
		if sha == "extra" {
			return hgithub.CommitChecksSummary{Total: 4, Green: 2, Red: []string{"Unit Tests", "lint"}, DCO: 1}, nil
		}
		return hgithub.CommitChecksSummary{Total: 4, Green: 3, Red: []string{"Unit Tests"}, DCO: 1}, nil
	}
	ctx := context.Background()
	pass := func(number int, head, base, reason string) stagelabels.Record {
		t.Helper()
		pr := nudgePR(number, head)
		pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr(base)}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, reason, false, nil)
		e.noteStageRole("acme", "widget", number, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", number))
		return rec
	}

	for _, head := range []string{"130c2547", "ead00404"} {
		rec := pass(811, head, "main", "required-check-failing:Unit Tests")
		if rec.Stage != stagelabels.ReviewPending || rec.Reason != stageReasonBaseBranchRed+"Unit Tests" {
			t.Fatalf("head %s red only on the base's check: record %s %q, want 050 %q", head, rec.Stage, rec.Reason, stageReasonBaseBranchRed+"Unit Tests")
		}
		if len(sink.nudges) != 0 || len(nudgeComments(api)) != 0 {
			t.Fatalf("head %s: author nudged for the base's red: sink=%+v comments=%v", head, sink.nudges, nudgeComments(api))
		}
		clock.advance(5 * time.Minute)
	}

	// Another red check on the head: the author's, as before.
	if rec := pass(811, "extra", "main", "required-check-failing:lint"); rec.Stage != stagelabels.ReturnedToContributor || len(sink.nudges) != 1 {
		t.Fatalf("a red check the base does not share: stage %s nudges %d, want 072 and one nudge", rec.Stage, len(sink.nudges))
	}

	// A head that edits a workflow may own the very check that is red.
	if rec := pass(812, "wf", "main", "required-check-failing:Unit Tests"); strings.HasPrefix(rec.Reason, stageReasonBaseBranchRed) || rec.Stage == stagelabels.ReviewPending {
		t.Fatalf("CI-owning path: record %s %q, want author-facing", rec.Stage, rec.Reason)
	}

	// Nothing remembered on this base: today's route.
	if rec := pass(813, "rel", "release", "required-check-failing:Unit Tests"); strings.HasPrefix(rec.Reason, stageReasonBaseBranchRed) {
		t.Fatalf("no remembered base red: record %s %q, want author-facing", rec.Stage, rec.Reason)
	}
}

func TestHeadTouchesCIOwningPathCachedPerHead(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	reads := 0
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/widget/pulls/9/files" {
			return false
		}
		reads++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"filename":"README.md"}]`))
		return true
	}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 17, 21, 0, 0, 0, time.UTC)}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, &fakeNudgeSink{}, StageLabelPolicy{Enabled: true})
	tracker := NewStageTracker()
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "aaa"}
	for i := 0; i < 2; i++ {
		if touches, known := e.headTouchesCIOwningPath(context.Background(), tracker, stageKey("acme", "widget", 9), obs); touches || !known {
			t.Fatalf("README-only head: touches=%v known=%v", touches, known)
		}
	}
	if reads != 1 {
		t.Fatalf("file list read %d times for one head, want once", reads)
	}
	obs.HeadSHA = "bbb"
	e.headTouchesCIOwningPath(context.Background(), tracker, stageKey("acme", "widget", 9), obs)
	if reads != 2 {
		t.Fatalf("a new head must be read again: %d reads", reads)
	}
}

// projectbluefin/common#1684: the red check was optional, so the lane soaked
// and the stage read it from the detail (optional-check-failing:E2E…). A head
// the index had not read yet is folded to base red from the same detail.
func TestFoldBaseRedRedOptionalWhileSoaking(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repos/acme/widget/pulls/1684/files" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"filename":"system_files/shared/usr/bin/ujust"}]`))
		return true
	}
	clock := &contributorLaneClock{at: time.Date(2026, 11, 17, 18, 34, 0, 0, time.UTC)}
	const e2e = "E2E — composed common suite / e2e / GNOME — common"
	policy := StageLabelPolicy{Enabled: true, BaseRedRemembered: func(repo, baseRef string) []string {
		if repo == "acme/widget" && baseRef == "main" {
			return []string{e2e}
		}
		return nil
	}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, &fakeNudgeSink{}, policy)
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 9, Green: 8, Red: []string{e2e}, DCO: 1}, nil
	}
	detail := stageDetail{Verdict: ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, VerdictKnown: true}
	detail.RedChecks, detail.RedChecksHead = []string{e2e}, "4d94a546a0"
	list := []stageObservation{{Lane: stageLaneContributor, Owner: "acme", Repo: "widget", Number: 1684, HeadSHA: "4d94a546a0", BaseRef: "main", Reason: contributorLaneReasonSoaking + "(1h)", Detail: detail}}
	e.foldBaseRed(context.Background(), NewStageTracker(), list, clock.now())
	if got := list[0].BaseRedChecks; len(got) != 1 || got[0] != e2e {
		t.Fatalf("BaseRedChecks = %v, want [%s]", got, e2e)
	}
	if v := classifyStage(list[0]); v.Stage != stagelabels.Reviewed || v.Reason != stageReasonBaseBranchRed+e2e {
		t.Fatalf("classifyStage = %+v, want 060 %q", v, stageReasonBaseBranchRed+e2e)
	}

	// The index already answered for the head: nothing to fold.
	answered := []stageObservation{{Lane: stageLaneContributor, Owner: "acme", Repo: "widget", Number: 1684, HeadSHA: "4d94a546a0", BaseRef: "main", Reason: "required-check-failing:build", BaseRedChecks: []string{"build"}}}
	e.foldBaseRed(context.Background(), NewStageTracker(), answered, clock.now())
	if got := answered[0].BaseRedChecks; len(got) != 1 || got[0] != "build" {
		t.Fatalf("index answer replaced: %v", got)
	}
}

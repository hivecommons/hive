package automerge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah#1074: a PR opened with GITHUB_TOKEN triggers no
// workflow, so the ruleset-required `contract` and `build_main / …` never
// reported, while the four checks that did run were green. The hive said
// "4/4 checks green … Ready for a human merge"; GitHub would refuse the
// merge. A required check with nothing reported is now a pending gate.
func TestHumanHandoffWaitsOnMissingRequiredChecks(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)}
	verdict := &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	e, store := humanStageEngine(t, api.url, clock, verdict, time.Hour)
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 4, Green: 4, DCO: 1, MissingRequired: []string{"build_main / Check all testing builds successful", "contract"}}, nil
	}
	ctx := context.Background()
	sweep := func(labels ...string) {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", stagePR(9, labels...), "untrusted-author-role", false, nil)
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), contributorLaneReasonClaimTier, false, nil)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sweep()
	clock.at = clock.at.Add(time.Hour) // soak elapsed
	sweep("role/contributor", "author/human", "stage/060-reviewed")
	rec, _ := store.Get(stageKey("acme", "widget", 9))
	if rec.Stage != stagelabels.Reviewed {
		t.Fatalf("stage = %v, want 060 while required checks have not reported", rec.Stage)
	}
	for _, c := range api.comments {
		if strings.HasPrefix(c, stagelabels.ReadyCommentMarker) {
			t.Fatalf("posted a ready-for-human-merge comment: %q", c)
		}
	}

	// Once they report green, the PR is ready.
	e.checkSummary = func(context.Context, string, string, string) (hgithub.CommitChecksSummary, error) {
		return hgithub.CommitChecksSummary{Total: 6, Green: 6, DCO: 1}, nil
	}
	clock.at = clock.at.Add(humanChecksTTL)
	sweep("role/contributor", "author/human", "stage/060-reviewed")
	if got := stageOf(t, store, 9); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage = %v, want 090 once the required checks reported", got)
	}
}

// The default reader asks the base branch's required set once per repo and
// branch within humanChecksTTL, and names a required check the head lacks.
func TestHeadRequiredChecksReadsBaseBranchOnce(t *testing.T) {
	var protectionReads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widget/branches/main/protection/required_status_checks":
			protectionReads++
			_, _ = w.Write([]byte(`{"contexts":["contract","lint"]}`))
		case "/repos/acme/widget/commits/sha1/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		case "/repos/acme/widget/commits/sha1/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"lint","status":"completed","conclusion":"success","app":{"id":1}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, srv.URL+"/")
	now := time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC)
	e := New(client, Options{Now: func() time.Time { return now }})
	tracker := NewStageTracker()
	obs := stageObservation{Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 1074, HeadSHA: "sha1", BaseRef: "main"}
	for i := 0; i < 2; i++ {
		summary, known := e.headChecks(context.Background(), tracker, stageKey("acme", "widget", 1074+i), obs, now)
		if !known || !reflect.DeepEqual(summary.MissingRequired, []string{"contract"}) {
			t.Fatalf("pass %d: summary = %+v known=%v, want contract missing", i, summary, known)
		}
	}
	if protectionReads != 1 {
		t.Fatalf("required set read %d times, want 1 (cached per repo and branch)", protectionReads)
	}
}

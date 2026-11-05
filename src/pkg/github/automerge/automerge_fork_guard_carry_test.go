package automerge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestLaneRedCheckAndCarriedForkGuard(t *testing.T) {
	fork := stageObservation{Owner: "acme", Repo: "widget", HeadRepo: "mallory/widget", Reason: "required-check-failing:Canary"}
	guard := stagelabels.Record{Stage: stagelabels.NeedsDecision, Reason: stageReasonForkGuard + "Canary,Publish"}
	for _, tc := range []struct {
		name string
		rec  stagelabels.Record
		ok   bool
		obs  func(stageObservation) stageObservation
		want int
	}{
		{name: "fork, recorded guard, same check red", rec: guard, ok: true, obs: func(o stageObservation) stageObservation { return o }, want: 2},
		{name: "optional check named", rec: guard, ok: true, obs: func(o stageObservation) stageObservation {
			o.Reason = stageReasonOptionalCheckFailing + "Publish"
			return o
		}, want: 2},
		{name: "another check red", rec: guard, ok: true, obs: func(o stageObservation) stageObservation { o.Reason = "required-check-failing:unit"; return o }},
		{name: "not red", rec: guard, ok: true, obs: func(o stageObservation) stageObservation { o.Reason = "check-pending"; return o }},
		{name: "same-repo head", rec: guard, ok: true, obs: func(o stageObservation) stageObservation { o.HeadRepo = "acme/widget"; return o }},
		{name: "head repo unknown", rec: guard, ok: true, obs: func(o stageObservation) stageObservation { o.HeadRepo = ""; return o }},
		{name: "record parked on something else", rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, Reason: "required-check-failing:Canary"}, ok: true, obs: func(o stageObservation) stageObservation { return o }},
		{name: "record names the check without the guard", rec: stagelabels.Record{Stage: stagelabels.ReturnedToContributor, Reason: "Canary"}, ok: true, obs: func(o stageObservation) stageObservation { return o }},
		{name: "no record", obs: func(o stageObservation) stageObservation { return o }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriedForkGuard(tc.rec, tc.ok, tc.obs(fork)); len(got) != tc.want {
				t.Fatalf("carriedForkGuard = %v, want %d checks", got, tc.want)
			}
		})
	}
}

// projectbluefin/utah-packages#869: a new head on a fork PR parked on the
// fork-guarded Canary (a merge of the base at 18:04Z, the author's push at
// 18:14Z) read as an ordinary red check before the index caught up, and the
// author was nudged at 18:05:40Z and 18:15:05Z. The guard carries onto the
// new head: the PR stays with a maintainer and nobody is nudged.
func TestForkGuardCarriesAcrossHeadsWithoutNudge(t *testing.T) {
	api, closeAPI := seededStageAPI(t)
	defer closeAPI()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)}
	sink := &fakeNudgeSink{outcome: ContributorNudgeOutcome{Mode: "queued", Online: true}}
	indexed := map[string]bool{"aaa": true}
	policy := StageLabelPolicy{Enabled: true, ContributorNudge: true, ForkGuard: func(repo string, number int, head string) []string {
		if indexed[head] {
			return []string{"Canary"}
		}
		return nil
	}}
	e := newNudgeEngine(t, apiURLOf(t, api), clock, sink, policy)
	ctx := context.Background()
	stageOf := func(head string) stagelabels.Stage {
		t.Helper()
		pr := nudgePR(869, head)
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/widget")}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:Canary", false, nil)
		e.noteStageRole("acme", "widget", 869, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", 869))
		return rec.Stage
	}
	if got := stageOf("aaa"); got != stagelabels.NeedsDecision {
		t.Fatalf("indexed head: stage %s, want 085", got)
	}
	clock.advance(5 * time.Minute)
	if got := stageOf("bbb"); got != stagelabels.NeedsDecision {
		t.Fatalf("new head before the index reads it: stage %s, want 085", got)
	}
	if len(sink.nudges) != 0 || len(nudgeComments(api)) != 0 {
		t.Fatalf("author nudged on a fork-guarded head: sink=%+v comments=%v", sink.nudges, api.comments)
	}
	rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", 869))
	if rec.Reason != stageReasonForkGuard+"Canary" {
		t.Fatalf("record reason = %q", rec.Reason)
	}

	// Any other red check on the new head still goes to the author.
	clock.advance(5 * time.Minute)
	pr := nudgePR(869, "ccc")
	pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("mallory/widget")}
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:unit", false, nil)
	e.noteStageRole("acme", "widget", 869, stagelabels.RoleContributor)
	if _, err := e.ApplyStageLabels(ctx); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.stageStoreOrShared().Get(stageKey("acme", "widget", 869)); rec.Stage != stagelabels.ReturnedToContributor || len(sink.nudges) != 1 {
		t.Fatalf("a real failure must return to the author: stage %s nudges %d", rec.Stage, len(sink.nudges))
	}
}

// projectbluefin/utah-packages#862: 13 merge-of-main heads while the PR
// waited on a maintainer's exact-head Canary run. A fork head the index shows
// waiting on a fork-guarded check is not updated; a fork head without one,
// and a same-repo head, are.
func TestUpdateBranchSkipsForkGuardedHead(t *testing.T) {
	var updates []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
			updates = append(updates, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			_, _ = w.Write([]byte(`{"number":862,"head":{"sha":"new"},"mergeable_state":"clean"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	policy := StageLabelPolicy{Enabled: true, ForkGuard: func(repo string, number int, head string) []string {
		if head == "guarded" {
			return []string{"Canary"}
		}
		return nil
	}}
	e := newNudgeEngine(t, srv.URL+"/", clock, nil, policy)
	ctx := context.Background()

	_, reason, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "guarded", "base1", "behind", true)
	if err != nil || reason != "required-check-failing:Canary" || len(updates) != 0 {
		t.Fatalf("fork-guarded head: reason=%q err=%v updates=%v, want no update", reason, err, updates)
	}
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "plain", "base1", "behind", true); err != nil || len(updates) != 1 {
		t.Fatalf("unguarded fork head: err=%v updates=%v, want one update", err, updates)
	}
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "guarded", "base1", "behind", false); err != nil || len(updates) != 2 {
		t.Fatalf("same-repo head: err=%v updates=%v, want an update", err, updates)
	}
}

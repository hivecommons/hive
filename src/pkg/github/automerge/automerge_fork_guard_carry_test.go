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

	_, reason, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "guarded", "base1", "behind", currentBaseTestPR(true, "feature", "alice", "behind"))
	if err != nil || reason != "required-check-failing:Canary" || len(updates) != 0 {
		t.Fatalf("fork-guarded head: reason=%q err=%v updates=%v, want no update", reason, err, updates)
	}
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "plain", "base1", "behind", currentBaseTestPR(true, "feature", "alice", "behind")); err != nil || len(updates) != 1 {
		t.Fatalf("unguarded fork head: err=%v updates=%v, want one update", err, updates)
	}
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 862, "guarded", "base1", "behind", currentBaseTestPR(false, "feature", "alice", "behind")); err != nil || len(updates) != 2 {
		t.Fatalf("same-repo head: err=%v updates=%v, want an update", err, updates)
	}
}

// projectbluefin/utah-packages#917: five merges of main in 22 hours while
// the PR sat at 085 on `fork-guard:Canary`, each in the first sweep after a
// restart or a rate-limited pass left the index without an entry for the
// head. With the index empty, the stage record's guard holds the update
// while the guarded check is red or still running on the head, lets it
// through once green, and holds it while the head's checks cannot be read.
func TestUpdateBranchHonoursRecordedForkGuard(t *testing.T) {
	var updates []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
			updates = append(updates, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			switch strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/commits/"), "/check-runs") {
			case "red":
				_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[{"id":1,"name":"Canary","status":"completed","conclusion":"failure"},{"id":2,"name":"unit","status":"completed","conclusion":"success"}]}`))
			case "running":
				// The maintainer's re-run is newer than the failure; an update would discard it.
				_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[{"id":1,"name":"Canary","status":"completed","conclusion":"failure"},{"id":4,"name":"Canary","status":"in_progress","conclusion":null}]}`))
			case "queued":
				_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"id":5,"name":"Canary","status":"queued","conclusion":null}]}`))
			case "green":
				_, _ = w.Write([]byte(`{"total_count":2,"check_runs":[{"id":1,"name":"Canary","status":"completed","conclusion":"failure"},{"id":3,"name":"Canary","status":"completed","conclusion":"success"}]}`))
			case "forbidden":
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			default:
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/pulls/"):
			_, _ = w.Write([]byte(`{"number":917,"head":{"sha":"new"},"mergeable_state":"clean"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)}
	policy := StageLabelPolicy{Enabled: true, ForkGuard: func(string, int, string) []string { return nil }}
	e := newNudgeEngine(t, srv.URL+"/", clock, nil, policy)
	e.stageStoreOrShared().Observe(stageKey("acme", "widget", 917), stagelabels.NeedsDecision, stageReasonForkGuard+"Canary")
	ctx := context.Background()
	fork := currentBaseTestPR(true, "task/475-libfdk-aac", "kylerankin", "behind")

	for _, head := range []string{"red", "running", "queued", "forbidden", "broken"} {
		_, reason, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 917, head, "base1", "behind", fork)
		if err != nil || reason != "required-check-failing:Canary" || len(updates) != 0 {
			t.Fatalf("head %s: reason=%q err=%v updates=%v, want the update held on Canary", head, reason, err, updates)
		}
	}
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 917, "green", "base1", "behind", fork); err != nil || len(updates) != 1 {
		t.Fatalf("Canary green on the head: err=%v updates=%v, want one update", err, updates)
	}

	// The record's ForkGuardChecks name the guard without a fork-guard reason.
	e.stageStoreOrShared().Observe(stageKey("acme", "widget", 921), stagelabels.ReturnedToContributor, "required-check-failing:Canary")
	e.stageStoreOrShared().NoteForkGuard(stageKey("acme", "widget", 921), []string{"Canary"})
	if _, reason, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 921, "red", "base1", "behind", fork); err != nil || reason != "required-check-failing:Canary" || len(updates) != 1 {
		t.Fatalf("record ForkGuardChecks: reason=%q err=%v updates=%v, want the update held", reason, err, updates)
	}

	// A same-repo head is never held by the record.
	if _, _, err := e.updateBranchAndSkip(ctx, "acme/widget", "acme", "widget", 917, "red", "base1", "behind", currentBaseTestPR(false, "feature", "alice", "behind")); err != nil || len(updates) != 2 {
		t.Fatalf("same-repo head: err=%v updates=%v, want an update", err, updates)
	}
}

// projectbluefin/utah-packages#917: the maintainer's fork Canary passed on
// one head, the author rebased the same patch onto a newer base, and the
// carried guard kept the PR at 085 without a stage change. The 085 comment
// still asked for a run on the earlier head. It is now rewritten in place
// for the new head (no second comment), and left alone on a repeat pass.
func TestForkGuardEntryCommentFollowsNewHead(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}, statefulComments: true}
	for _, d := range stagelabels.AllLabelDefs() {
		api.labels[d.Name] = d.Color
	}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)}
	indexed := map[string]bool{"8d7a655aaaa": true}
	policy := StageLabelPolicy{Enabled: true, ForkGuard: func(repo string, number int, head string) []string {
		if indexed[head] {
			return []string{"Canary"}
		}
		return nil
	}}
	e := newNudgeEngine(t, srv.URL, clock, nil, policy)
	ctx := context.Background()
	key := stageKey("acme", "widget", 917)
	pass := func(head string) {
		t.Helper()
		api.reset()
		pr := nudgePR(917, head)
		pr.Head.Repo = &gh.Repository{FullName: gh.Ptr("kylerankin/widget")}
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, "required-check-failing:Canary", false, nil)
		e.noteStageRole("acme", "widget", 917, stagelabels.RoleContributor)
		if _, err := e.ApplyStageLabels(ctx); err != nil {
			t.Fatal(err)
		}
		if rec, _ := e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || rec.Reason != stageReasonForkGuard+"Canary" {
			t.Fatalf("head %s: record %s %q, want 085 fork-guard", head, rec.Stage, rec.Reason)
		}
	}

	entries := func(bodies []string) []string {
		var out []string
		for _, b := range bodies {
			if strings.HasPrefix(b, stagelabels.DecisionCommentMarker) {
				out = append(out, b)
			}
		}
		return out
	}

	pass("8d7a655aaaa")
	if got := entries(api.comments); len(got) != 1 || !strings.Contains(got[0], "for head `8d7a655`") {
		t.Fatalf("first 085 comment = %q", api.comments)
	}

	// The rebase: same patch, newer base, new head the index has not read.
	clock.advance(time.Hour)
	pass("82ef790bbbb")
	if got := entries(api.comments); len(got) != 0 {
		t.Fatalf("new head posted another 085 comment instead of editing: %q", got)
	}
	edits := entries(api.commentEdits)
	if len(edits) != 1 || !strings.Contains(edits[0], "for head `82ef790`") ||
		!strings.Contains(edits[0], "does not cover `82ef790`") || strings.Contains(edits[0], "8d7a655") {
		t.Fatalf("085 comment not rewritten for the new head: %q", api.commentEdits)
	}
	if rec, _ := e.stageStoreOrShared().Get(key); rec.EntryCommentedHead != "82ef790bbbb" {
		t.Fatalf("entry head = %q, want the new head", rec.EntryCommentedHead)
	}

	// Same head again: nothing to rewrite (commentEdits is cumulative).
	clock.advance(5 * time.Minute)
	pass("82ef790bbbb")
	if got := entries(api.comments); len(got) != 0 || len(entries(api.commentEdits)) != 1 {
		t.Fatalf("repeat pass rewrote the comment: comments=%q edits=%q", api.comments, api.commentEdits)
	}
}

func TestForkGuardEntryStale(t *testing.T) {
	parked := stagelabels.Record{Stage: stagelabels.NeedsDecision, EntryCommented: true, EntryCommentedHead: "old", EntryCommentedStage: stagelabels.NeedsDecision}
	route := stagelabels.Route{Stage: stagelabels.NeedsDecision}
	obs := stageObservation{HeadSHA: "new", Reason: stageReasonForkGuard + "Canary"}
	if !forkGuardEntryStale(parked, obs, route) {
		t.Fatal("fork-guard park on a new head must be stale")
	}
	same := obs
	same.HeadSHA = "old"
	other := obs
	other.Reason = "sentinel"
	ready := route
	ready.Stage = stagelabels.ReadyForHumanMerge
	never := parked
	never.EntryCommentedHead = ""
	for name, tc := range map[string]struct {
		rec   stagelabels.Record
		obs   stageObservation
		route stagelabels.Route
	}{
		"same head":           {parked, same, route},
		"another 085 cause":   {parked, other, route},
		"not an 085 route":    {parked, obs, ready},
		"never commented yet": {never, obs, route},
	} {
		if forkGuardEntryStale(tc.rec, tc.obs, tc.route) {
			t.Errorf("%s: stale, want not", name)
		}
	}
}

package automerge

// One sign-off attribution note per PR (hivecommons/hive#17066).

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestMarkerHistoryRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 4, 12, 4, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		body     string
		wantCore string
		wantHist []string
	}{
		{name: "no history", body: "<!-- m -->\nbody\n", wantCore: "<!-- m -->\nbody"},
		{
			name:     "two lines",
			body:     renderMarkerHistory("<!-- m -->\nbody", []string{"updated a", "updated b"}),
			wantCore: "<!-- m -->\nbody",
			wantHist: []string{"updated a", "updated b"},
		},
		{
			name:     "stray text after the heading is ignored",
			body:     "<!-- m -->\nbody\n\n" + markerHistoryHeading + "\nnot a bullet\n- kept\n",
			wantCore: "<!-- m -->\nbody",
			wantHist: []string{"kept"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, hist := splitMarkerHistory(tc.body)
			if core != tc.wantCore || strings.Join(hist, "|") != strings.Join(tc.wantHist, "|") {
				t.Fatalf("split = %q / %q, want %q / %q", core, hist, tc.wantCore, tc.wantHist)
			}
		})
	}
	var hist []string
	for i := 1; i <= markerHistoryCap+2; i++ {
		hist = appendMarkerHistory(hist, markerHistoryLine(at.Add(time.Duration(i)*time.Minute), fmt.Sprintf("change %d", i)))
	}
	if len(hist) != markerHistoryCap || hist[0] != "updated 2026-11-04 12:07 UTC: change 3" || hist[markerHistoryCap-1] != "updated 2026-11-04 12:11 UTC: change 7" {
		t.Fatalf("capped history = %q", hist)
	}
}

func TestSignoffAttributionRewrite(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 4, 59, 0, time.UTC)
	stage := stagelabels.NeedsAuthor
	nudge := func(head string, hiveSigned int) ContributorNudge {
		_, asks, _ := nudgeDiagnosis(stagelabels.LaneReasonHiveSignoffAttribution, stageDetail{DCOTotal: hiveSigned, DCOSquashUnsafeHive: hiveSigned})
		return ContributorNudge{Author: "clubanderson", HeadSHA: head, ReviewAsks: asks}
	}
	first := SignoffAttributionComment(nudge("eb40422ae63967eb7363a3f866e73121c58f2c1b", 8), stage, nil)
	legacy := strings.Replace(first, SignoffAttributionMarker, SignoffAttributionMarkerPrefix+" head=eb40422ae63967eb7363a3f866e73121c58f2c1b -->", 1)
	fiveDeep := first
	for i := 1; i <= markerHistoryCap; i++ {
		fiveDeep, _ = signoffAttributionRewrite(fiveDeep, nudge(strings.Repeat(fmt.Sprint(i), 40), 8), stage, 8, now.Add(time.Duration(i)*time.Minute))
	}
	for _, tc := range []struct {
		name        string
		previous    string
		head        string
		hiveSigned  int
		wantChanged bool
		wantLines   int
		wantLast    string
	}{
		{name: "same head and asks leave the note alone", previous: first, head: "eb40422ae63967eb7363a3f866e73121c58f2c1b", hiveSigned: 8},
		{
			name: "new head rewrites and records one line", previous: first, head: "2940830726b72f08dff5a1ef6a37bff871b4a7d5", hiveSigned: 20,
			wantChanged: true, wantLines: 1, wantLast: "updated 2026-11-04 12:04 UTC: head `2940830726b7`, 20 commits signed off as the hive App",
		},
		{
			name: "legacy per-head note is adopted under the stable marker", previous: legacy, head: "2940830726b72f08dff5a1ef6a37bff871b4a7d5", hiveSigned: 1,
			wantChanged: true, wantLines: 1, wantLast: "updated 2026-11-04 12:04 UTC: head `2940830726b7`, 1 commit signed off as the hive App",
		},
		{
			name: "history stays capped at five", previous: fiveDeep, head: "dc8ccef2b084b5860f317dacc5037071d78bc1b6", hiveSigned: 30,
			wantChanged: true, wantLines: markerHistoryCap, wantLast: "updated 2026-11-04 12:04 UTC: head `dc8ccef2b084`, 30 commits signed off as the hive App",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, changed := signoffAttributionRewrite(tc.previous, nudge(tc.head, tc.hiveSigned), stage, tc.hiveSigned, now)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v\n%s", changed, tc.wantChanged, body)
			}
			if !changed {
				if body != tc.previous {
					t.Fatalf("unchanged rewrite altered the body:\n%s", body)
				}
				return
			}
			if !strings.HasPrefix(body, SignoffAttributionMarker+"\n") || strings.Contains(body, " head=") {
				t.Fatalf("body not under the stable marker:\n%s", body)
			}
			if !strings.Contains(body, "Current head: `"+shortSHA(tc.head)+"`.") {
				t.Fatalf("body does not name the current head:\n%s", body)
			}
			_, hist := splitMarkerHistory(body)
			if len(hist) != tc.wantLines || hist[len(hist)-1] != tc.wantLast {
				t.Fatalf("history = %q, want %d lines ending %q", hist, tc.wantLines, tc.wantLast)
			}
			if again, changedAgain := signoffAttributionRewrite(body, nudge(tc.head, tc.hiveSigned), stage, tc.hiveSigned, now.Add(time.Hour)); changedAgain || again != body {
				t.Fatalf("second pass on the same head rewrote again:\n%s", again)
			}
		})
	}
}

// TestApplyStageLabelsSignoffAttributionUpsertsAcrossHeads pins the
// hivecommons/hive#16524 shape: eight pushes used to mean eight notes. Now
// the first head posts, each later head edits the same comment, and a
// repeat sweep on an already-noted head writes nothing.
func TestApplyStageLabelsSignoffAttributionUpsertsAcrossHeads(t *testing.T) {
	hiveCommit := stageCommitFixture{
		message:     "Add OWNERS file\n\nSigned-off-by: Andy Anderson <280983584+hivecommons-hive[bot]@users.noreply.github.com>",
		authorLogin: testHiveAppBotLogin,
		authorName:  "Andy Anderson",
		authorEmail: "280983584+hivecommons-hive[bot]@users.noreply.github.com",
	}
	api := &stageAPI{labels: map[string]string{}, statefulComments: true, commitDetails: map[int][]stageCommitFixture{16524: {hiveCommit}}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 4, 3, 1, 0, 0, time.UTC)}
	e, _, store := newStageEngine(t, srv.URL, clock)
	e.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true, ContributorNudge: true} }

	sweep := func(head string, labels ...string) {
		t.Helper()
		pr := stagePR(16524, labels...)
		pr.Head.SHA = gh.Ptr(head)
		pr.User = &gh.User{Login: gh.Ptr("clubanderson")}
		e.recordStage(stageLaneTrusted, "acme/widget", "acme", "widget", pr, autoMergeReasonReviewGatePrefix+ReviewGateOutcomeAwaiting, false, nil)
		e.noteStageRole("acme", "widget", 16524, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		rec, ok := store.Get(stageKey("acme", "widget", 16524))
		if !ok || rec.Reason != stagelabels.LaneReasonHiveSignoffAttribution || rec.NudgeHead != head {
			t.Fatalf("head %s: record %+v ok=%v", head, rec, ok)
		}
	}
	held := []string{"role/trusted", "author/human", stagelabels.DCOLabel, "stage/071-needs-author"}

	sweep("eb40422", "role/trusted")
	if len(api.comments) != 1 || len(api.commentEdits) != 0 {
		t.Fatalf("first head: comments %q edits %q", api.comments, api.commentEdits)
	}
	for i, head := range []string{"d61d907", "2940830", "7879a51"} {
		clock.at = clock.at.Add(10 * time.Minute)
		sweep(head, held...)
		if len(api.comments) != 1 || len(api.commentEdits) != i+1 {
			t.Fatalf("head %s: comments %q edits %q", head, api.comments, api.commentEdits)
		}
		last := api.commentEdits[i]
		if !strings.HasPrefix(last, SignoffAttributionMarker+"\n") || !strings.Contains(last, "Current head: `"+head+"`.") {
			t.Fatalf("head %s: edit not the upserted note:\n%s", head, last)
		}
		if _, hist := splitMarkerHistory(last); len(hist) != i+1 || !strings.Contains(hist[i], "head `"+head+"`, 1 commit signed off as the hive App") {
			t.Fatalf("head %s: history %q", head, hist)
		}
	}
	// Same head, next sweep: nothing is written.
	clock.at = clock.at.Add(10 * time.Minute)
	sweep("7879a51", held...)
	if len(api.comments) != 1 || len(api.commentEdits) != 3 {
		t.Fatalf("repeat sweep wrote: comments %q edits %q", api.comments, api.commentEdits)
	}
}

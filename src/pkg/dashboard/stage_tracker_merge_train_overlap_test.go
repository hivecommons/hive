package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hive's own page, 2026-10-29 20:00 EDT: "⛁ merge train · 2 PRs · 1st:
// #15637 — overlapping: sidebar.html, index.html" under two PRs whose
// chevrons both read CI / Review — #15637 (Twitch community links) and
// #15633 (the hive's adopted copy of #15566). The planner had ordered
// them; neither was at 080. The merge train is a merge-queue concept:
// a PR short of 080 rides no train, counts in no header, and instead
// carries an `overlap` hint naming the PR it will queue behind.
func TestStageTrackerMergeTrainCIReviewPairRidesNoTrain(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"sidebar.html", "index.html"}
	store.Observe(stagelabels.Key("acme/console", 15637), stagelabels.ReviewPending, "required-check-pending:build-and-test")
	store.Observe(stagelabels.Key("acme/console", 15633), stagelabels.ReviewPending,
		prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15637, Position: 2, Size: 2, OverlapFiles: overlap}.Reason())
	s.statusMu.Lock()
	s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
		FrontendPR{PullRequest: github.PullRequest{Number: 15637, Title: "Twitch community links", Author: "clubanderson", BaseRef: "v5", Labels: []string{stagelabels.ReviewPending.Label()}}},
		FrontendPR{PullRequest: github.PullRequest{Number: 15633, Title: "Adopted copy", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: []string{stagelabels.ReviewPending.Label(), "author/hive"}}})
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	front := trackerItem(t, resp, "pr", 15637)
	rider := trackerItem(t, resp, "pr", 15633)
	for _, it := range []StageTrackerItem{front, rider} {
		if it.Stage != stagelabels.ReviewPending.Number() {
			t.Fatalf("#%d stage = %d, want 050", it.Number, it.Stage)
		}
		if it.MergeTrain != nil {
			t.Errorf("#%d at CI / Review rides no merge train: %+v", it.Number, it.MergeTrain)
		}
		if it.LeftTrain != nil {
			t.Errorf("#%d is not under repair; no left_train note: %+v", it.Number, it.LeftTrain)
		}
		if it.MergeGate != nil && (it.MergeGate.State == MergeGateStateQueuedBehind || it.MergeGate.State == MergeGateStateTrainFront) {
			t.Errorf("#%d gate keeps the train's words: %+v", it.Number, it.MergeGate)
		}
	}
	if rider.Overlap == nil || rider.Overlap.Behind != 15637 || strings.Join(rider.Overlap.Files, ",") != "sidebar.html,index.html" {
		t.Fatalf("rider overlap = %+v", rider.Overlap)
	}
	if want := "overlaps #15637 (sidebar.html, index.html) — will queue behind it"; rider.Overlap.Note != want {
		t.Errorf("rider note = %q, want %q", rider.Overlap.Note, want)
	}
	if front.Overlap == nil || front.Overlap.Behind != 0 || len(front.Overlap.With) != 1 || front.Overlap.With[0] != 15633 {
		t.Fatalf("front overlap = %+v", front.Overlap)
	}
	if want := "overlaps #15633 (sidebar.html, index.html) — merges ahead of it once both are ready"; front.Overlap.Note != want {
		t.Errorf("front note = %q, want %q", front.Overlap.Note, want)
	}
	raw, err := json.Marshal(rider)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"overlap":{"behind":15637,"with":[15637],"files":["sidebar.html","index.html"],"note":"overlaps #15637`) {
		t.Errorf("payload lacks the overlap hint:\n%s", raw)
	}
	if strings.Contains(string(raw), `"merge_train"`) || strings.Contains(string(raw), `provisional`) {
		t.Errorf("payload still carries a train:\n%s", raw)
	}
}

// A train of three where one member is still at 050: the two 080 rows
// are the train (size 2, the 050 row counted nowhere), and the 050 row
// carries the hint naming the front.
func TestStageTrackerMergeTrainCountsOnlyMergeQueueMembers(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"tracker-words.js"}
	queued := func(pos int) string {
		return prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15600, Position: pos, Size: 3, OverlapFiles: overlap}.Reason()
	}
	store.Observe(stagelabels.Key("acme/console", 15600), stagelabels.AwaitingMergeAdmission, "")
	store.Observe(stagelabels.Key("acme/console", 15601), stagelabels.ReviewPending, queued(2))
	store.Observe(stagelabels.Key("acme/console", 15602), stagelabels.AwaitingMergeAdmission, queued(3))
	s.statusMu.Lock()
	for n, stage := range map[int]stagelabels.Stage{15600: stagelabels.AwaitingMergeAdmission, 15601: stagelabels.ReviewPending, 15602: stagelabels.AwaitingMergeAdmission} {
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Tracker words", Author: "someone", BaseRef: "v5", Labels: []string{stage.Label()}}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	front := trackerItem(t, resp, "pr", 15600)
	pending := trackerItem(t, resp, "pr", 15601)
	tail := trackerItem(t, resp, "pr", 15602)
	if front.MergeTrain == nil || tail.MergeTrain == nil {
		t.Fatalf("the two 080 rows ride: front=%+v tail=%+v", front.MergeTrain, tail.MergeTrain)
	}
	if front.MergeTrain.Size != 2 || tail.MergeTrain.Size != 2 || tail.MergeTrain.Position != 2 || len(tail.MergeTrain.Members) != 2 {
		t.Errorf("the 050 row counts in no header: front=%+v tail=%+v", front.MergeTrain, tail.MergeTrain)
	}
	if front.MergeGate == nil || !strings.Contains(front.MergeGate.Reason, "1st of 2") || strings.Contains(front.MergeGate.Reason, "once it is ready") {
		t.Errorf("front gate = %+v", front.MergeGate)
	}
	if pending.MergeTrain != nil || pending.Overlap == nil || pending.Overlap.Behind != 15600 {
		t.Errorf("the 050 row rides no train and hints at the front: train=%+v overlap=%+v", pending.MergeTrain, pending.Overlap)
	}
}

// Two overlapping PRs, the planner's front still at 050 and the sibling
// at 080: no train (one merge-queue member is no train), the 080 row
// back to its own caption, the 050 row with the hint.
func TestStageTrackerMergeTrainPendingFrontLeavesLoneRider(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	store.Observe(stagelabels.Key("acme/console", 15577), stagelabels.ReviewPending, "required-check-pending:build-and-test")
	store.Observe(stagelabels.Key("acme/console", 15578), stagelabels.AwaitingMergeAdmission,
		prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15577, Position: 2, Size: 2, OverlapFiles: []string{"tracker-words.js"}}.Reason())
	s.statusMu.Lock()
	for n, stage := range map[int]stagelabels.Stage{15577: stagelabels.ReviewPending, 15578: stagelabels.AwaitingMergeAdmission} {
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Tracker words", Author: "someone", BaseRef: "v5", Labels: []string{stage.Label()}}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)
	front := trackerItem(t, resp, "pr", 15577)
	rider := trackerItem(t, resp, "pr", 15578)
	if front.MergeTrain != nil || rider.MergeTrain != nil {
		t.Fatalf("no train of one: front=%+v rider=%+v", front.MergeTrain, rider.MergeTrain)
	}
	if front.Overlap == nil || front.Overlap.Behind != 0 || len(front.Overlap.With) != 1 || front.Overlap.With[0] != 15578 {
		t.Errorf("front overlap = %+v", front.Overlap)
	}
	if rider.Overlap != nil || rider.Caption != stageTrackerAwaitingAdmissionCaption {
		t.Errorf("080 rider: overlap=%+v caption=%q", rider.Overlap, rider.Caption)
	}
}

// Owner, hive's own page: "merge train · 2 of 9 PRs here · 1st: #15516 ·
// not in this view: 7 more" — "what does 'not in this view' mean? if there
// are 7 not ready to merge, then why are they mentioned?". The planner
// grouped nine overlapping PRs but only two carried a train record in the
// payload; the other seven were below the merge queue or no longer listed.
// Size and Members count the PRs riding in this payload, nothing wider.
func TestStageTrackerMergeTrainSizeCountsRidersInPayload(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"dashboard-misc.md", "dashboard-sections.md"}
	store.Observe(stagelabels.Key("acme/console", 15516), stagelabels.AwaitingMergeAdmission, "")
	store.Observe(stagelabels.Key("acme/console", 15563), stagelabels.AwaitingMergeAdmission,
		prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15516, Position: 5, Size: 9, OverlapFiles: overlap}.Reason())
	s.statusMu.Lock()
	for _, n := range []int{15516, 15563} {
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Docs", Author: "someone", BaseRef: "v5", Labels: []string{stagelabels.AwaitingMergeAdmission.Label()}}})
	}
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	front := trackerItem(t, resp, "pr", 15516)
	second := trackerItem(t, resp, "pr", 15563)
	if front.MergeTrain == nil || second.MergeTrain == nil {
		t.Fatalf("both 080 rows ride: front=%+v second=%+v", front.MergeTrain, second.MergeTrain)
	}
	for _, mt := range []*StageTrackerMergeTrain{front.MergeTrain, second.MergeTrain} {
		if mt.Size != 2 || len(mt.Members) != 2 {
			t.Errorf("size counts the two riders in this payload, not the planner's nine: %+v", mt)
		}
	}
	if second.MergeTrain.Position != 2 || len(second.MergeTrain.Ahead) != 1 || second.MergeTrain.Ahead[0] != 15516 {
		t.Errorf("close ranks to 2nd of 2 behind the front: %+v", second.MergeTrain)
	}
	if front.MergeGate == nil || !strings.Contains(front.MergeGate.Reason, "1st of 2") {
		t.Errorf("front gate = %+v", front.MergeGate)
	}
}

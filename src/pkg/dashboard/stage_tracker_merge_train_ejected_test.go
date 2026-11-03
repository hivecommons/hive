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

// The rule (hivecommons/hive#15825): items that have not passed the review
// or CI is not green should not be (a) ready for merge, (b) in a merge
// train — and "they should not be in the merge queue column". These tests
// pin the tracker column: a PR whose current head has pending CI or owed
// verdicts renders below 080 with a caption that says what is owed, the
// gray pill carrying the lineage, however it was seated before.

func ejectedTrainFixture(t *testing.T, now time.Time) (*Server, *stagelabels.Store, func(n int, stage string, head string)) {
	t.Helper()
	s, store := stageTrackerFixture(t, now)
	add := func(n int, stage string, head string) {
		s.statusMu.Lock()
		defer s.statusMu.Unlock()
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Docs change", Author: "contrib", BaseRef: "v5", HeadSHA: head, Labels: []string{stage, "author/contributor"}}})
	}
	return s, store, add
}

// TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_CIPendingOnCurrentHead:
// the live front #15748 (approval recorded, new push at 11:50, CI
// re-running) must sit in the CI column, not ride a train, and say what
// the push owes.
func TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_CIPendingOnCurrentHead(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 10, 0, 0, time.UTC)
	s, store, add := ejectedTrainFixture(t, now)
	overlap := []string{"hive-merge.md"}
	front := stagelabels.Key("acme/console", 15748)
	store.Observe(front, stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateFront, Front: 15748, Position: 1, Size: 3, OverlapFiles: overlap}.Reason())
	store.NoteActivity(front, stagelabels.Activity{Head: "aaa1111", Comments: -1, Reviews: -1})
	// New push: the lane re-stages the row to 050 with CI pending and the
	// train ejects it.
	store.Observe(front, stagelabels.ReviewPending, "required-check-pending:coverage")
	store.NotePush(front, "bbb2222", now.Add(-3*time.Minute))
	store.NoteTrainEjected(front, "bbb2222", "head-moved", now.Add(-2*time.Minute))
	store.Observe(stagelabels.Key("acme/console", 15804), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateFront, Front: 15804, Position: 1, Size: 2, OverlapFiles: overlap}.Reason())
	store.Observe(stagelabels.Key("acme/console", 15812), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15804, Position: 2, Size: 2, OverlapFiles: overlap}.Reason())
	add(15748, stagelabels.ReviewPending.Label(), "bbb2222")
	add(15804, stagelabels.AwaitingMergeAdmission.Label(), "ccc3333")
	add(15812, stagelabels.AwaitingMergeAdmission.Label(), "ddd4444")

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "pr", 15748)
	if it.Stage >= stagelabels.AwaitingMergeAdmission.Number() {
		t.Fatalf("CI pending on the current head must not render in the merge queue column: stage %d", it.Stage)
	}
	if it.MergeTrain != nil {
		t.Fatalf("an ejected PR rides no train: %+v", it.MergeTrain)
	}
	if it.LeftTrain == nil || it.LeftTrain.Why != "head-moved" || it.LeftTrain.Reason != "merge-train:ejected:head-moved" {
		t.Fatalf("left_train = %+v", it.LeftTrain)
	}
	if it.LeftTrain.Badge != "left merge train · pushed 3m ago" {
		t.Fatalf("lineage pill = %q", it.LeftTrain.Badge)
	}
	if !strings.Contains(it.Caption, "left the merge train") || !strings.Contains(it.Caption, "CI re-running after push") {
		t.Fatalf("caption must say what the push owes: %q", it.Caption)
	}
	if it.MergeGate != nil && it.MergeGate.State == MergeGateStateReady {
		t.Fatalf("not ready for merge: gate %+v", it.MergeGate)
	}
	raw, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason":"merge-train:ejected:head-moved"`, `"badge":"left merge train · pushed 3m ago"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("payload lacks %s:\n%s", want, raw)
		}
	}
	// The ready members close ranks: #15804 leads a train of two.
	lead := trackerItem(t, resp, "pr", 15804)
	if lead.MergeTrain == nil || lead.MergeTrain.Front != 15804 || lead.MergeTrain.Size != 2 || lead.MergeTrain.State != prstage.MergeTrainStateFront {
		t.Fatalf("next ready member is the front: %+v", lead.MergeTrain)
	}
}

// TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_VerdictOwedOnCurrentHead:
// approval recorded for an older head, verdicts owed again for the current
// one — the Review column, with the caption naming the owed re-review.
func TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_VerdictOwedOnCurrentHead(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 10, 0, 0, time.UTC)
	s, store, add := ejectedTrainFixture(t, now)
	key := stagelabels.Key("acme/console", 15812)
	store.Observe(key, stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: 15804, Position: 2, Size: 2}.Reason())
	store.NoteActivity(key, stagelabels.Activity{Head: "aaa1111", Comments: -1, Reviews: -1})
	store.Observe(key, stagelabels.ReviewPending, "contributor-lane:no-verdict")
	store.NoteActivity(key, stagelabels.Activity{Head: "bbb2222", Comments: -1, Reviews: -1})
	store.NoteTrainEjected(key, "bbb2222", "verdict-owed", now.Add(-5*time.Minute))
	store.Observe(stagelabels.Key("acme/console", 15804), stagelabels.AwaitingMergeAdmission, "")
	add(15812, stagelabels.ReviewPending.Label(), "bbb2222")
	add(15804, stagelabels.AwaitingMergeAdmission.Label(), "ccc3333")

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "pr", 15812)
	if it.Stage >= stagelabels.AwaitingMergeAdmission.Number() {
		t.Fatalf("verdicts owed on the current head must not render in the merge queue column: stage %d", it.Stage)
	}
	if it.MergeTrain != nil {
		t.Fatalf("an ejected PR rides no train: %+v", it.MergeTrain)
	}
	if it.LeftTrain == nil || it.LeftTrain.Why != "verdict-owed" || it.LeftTrain.Reason != "merge-train:ejected:verdict-owed" || it.LeftTrain.Badge != "left merge train · 5m ago" {
		t.Fatalf("left_train = %+v", it.LeftTrain)
	}
	if !strings.Contains(it.Caption, "left the merge train") || !strings.Contains(it.Caption, "review re-run owed") {
		t.Fatalf("caption must name the owed review: %q", it.Caption)
	}
	// The lone survivor is no train and keeps its own 080 words.
	alone := trackerItem(t, resp, "pr", 15804)
	if alone.MergeTrain != nil || alone.LeftTrain != nil || alone.Stage != stagelabels.AwaitingMergeAdmission.Number() {
		t.Fatalf("survivor = stage %d train %+v left %+v", alone.Stage, alone.MergeTrain, alone.LeftTrain)
	}
}

// TestStageTrackerAdmittedCandidateRidesNoTrain: a green, approved PR the
// lane found sharing files with a train holds no seat until the next plan —
// no ordinal, never the front, the overlap chip pointed at the front.
func TestStageTrackerAdmittedCandidateRidesNoTrain(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 10, 0, 0, time.UTC)
	s, store, add := ejectedTrainFixture(t, now)
	overlap := []string{"hive-merge.md"}
	store.Observe(stagelabels.Key("acme/console", 15804), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateFront, Front: 15804, Position: 1, Size: 1, OverlapFiles: overlap}.Reason())
	store.Observe(stagelabels.Key("acme/console", 15816), stagelabels.AwaitingMergeAdmission, prstage.MergeTrain{State: prstage.MergeTrainStateAdmitted, Front: 15804, Size: 1, OverlapFiles: overlap}.Reason())
	add(15804, stagelabels.AwaitingMergeAdmission.Label(), "ccc3333")
	add(15816, stagelabels.AwaitingMergeAdmission.Label(), "eee5555")

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "pr", 15816)
	if it.MergeTrain != nil {
		t.Fatalf("an admitted candidate holds no seat yet: %+v", it.MergeTrain)
	}
	if it.Overlap == nil || it.Overlap.Behind != 15804 || !strings.Contains(it.Overlap.Badge, "joins behind #15804") {
		t.Fatalf("overlap = %+v", it.Overlap)
	}
	if !strings.Contains(it.Caption, "Green and approved") || !strings.Contains(it.Caption, "#15804") {
		t.Fatalf("caption = %q", it.Caption)
	}
}

// TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_RedCheckRiderUnseated:
// the live header read "merge train · 8 PRs" over seven seats — the eighth
// was a red-check row at 090. A member whose current head has a failing
// check holds no seat and is not counted in the train's size.
func TestStageTrackerNotGreenOrReviewedIsNotInMergeQueueColumn_RedCheckRiderUnseated(t *testing.T) {
	now := time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"tracker-row.js"}
	for i, n := range []int{15771, 15834, 15831} {
		st := prstage.MergeTrainStateQueuedBehind
		if i == 0 {
			st = prstage.MergeTrainStateFront
		}
		stage := stagelabels.AwaitingMergeAdmission
		if n == 15831 {
			stage = stagelabels.ReadyForHumanMerge
		}
		store.Observe(stagelabels.Key("acme/console", n), stage, prstage.MergeTrain{State: st, Front: 15771, Position: i + 1, Size: 3, OverlapFiles: overlap}.Reason())
		pr := github.PullRequest{Number: n, Title: "Tracker", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: []string{stage.Label(), "author/hive"}}
		if n == 15831 {
			pr.CIStatus = "failure"
			pr.FailingChecks = []string{"build-and-test"}
		}
		s.statusMu.Lock()
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs, FrontendPR{PullRequest: pr})
		s.statusMu.Unlock()
	}
	resp := s.buildStageTracker(s.status, store, now)
	red := trackerItem(t, resp, "pr", 15831)
	if red.MergeTrain != nil {
		t.Fatalf("a red-check row holds no seat: %+v", red.MergeTrain)
	}
	if red.LeftTrain == nil || red.LeftTrain.Why != "checks-failing" {
		t.Fatalf("red row lineage = %+v", red.LeftTrain)
	}
	front := trackerItem(t, resp, "pr", 15771)
	rider := trackerItem(t, resp, "pr", 15834)
	if front.MergeTrain == nil || front.MergeTrain.Size != 2 || rider.MergeTrain == nil || rider.MergeTrain.Size != 2 || rider.MergeTrain.Position != 2 {
		t.Fatalf("size counts seated members only: front=%+v rider=%+v", front.MergeTrain, rider.MergeTrain)
	}
	if got := front.MergeTrain.Members; len(got) != 2 || got[0] != 15771 || got[1] != 15834 {
		t.Fatalf("members = %v", got)
	}
	if !strings.Contains(rider.MergeGate.Reason, "behind #15771 in merge train (2nd of 2") || !strings.Contains(rider.Caption, "behind #15771") {
		t.Fatalf("rider words = gate %q caption %q", rider.MergeGate.Reason, rider.Caption)
	}
}

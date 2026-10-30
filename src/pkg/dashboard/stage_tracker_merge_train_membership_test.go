package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerMergeTrainUnderRepairLeaves: the owner's page read
// "merge train · 2 PRs · 1st: #15405 · overlapping: operator-reference.md"
// while #15405's own chevron said "Fixing" — a maintainer had just
// requested changes. A PR under repair (070 fixing, 071 needs-author, 072
// returned, 085 needs-decision, or red required checks) is not a train
// member: it leaves with a hover note, the survivors close ranks, and a
// train of one is no train (no header, no ordinal).
func TestStageTrackerMergeTrainUnderRepairLeaves(t *testing.T) {
	now := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	overlap := []string{"operator-reference.md"}
	queued := func(front, pos, size int) string {
		return prstage.MergeTrain{State: prstage.MergeTrainStateQueuedBehind, Front: front, Position: pos, Size: size, OverlapFiles: overlap}.Reason()
	}
	addPR := func(n int, stage string) {
		s.status.Repos[0].OpenPrs = append(s.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Docs change", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: []string{stage, "author/hive"}}})
	}
	// Train of 2: the front #15405 just dropped to 070 fixing.
	store.Observe(stagelabels.Key("acme/console", 15405), stagelabels.Fixing, "reviewer-verdict:changes-requested")
	store.Observe(stagelabels.Key("acme/console", 15410), stagelabels.AwaitingMergeAdmission, queued(15405, 2, 2))
	s.statusMu.Lock()
	addPR(15405, stagelabels.Fixing.Label())
	addPR(15410, stagelabels.AwaitingMergeAdmission.Label())
	s.statusMu.Unlock()
	resp := s.buildStageTracker(s.status, store, now)

	fixing := trackerItem(t, resp, "pr", 15405)
	if fixing.Stage != 70 || fixing.MergeTrain != nil {
		t.Fatalf("a PR being fixed rides no train: stage %d train %+v", fixing.Stage, fixing.MergeTrain)
	}
	if fixing.LeftTrain == nil || fixing.LeftTrain.Why != "fixing" || fixing.LeftTrain.Note != stageTrackerLeftTrainNote {
		t.Fatalf("left_train = %+v", fixing.LeftTrain)
	}
	if fixing.MergeGate != nil && (fixing.MergeGate.State == MergeGateStateTrainFront || fixing.MergeGate.State == MergeGateStateQueuedBehind) {
		t.Fatalf("the gate must not keep the train's words: %+v", fixing.MergeGate)
	}
	raw, err := json.Marshal(fixing)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"left_train":{"why":"fixing","note":"` + stageTrackerLeftTrainNote + `"}`; !strings.Contains(string(raw), want) {
		t.Fatalf("payload lacks %s:\n%s", want, raw)
	}
	// The lone survivor is no train: no header, no ordinal, no stale
	// "behind #15405" words.
	alone := trackerItem(t, resp, "pr", 15410)
	if alone.MergeTrain != nil || alone.LeftTrain != nil {
		t.Fatalf("a train of one is no train: train %+v left %+v", alone.MergeTrain, alone.LeftTrain)
	}
	if alone.MergeGate == nil || alone.MergeGate.State != MergeGateStateReady || strings.Contains(alone.MergeGate.Reason, "#15405") {
		t.Fatalf("survivor gate = %+v, want the 080 row's own ready state", alone.MergeGate)
	}
	if strings.Contains(alone.Caption, "#15405") || strings.Contains(alone.Caption, "merge train") {
		t.Fatalf("survivor caption still names the train: %q", alone.Caption)
	}

	// Train of 3 where the middle member is returned to its contributor:
	// the two others close ranks (2nd of 2), and the member that left
	// says so.
	s2, store2 := stageTrackerFixture(t, now)
	store2.Observe(stagelabels.Key("acme/console", 15500), stagelabels.AwaitingMergeAdmission, "")
	store2.Observe(stagelabels.Key("acme/console", 15501), stagelabels.ReturnedToContributor, queued(15500, 2, 3))
	store2.Observe(stagelabels.Key("acme/console", 15502), stagelabels.AwaitingMergeAdmission, queued(15500, 3, 3))
	s2.statusMu.Lock()
	for _, n := range []int{15500, 15501, 15502} {
		stage := stagelabels.AwaitingMergeAdmission.Label()
		if n == 15501 {
			stage = stagelabels.ReturnedToContributor.Label()
		}
		s2.status.Repos[0].OpenPrs = append(s2.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Docs change", Author: "hive-app[bot]", AppAuthored: true, BaseRef: "v5", Labels: []string{stage, "author/hive"}}})
	}
	s2.statusMu.Unlock()
	resp2 := s2.buildStageTracker(s2.status, store2, now)
	returned := trackerItem(t, resp2, "pr", 15501)
	if returned.MergeTrain != nil || returned.LeftTrain == nil || returned.LeftTrain.Why != "returned-to-contributor" {
		t.Fatalf("returned member = train %+v left %+v", returned.MergeTrain, returned.LeftTrain)
	}
	front := trackerItem(t, resp2, "pr", 15500)
	last := trackerItem(t, resp2, "pr", 15502)
	if front.MergeTrain == nil || front.MergeTrain.Position != 1 || front.MergeTrain.Size != 2 || front.MergeTrain.State != MergeGateStateTrainFront {
		t.Fatalf("front after the drop = %+v", front.MergeTrain)
	}
	if last.MergeTrain == nil || last.MergeTrain.Position != 2 || last.MergeTrain.Size != 2 || last.MergeTrain.Front != 15500 {
		t.Fatalf("last after the drop = %+v", last.MergeTrain)
	}
	if fmt.Sprint(last.MergeTrain.Ahead) != "[15500]" || fmt.Sprint(last.MergeTrain.Members) != "[15500 15502]" || fmt.Sprint(front.MergeTrain.Members) != "[15500 15502]" {
		t.Fatalf("ranks did not close: ahead %v members %v / %v", last.MergeTrain.Ahead, last.MergeTrain.Members, front.MergeTrain.Members)
	}

	// The front itself under repair with two members still fit: the next
	// in line leads and the header's "1st" follows it.
	s3, store3 := stageTrackerFixture(t, now)
	store3.Observe(stagelabels.Key("acme/console", 15600), stagelabels.NeedsAuthor, "required-check-failing:build-and-test")
	store3.Observe(stagelabels.Key("acme/console", 15601), stagelabels.AwaitingMergeAdmission, queued(15600, 2, 3))
	store3.Observe(stagelabels.Key("acme/console", 15602), stagelabels.AwaitingMergeAdmission, queued(15600, 3, 3))
	s3.statusMu.Lock()
	for _, n := range []int{15600, 15601, 15602} {
		stage := stagelabels.AwaitingMergeAdmission.Label()
		if n == 15600 {
			stage = stagelabels.NeedsAuthor.Label()
		}
		s3.status.Repos[0].OpenPrs = append(s3.status.Repos[0].OpenPrs,
			FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "Docs change", Author: "someone", BaseRef: "v5", Labels: []string{stage, "author/human"}}})
	}
	s3.statusMu.Unlock()
	resp3 := s3.buildStageTracker(s3.status, store3, now)
	gone := trackerItem(t, resp3, "pr", 15600)
	if gone.MergeTrain != nil || gone.LeftTrain == nil || gone.LeftTrain.Why != "needs-author" {
		t.Fatalf("red front = train %+v left %+v", gone.MergeTrain, gone.LeftTrain)
	}
	newFront := trackerItem(t, resp3, "pr", 15601)
	tail := trackerItem(t, resp3, "pr", 15602)
	if newFront.MergeTrain == nil || newFront.MergeTrain.State != MergeGateStateTrainFront || newFront.MergeTrain.Position != 1 || newFront.MergeTrain.Size != 2 || newFront.MergeTrain.Front != 15601 {
		t.Fatalf("promoted front = %+v", newFront.MergeTrain)
	}
	if newFront.MergeGate == nil || newFront.MergeGate.State != MergeGateStateTrainFront || !strings.HasPrefix(newFront.MergeGate.Reason, "1st in the merge queue; merges next (1st of 2") {
		t.Fatalf("promoted front gate = %+v", newFront.MergeGate)
	}
	if tail.MergeTrain == nil || tail.MergeTrain.Position != 2 || tail.MergeTrain.Size != 2 || tail.MergeTrain.Front != 15601 || fmt.Sprint(tail.MergeTrain.Ahead) != "[15601]" {
		t.Fatalf("tail after the front left = %+v", tail.MergeTrain)
	}

	// stageTrackerTrainUnderRepair names each repair, and nothing else.
	for stage, want := range map[int]string{70: "fixing", 71: "needs-author", 72: "returned-to-contributor", 85: "needs-decision", 80: "", 50: "", 60: "", 95: ""} {
		if got := stageTrackerTrainUnderRepair(stage, ""); got != want {
			t.Errorf("underRepair(%d) = %q, want %q", stage, got, want)
		}
	}
	if got := stageTrackerTrainUnderRepair(60, "required-check-failing:lint"); got != "checks-failing" {
		t.Errorf("red checks = %q", got)
	}
	if got := stageTrackerTrainUnderRepair(50, "required-check-pending:lint"); got != "" {
		t.Errorf("a pending check is provisional, not repair: %q", got)
	}
}

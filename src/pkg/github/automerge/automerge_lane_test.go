package automerge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// laneGate records what each sweep path hands to the serialized lane and
// answers with a scripted outcome.
type laneGate struct {
	reqs   []hgithub.LaneMergeRequest
	answer func(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error)
}

func (g *laneGate) gate(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
	g.reqs = append(g.reqs, req)
	return g.answer(ctx, req)
}

func laneAnswer(outcome, reason string) func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
	return func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		res := hgithub.LaneMergeResult{Outcome: outcome, Reason: reason}
		if outcome == hgithub.LaneOutcomeMerged {
			res.SHA, res.Method = "lanesha", "squash"
		}
		return res, nil
	}
}

func newLaneSweepEngine(apiURL, strategy string, gate hgithub.SerializedLaneGate, opts Options) *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	client.SetSerializedLane(func(string) string { return strategy }, gate)
	opts.MergerAuthorizer = func(login string) bool {
		return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
	}
	return New(client, opts)
}

func greenQueuedPR(number int) sweepPR {
	return sweepPR{number: number, author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"}
}

func greenSelfPR(number int) selfAuthoredPR {
	return selfAuthoredPR{number: number, author: testHiveAppBotLogin, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"}
}

// AC2: a direct repo never consults the lane and merges exactly as today.
func TestLaneSweep_DirectRepoMergesAsToday(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7)}, &merged)
	defer api.Close()
	g := &laneGate{answer: func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		t.Fatal("a direct repo must never consult the lane")
		return hgithub.LaneMergeResult{}, nil
	}}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyDirect, g.gate, Options{})
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Merged) != 1 || len(merged) != 1 || merged[0] != 7 {
		t.Fatalf("merged = %v, calls = %v; want today's direct merge of PR 7", result.Merged, merged)
	}
}

// AC7: a lgtm-queued PR that is not at the front of the lane is deferred
// with that reason and no merge call; it stays queued for the next sweep.
func TestLaneSweep_QueuedNotAtFrontIsDeferred(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7)}, &merged)
	defer api.Close()
	g := &laneGate{answer: laneAnswer(hgithub.LaneOutcomeDeferred, "deferred: not at the front of the lane")}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{})

	event, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel)
	if err != nil || reason != "deferred: not at the front of the lane" || event.Number != 0 {
		t.Fatalf("trySweepQueuedPR = %+v, %q, %v; want a deferral", event, reason, err)
	}
	if len(merged) != 0 {
		t.Fatalf("merge calls = %v, want none", merged)
	}
	if len(g.reqs) != 1 || g.reqs[0].Repo != "acme/widget" || g.reqs[0].Number != 7 || g.reqs[0].Path != hgithub.PRAuditPathQueue {
		t.Fatalf("lane requests = %+v", g.reqs)
	}
}

// The lgtm path's authorization for the lane's final re-check is the queue
// approval itself: it holds for the approved head only.
func TestLaneSweep_QueuedMergesWithApprovalAuthorization(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7)}, &merged)
	defer api.Close()
	g := &laneGate{answer: func(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		if err := req.Authorize(ctx, "sha7"); err != nil {
			t.Errorf("approval for the approved head: %v", err)
		}
		if err := req.Authorize(ctx, "moved"); err == nil {
			t.Error("the approval must not cover a different head")
		}
		return laneAnswer(hgithub.LaneOutcomeMerged, "merged")(ctx, req)
	}}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{})
	var audits []AutoMergeSweepEvent
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{
		Audit: func(e AutoMergeSweepEvent) { audits = append(audits, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 0 {
		t.Fatalf("the sweep itself made merge calls %v; only the lane merges", merged)
	}
	if len(result.Merged) != 1 || len(audits) != 1 || audits[0].QueuedBy != "bob" || audits[0].MergeSHA != "lanesha" || audits[0].HeadSHA != "sha7" {
		t.Fatalf("merged = %+v, audits = %+v", result.Merged, audits)
	}
}

// Inv. 7: the max_merges loop never merges two PRs into one lane branch in
// one pass; the second PR is not even handed to the lane.
func TestLaneSweep_OneMergePerLaneBranchPerPass(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7), greenQueuedPR(8)}, &merged)
	defer api.Close()
	g := &laneGate{answer: laneAnswer(hgithub.LaneOutcomeMerged, "merged")}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{})
	result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{MaxMerges: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Merged) != 1 || len(g.reqs) != 1 || result.Skipped != 1 {
		t.Fatalf("merged = %+v, lane requests = %d, skipped = %d; want one merge per lane branch per pass", result.Merged, len(g.reqs), result.Skipped)
	}
}

// AC26 and fail-closed outcomes surface as skips with the lane's reason.
func TestLaneSweep_RefusalAndErrorAreSkips(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{greenQueuedPR(7)}, &merged)
	defer api.Close()

	refusal := `target branch "main" has GitHub's native merge queue; Hive makes no direct merge`
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, (&laneGate{answer: laneAnswer(hgithub.LaneOutcomeRefused, refusal)}).gate, Options{})
	if _, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel); err != nil || reason != refusal {
		t.Fatalf("reason = %q, err = %v; want the native merge queue refusal", reason, err)
	}

	c = newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, (&laneGate{answer: func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		return hgithub.LaneMergeResult{}, errors.New("lane record unwritable")
	}}).gate, Options{})
	if _, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel); err == nil || reason != "lane record unwritable" {
		t.Fatalf("reason = %q, err = %v; want the lane error", reason, err)
	}

	c = newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, nil, Options{})
	if _, reason, err := c.trySweepQueuedPR(context.Background(), "widget", "acme", "widget", 7, hgithub.AutoMergeQueuedLabel); err != nil || reason != hgithub.ReasonLaneUnavailable {
		t.Fatalf("reason = %q, err = %v; want %q", reason, err, hgithub.ReasonLaneUnavailable)
	}
	if len(merged) != 0 {
		t.Fatalf("merge calls = %v, want none", merged)
	}
}

// AC4: auto-merge off or a held PR never reaches the lane.
func TestLaneSweep_GatesAheadOfTheLane(t *testing.T) {
	var merged []int
	held := greenQueuedPR(7)
	held.extraLabels = []string{"hold"}
	api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{held}, &merged)
	defer api.Close()
	g := &laneGate{answer: laneAnswer(hgithub.LaneOutcomeMerged, "merged")}

	off := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{RepoAutoMergeEnabled: func(string) bool { return false }})
	if result, err := off.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil || len(result.Merged) != 0 {
		t.Fatalf("auto-merge off: result = %+v, err = %v", result, err)
	}
	on := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{})
	if result, err := on.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil || len(result.Merged) != 0 {
		t.Fatalf("held PR: result = %+v, err = %v", result, err)
	}
	if len(g.reqs) != 0 || len(merged) != 0 {
		t.Fatalf("lane requests = %d, merges = %v; want none", len(g.reqs), merged)
	}
}

// AC7 for the self-merge sweep, plus the authorization bound to the head the
// sweep evaluated green.
func TestLaneSweep_SelfAuthoredGoesThroughLane(t *testing.T) {
	var merged []int
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{greenSelfPR(11)}, &merged)
	defer api.Close()

	deferred := &laneGate{answer: laneAnswer(hgithub.LaneOutcomeDeferred, "deferred: not at the front of the lane")}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, deferred.gate, Options{})
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil || len(result.Merged) != 0 || len(merged) != 0 || len(deferred.reqs) != 1 {
		t.Fatalf("deferred: result = %+v, err = %v, merges = %v, lane = %d", result, err, merged, len(deferred.reqs))
	}
	if req := deferred.reqs[0]; req.Path != hgithub.PRAuditPathSweep || req.Number != 11 || req.Method == "" {
		t.Fatalf("lane request = %+v", req)
	}

	var head string // the repo the front request named
	front := &laneGate{answer: func(ctx context.Context, req hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
		head = req.Repo
		if err := req.Authorize(ctx, "not-the-evaluated-head"); err == nil {
			t.Error("authorization must be bound to the evaluated head")
		}
		return laneAnswer(hgithub.LaneOutcomeMerged, "merged")(ctx, req)
	}}
	c = newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, front.gate, Options{})
	result, err = c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil || len(result.Merged) != 1 || result.Merged[0].MergeSHA != "lanesha" || len(merged) != 0 || head != "acme/widget" {
		t.Fatalf("front: result = %+v, err = %v, merges = %v", result, err, merged)
	}
	if err := front.reqs[0].Authorize(context.Background(), result.Merged[0].HeadSHA); err != nil {
		t.Fatalf("authorization for the evaluated head: %v", err)
	}
}

// A behind self-authored PR in a hive-serialized repo is handed to the lane
// (which updates only its front PR, pinned) instead of today's unpinned
// update; a lane update counts as the sweep's updated-branch.
func TestLaneSweep_BehindSelfAuthoredDefersToLaneUpdate(t *testing.T) {
	var merged []int
	updates := 0
	pr := greenSelfPR(11)
	pr.mergeableState = "behind"
	pr.updateCount = &updates
	api := newSelfAuthoredAutoMergeAPI(t, []selfAuthoredPR{pr}, &merged)
	defer api.Close()
	g := &laneGate{answer: laneAnswer(hgithub.LaneOutcomeUpdated, "merged the tip in")}
	c := newLaneSweepEngine(api.URL, config.MergeStrategyHiveSerialized, g.gate, Options{})
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updates != 0 || len(merged) != 0 || len(g.reqs) != 1 || result.UpdatedBranches != 1 {
		t.Fatalf("updates = %d, merges = %v, lane = %d, result = %+v", updates, merged, len(g.reqs), result)
	}
}

func TestLaneMergedBranchesWithoutPassContext(t *testing.T) {
	if laneMergedBranchesFromContext(context.Background()) != nil {
		t.Fatal("no pass context, no record")
	}
	c := &Engine{}
	if serialized, gate := c.serializedLane("widget"); serialized || gate != nil {
		t.Fatal("a transport without the lane capability is direct")
	}
}

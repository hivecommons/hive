package dashboard

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	forgequeue "github.com/hivecommons/hive/pkg/forgestate"
)

func TestNativeQueueTrackerOffScopeStaleAndCopy(t *testing.T) {
	s := covApiServer(t)
	now := time.Now()
	s.nativeQueueTracker.observations = map[string]forgequeue.QueueObservation{
		"o/r#v5": {Provider: "github-native", Repo: "o/r", Base: "v5", State: "observed", Complete: true,
			ObservedAt: now.Add(-time.Hour), Candidates: []forgequeue.QueueCandidate{{SHA: "candidate", Membership: "proven", Members: []forgequeue.QueueMember{{PR: 7, Head: "head"}}, Checks: []forgequeue.QueueCheck{{Name: "test"}}}}},
		"o/r#v6": {Repo: "o/r", Base: "v6"},
	}
	if s.stageTrackerNativeQueues(now) != nil || s.ReconcileNativeQueues(context.Background()) != nil || !s.nativeQueueTracker.lastAttempt.IsZero() {
		t.Fatal("default-off native observations have side effects")
	}
	s.deps.Config.AutoMerge.NativeMergeQueue = config.NativeMergeQueueAutoMergeConfig{Enabled: true, Repos: []string{"o/r"}, Bases: []string{"o/r#v5"}}
	got := s.stageTrackerNativeQueues(now)
	if len(got) != 1 || !got[0].Stale || got[0].Base != "v5" || got[0].FreshForS == 0 {
		t.Fatalf("scope/staleness lost: %+v", got)
	}
	got[0].Candidates[0].Members[0].Head = "mutated"
	got[0].Candidates[0].Checks[0].Name = "mutated"
	if original := s.stageTrackerNativeQueues(now)[0]; original.Candidates[0].Members[0].Head != "head" || original.Candidates[0].Checks[0].Name != "test" {
		t.Fatal("render mutated observation cache")
	}
	s.deps.Config.AutoMerge.NativeMergeQueue.Bases = []string{"o/r#other"}
	got = s.stageTrackerNativeQueues(now)
	if len(got) != 1 || got[0].State != "unknown" || got[0].Base != "other" || len(got[0].Candidates) != 0 {
		t.Fatalf("missing scope fabricated empty success: %+v", got)
	}
	if err := s.ReconcileNativeQueues(context.Background()); err == nil {
		t.Fatal("missing installation authentication accepted")
	}
	got = s.stageTrackerNativeQueues(now)
	if len(got) != 1 || got[0].State != "unavailable" || !strings.Contains(got[0].Reason, "authentication") {
		t.Fatalf("authentication diagnostic unavailable: %+v", got)
	}
	if err := s.ReconcileNativeQueues(context.Background()); err != nil {
		t.Fatal("bounded retry interval not respected")
	}
}

func TestNativeQueueLifecycleNeverMergesAtEnqueue(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.AutoMerge.NativeMergeQueue = config.NativeMergeQueueAutoMergeConfig{Enabled: true, Repos: []string{"o/r"}, Bases: []string{"o/r#v5"}}
	now := time.Now()
	s.nativeQueueTracker.observations = map[string]forgequeue.QueueObservation{"o/r#v5": {
		Provider: "github-native", Repo: "o/r", Base: "v5", State: "observed", Complete: true, ObservedAt: now,
		Entries: []forgequeue.QueueEntry{{ID: "entry", PR: 7, Head: "head", Position: 1}},
	}}
	deliver := func(action string) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"action": action, "repository": map[string]any{"full_name": "o/r"},
			"merge_group": map[string]any{"base_ref": "refs/heads/v5", "base_sha": strings.Repeat("a", 40),
				"head_ref": "refs/heads/gh-readonly-queue/v5/pr-7", "head_sha": strings.Repeat("b", 40)},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.observeNativeQueueEvent("merge_group", body, now)
	}
	deliver("checks_requested")
	if got := s.stageTrackerNativeQueues(now); len(got[0].Outcomes) != 0 || len(s.nativeQueueTracker.hints) != 1 {
		t.Fatal("group creation fabricated merged outcome")
	}
	deliver("destroyed")
	deliver("destroyed")
	if got := s.stageTrackerNativeQueues(now); len(got[0].Outcomes) != 1 || got[0].Outcomes[0].Kind != "candidate-destroyed" || len(s.nativeQueueTracker.hints) != 0 {
		t.Fatalf("destroyed delivery not idempotent: %+v", got)
	}
	for _, head := range []string{"other", "head"} {
		body := []byte(`{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"merge_commit_sha":"merge","head":{"sha":"` + head + `"},"base":{"ref":"v5"}}}`)
		s.observeNativeQueueEvent("pull_request", body, now)
	}
	got := s.stageTrackerNativeQueues(now)
	if len(got[0].Outcomes) != 2 || got[0].Outcomes[1].Kind != "merged" || got[0].Outcomes[1].MergeSHA != "merge" {
		t.Fatalf("exact merged receipt missing: %+v", got)
	}
	// No pipeline rows or merge ledger entries are rewritten by observations.
	if !reflect.DeepEqual(got[0].Entries, []forgequeue.QueueEntry{{ID: "entry", PR: 7, Head: "head", Position: 1}}) {
		t.Fatal("outcome observation rewrote the queue into a pipeline")
	}
}

func TestNativeQueueOutcomeHistoryBounded(t *testing.T) {
	var outcomes []forgequeue.QueueOutcome
	for pr := 1; pr <= nativeQueueObservationHistory+1; pr++ {
		outcomes = appendNativeQueueOutcome(outcomes, forgequeue.QueueOutcome{Kind: "entry-removed", PR: pr})
	}
	if len(outcomes) != nativeQueueObservationHistory || outcomes[0].PR != 2 {
		t.Fatal("unbounded native lifecycle history")
	}
}

func TestNativeQueueRemovalRequiresCompleteSnapshots(t *testing.T) {
	prior := forgequeue.QueueObservation{EntriesComplete: true, Entries: []forgequeue.QueueEntry{{ID: "entry", PR: 7, Head: "head"}}}
	for _, complete := range []bool{false, true} {
		next := nativeQueueObservationDelta(prior, forgequeue.QueueObservation{EntriesComplete: complete}, time.Now())
		if (len(next.Outcomes) == 1) != complete {
			t.Fatalf("partial snapshot claimed removal: %+v", next)
		}
		if complete && (next.Outcomes[0].Kind != "entry-removed" || next.Outcomes[0].MergeSHA != "") {
			t.Fatal("removal fabricated a merge receipt")
		}
	}
	prior.EntriesComplete = false
	if got := nativeQueueObservationDelta(prior, forgequeue.QueueObservation{EntriesComplete: true}, time.Now()); len(got.Outcomes) != 0 {
		t.Fatal("incomplete prior snapshot claimed removal")
	}
}

func TestNativeQueueSnapshotCacheBoundedAcrossScopeChanges(t *testing.T) {
	var runtime nativeQueueTrackerRuntime
	for i := 0; i <= nativeQueueObservationCache; i++ {
		key := time.Unix(int64(i), 0).String()
		runtime.storeObservation(key, forgequeue.QueueObservation{ObservedAt: time.Unix(int64(i), 0)})
	}
	if len(runtime.observations) != nativeQueueObservationCache {
		t.Fatal("scope changes created an unbounded snapshot cache")
	}
	if _, exists := runtime.observations[time.Unix(0, 0).String()]; exists {
		t.Fatal("oldest observation not evicted")
	}
}

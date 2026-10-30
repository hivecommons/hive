package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#15808: a merge attempt that landed patches the cached
// snapshot like a webhook `closed` read — the row leaves as merged, the
// merged ledger records it as the hive's, and open tabs are nudged — so
// the celebration never waits on the webhook or the next enumerate.
func TestNoteMergeAttemptMergedEvictsRowRecordsLedgerAndNudges(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 30, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	clock := now
	s.stageTrackerNudge.now = func() time.Time { return clock }
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("first payload builds")
	}
	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	attempts := github.NewMergeAttempts(func() time.Time { return clock })
	attempts.SetOnChange(s.noteMergeAttempt)
	attempts.BeginOnHead("acme/console", 101, github.PRAuditPathSweep, "abc1234")
	select {
	case f := <-ch:
		if !strings.HasPrefix(string(f), "event: stage-tracker\n") {
			t.Fatalf("frame = %q", f)
		}
	default:
		t.Fatal("beginning a merge attempt must nudge open tabs")
	}
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("beginning a merge attempt must drop the memoised payload")
	}
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	// Past the nudge coalescing window, so the landing's nudge goes out at once.
	clock = now.Add(2 * stageTrackerNudgeInterval)
	attempts.End("acme/console", 101, true, nil)

	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	after := s.buildStageTracker(cur, s.stageStore, now)
	for _, it := range after.Items {
		if it.Kind == "pr" && it.Number == 101 {
			t.Fatalf("merged PR 101 still listed open: %+v", it)
		}
	}
	recent := s.mergedLedger().Summary(now, time.UTC).Recent
	found := false
	for _, e := range recent {
		if e.Number == 101 {
			found = true
			if e.MergedBy != MergedByHive || !e.MergedAt.Equal(clock) {
				t.Fatalf("ledger entry = %+v, want merged by hive at %s", e, clock)
			}
		}
	}
	if !found {
		t.Fatalf("merged ledger recent = %+v, want PR 101", recent)
	}
	select {
	case f := <-ch:
		if !strings.HasPrefix(string(f), "event: stage-tracker\n") {
			t.Fatalf("frame = %q", f)
		}
	default:
		t.Fatal("a landed merge must nudge open tabs")
	}
}

// A refused attempt (or one on a PR the snapshot never listed) still drops
// the memoised payload and nudges, so "Merge failed" shows within one poll.
func TestNoteMergeAttemptRefusedNudgesWithoutEvicting(t *testing.T) {
	now := time.Date(2026, 10, 30, 15, 30, 0, 0, time.UTC)
	t.Cleanup(github.ResetWebhookTrackerForTestWithClock(func() time.Time { return now }))
	s, _ := stageTrackerFixture(t, now)
	clock := now
	s.stageTrackerNudge.now = func() time.Time { return clock }
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("first payload builds")
	}
	attempts := github.NewMergeAttempts(func() time.Time { return clock })
	attempts.SetOnChange(s.noteMergeAttempt)
	attempts.Begin("acme/console", 101, github.PRAuditPathSweep)
	if _, _, _, fresh := s.stageTrackerPayload(s.status, now); !fresh {
		t.Fatal("beginning a merge attempt must drop the memoised payload")
	}
	ch := make(chan []byte, 4)
	s.sseMu.Lock()
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()
	clock = now.Add(2 * stageTrackerNudgeInterval)
	attempts.End("acme/console", 101, false, errors.New("base branch was modified"))

	s.statusMu.RLock()
	cur := s.status
	s.statusMu.RUnlock()
	after := s.buildStageTracker(cur, s.stageStore, now)
	trackerItem(t, after, "pr", 101)
	if len(s.mergedLedger().Summary(now, time.UTC).Recent) != 0 {
		t.Fatal("a refused merge must not reach the merged ledger")
	}
	select {
	case <-ch:
	default:
		t.Fatal("a refused merge attempt must nudge open tabs")
	}
}

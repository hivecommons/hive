package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestHeartbeatEvidenceTimeToMerge pins the per-window median time-to-merge
// on the heartbeat (#12464): only merged PRs that recorded merge.opened_at
// count, a merge without it is skipped rather than counted as zero, and
// each window covers the PRs opened inside it.
func TestHeartbeatEvidenceTimeToMerge(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	merged := func(n int, created, opened, at time.Time) *evidence.Bundle {
		b := evSummaryBundle("a/b", n, evidence.AuthorAgent, created, true, nil, 0, true)
		b.Merge.At, b.Merge.OpenedAt = at, opened
		return b
	}
	// PR 1: opened 3h ago, merged 1h ago (2h); a newer live bundle without
	// the merge folds into the same PR.
	pr1 := merged(1, now.Add(-2*time.Hour), now.Add(-3*time.Hour), now.Add(-time.Hour))
	live1 := evSummaryBundle("a/b", 1, evidence.AuthorAgent, now.Add(-30*time.Minute), false, nil, 0, true)
	// PR 2: opened 3d1h ago, merged after 24h.
	pr2 := merged(2, now.Add(-72*time.Hour), now.Add(-73*time.Hour), now.Add(-49*time.Hour))
	// PR 3: opened 20d1h ago, merged after 6h.
	pr3 := merged(3, now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour-time.Hour), now.Add(-20*24*time.Hour+5*time.Hour))
	// PR 4: merged before opened_at was captured: skipped, never zero.
	pr4 := merged(4, now.Add(-time.Hour), time.Time{}, now)
	// PR 5: still open.
	pr5 := evSummaryBundle("a/b", 5, evidence.AuthorAgent, now.Add(-time.Hour), false, nil, 0, true)

	got := buildHeartbeatEvidence([]*evidence.Bundle{pr1, live1, pr2, pr3, pr4, pr5}, now, nil)
	want := map[string]*spoke.HeartbeatEvidenceTimeToMerge{
		"90d": {MedianSeconds: 6 * 3600, N: 3},
		"30d": {MedianSeconds: 6 * 3600, N: 3},
		"7d":  {MedianSeconds: 13 * 3600, N: 2},
		"24h": {MedianSeconds: 2 * 3600, N: 1},
	}
	blocks := map[string]*spoke.HeartbeatEvidence{"90d": got}
	for key := range spoke.HeartbeatEvidenceWindowKeys {
		blocks[key] = got.Windows[key]
	}
	for key, w := range want {
		b := blocks[key]
		if b == nil || b.TimeToMerge == nil || *b.TimeToMerge != *w {
			t.Errorf("%s time_to_merge = %+v, want %+v", key, b, w)
		}
	}
	if got.Merged90d != 4 {
		t.Errorf("merged = %d, want 4 (time-to-merge must not change the merged count)", got.Merged90d)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"time_to_merge":{"median_s":21600,"n":3}`) {
		t.Errorf("wire shape: %s", raw)
	}
}

// TestHeartbeatEvidenceTimeToMergeUnreported: with no merged PR carrying
// merge.opened_at the block omits time_to_merge entirely, so the hub shows
// "not reported" instead of a zero median.
func TestHeartbeatEvidenceTimeToMergeUnreported(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	b := evSummaryBundle("a/b", 1, evidence.AuthorAgent, now.Add(-time.Hour), true, nil, 0, true)
	b.Merge.At = now
	got := buildHeartbeatEvidence([]*evidence.Bundle{b}, now, nil)
	if got.TimeToMerge != nil {
		t.Errorf("time_to_merge = %+v, want nil", got.TimeToMerge)
	}
	for key, w := range got.Windows {
		if w.TimeToMerge != nil {
			t.Errorf("%s time_to_merge = %+v, want nil", key, w.TimeToMerge)
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "time_to_merge") {
		t.Errorf("time_to_merge must be omitted: %s", raw)
	}
	if heartbeatTimeToMerge(nil) != nil {
		t.Error("heartbeatTimeToMerge(nil) must be nil")
	}
}

package github

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestMergedPRsSince pins the lifecycle backfill listing (#12007): merged PRs
// inside the window come back with their real created_at/merged_at and the
// same-repo issues they close; closed-without-merge PRs, merges before the
// window and everything past the first PR updated before it are skipped.
func TestMergedPRsSince(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-MergedPRBackfillWindow)
	created := now.Add(-50 * time.Hour)
	merged := now.Add(-48 * time.Hour)
	ancient := since.Add(-24 * time.Hour)

	fixed := mergedPR(600, "kubestellar-hive[bot]", "fix it", "Fixes #300\nCloses torch-spyre/spyre-inference#301\nFixes other/repo#302\nRefs #303", merged, merged)
	fixed["created_at"] = created.Format(time.RFC3339)
	srv := mergedClaimServer(t, nil, []map[string]any{
		fixed,
		mergedPR(601, "kubestellar-hive[bot]", "abandoned", "Fixes #304", time.Time{}, merged),
		mergedPR(602, "kubestellar-hive[bot]", "old fix", "Fixes #305", ancient, merged),
		mergedPR(603, "kubestellar-hive[bot]", "stale", "Fixes #306", ancient, ancient),
		mergedPR(604, "kubestellar-hive[bot]", "unreachable", "Fixes #307", merged, ancient),
	})
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())

	got, err := c.MergedPRsSince(context.Background(), since)
	if err != nil {
		t.Fatalf("MergedPRsSince: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d observations (%+v), want 1", len(got), got)
	}
	obs := got[0]
	if obs.Number != 600 || obs.State != PRStateMerged {
		t.Fatalf("observation = %+v", obs)
	}
	if !obs.CreatedAt.Equal(created) || !obs.ObservedAt.Equal(merged) {
		t.Errorf("timestamps = created %v merged %v, want %v / %v", obs.CreatedAt, obs.ObservedAt, created, merged)
	}
	if !reflect.DeepEqual(obs.Issues, []int{300, 301}) {
		t.Errorf("issues = %v, want [300 301] (same-repo closing refs only)", obs.Issues)
	}
	if obs.URL == "" {
		t.Error("URL not set")
	}

	var nilClient *Client
	if _, err := nilClient.MergedPRsSince(context.Background(), since); err == nil {
		t.Error("nil client must error")
	}
}

// TestFetchClaimScan_MergedObservationCarriesIssueLinks: the merge sweep's
// terminal observation carries the PR's created_at and closing issues so the
// lifecycle hook can backfill them each sweep (#12007).
func TestFetchClaimScan_MergedObservationCarriesIssueLinks(t *testing.T) {
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	created := recent.Add(-3 * time.Hour)
	pr := mergedPR(700, "kubestellar-hive[bot]", "fix", "Fixes #310", recent, recent)
	pr["created_at"] = created.Format(time.RFC3339)
	srv := mergedClaimServer(t, nil, []map[string]any{pr})
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	var seen []PRTerminalObservation
	c.SetPRTerminalObservedHook(func(o PRTerminalObservation) { seen = append(seen, o) })

	if _, err := c.FetchClaimScan(context.Background(), HiveIdentity{AppLogin: "kubestellar-hive[bot]"}); err != nil {
		t.Fatalf("FetchClaimScan: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("observations = %+v, want 1", seen)
	}
	if !seen[0].CreatedAt.Equal(created) || !reflect.DeepEqual(seen[0].Issues, []int{310}) || seen[0].URL == "" {
		t.Errorf("merged observation = %+v", seen[0])
	}
}

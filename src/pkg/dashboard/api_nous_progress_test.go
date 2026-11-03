package dashboard

import (
	"testing"
	"time"
)

func TestNousExperimentRowComputesPhaseSnapshotsAndMetrics(t *testing.T) {
	now := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	start := now.Add(-90 * time.Minute)
	buckets := []nousBucketLite{
		{Start: start.Add(10 * time.Minute), Samples: 2, QueueIssues: []int{8, 10}, QueuePRs: []int{2}, QueueHold: []int{1}, SLAViolatedSamples: 1, Kicks: 1, Tokens: 100},
		{Start: start.Add(70 * time.Minute), Samples: 2, QueueIssues: []int{4, 6}, QueuePRs: []int{1}, QueueHold: []int{0}, Kicks: 2, Tokens: 80},
	}
	row := nousExperimentRow(map[string]interface{}{
		"id":         "exp-1",
		"type":       "active",
		"hypothesis": "shard slow tests",
		"started_at": start.Format(time.RFC3339),
	}, "active", 1, 2, 1, now, buckets, nil)

	if row.Phase != "experiment" {
		t.Fatalf("phase = %q, want experiment", row.Phase)
	}
	if row.SnapshotsInPhase != 2 {
		t.Fatalf("snapshots in phase = %d, want 2", row.SnapshotsInPhase)
	}
	if row.SnapshotCounts["baseline"] != 2 || row.SnapshotCounts["experiment"] != 2 {
		t.Fatalf("snapshot counts = %+v", row.SnapshotCounts)
	}
	if len(row.MetricsSoFar) == 0 {
		t.Fatal("metricsSoFar is empty")
	}
	var queueMetric *nousExperimentMetric
	for i := range row.MetricsSoFar {
		if row.MetricsSoFar[i].Name == "queue_issues_p50" {
			queueMetric = &row.MetricsSoFar[i]
		}
	}
	if queueMetric == nil || queueMetric.Baseline == nil || queueMetric.Experiment == nil {
		t.Fatalf("queue metric = %+v", queueMetric)
	}
	if *queueMetric.Baseline != 8 || *queueMetric.Experiment != 4 {
		t.Fatalf("queue baseline/experiment = %v/%v, want 8/4", *queueMetric.Baseline, *queueMetric.Experiment)
	}
	if row.NextOutputETASeconds <= 0 {
		t.Fatalf("next output eta = %d", row.NextOutputETASeconds)
	}
}

func TestAddNousProgressStatusBaselineAndResults(t *testing.T) {
	s, deps := apiServerWithNous(t)
	deps.Config.Governor.EvalIntervalS = 60
	deps.Nous.Status = map[string]interface{}{"snapshotCount": 671}
	status := map[string]interface{}{"snapshotCount": 671}
	ledger := []map[string]interface{}{{
		"id":        "exp-2",
		"type":      "completed",
		"verdict":   "improved",
		"delta":     "-20%",
		"pr_url":    "https://github.com/hivecommons/hive/pull/1",
		"timestamp": "2026-10-16T12:00:00Z",
	}}

	s.addNousProgressStatus(status, nil, ledger, nil)

	base, ok := status["baseline"].(nousBaselineProgress)
	if !ok {
		t.Fatalf("baseline = %#v", status["baseline"])
	}
	if base.RemainingSnapshots != 1 || base.ETASeconds != 60 || base.Complete {
		t.Fatalf("baseline = %+v", base)
	}
	results, ok := status["results"].([]nousExperimentProgress)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %#v", status["results"])
	}
	if len(results[0].Outputs) != 1 || results[0].Outputs[0].URL == "" {
		t.Fatalf("outputs = %+v", results[0].Outputs)
	}
}

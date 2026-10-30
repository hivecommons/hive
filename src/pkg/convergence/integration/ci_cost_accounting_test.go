package integration

import (
	"strings"
	"testing"
	"time"
)

func TestAssignedMetadataDoesNotTurnQueuedSkippedOrIncompleteJobsIntoKnownExecution(t *testing.T) {
	from, through := epoch.Add(time.Hour), epoch.Add(2*time.Hour)
	coverage := Coverage{From: from, Through: through, Complete: true}
	for _, tc := range []struct {
		name, status, conclusion string
		end                      time.Time
		complete                 bool
		minutes                  float64
	}{
		{"queued", "queued", "", through, true, 0},
		{"waiting", "waiting", "", through, true, 0},
		{"skipped", "completed", "skipped", through, true, 0},
		{"missing-status", "", "success", through, false, 0},
		{"missing-end", "completed", "success", time.Time{}, false, 0},
		{"clipped", "completed", "cancelled", through.Add(time.Hour), true, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job := Job{Repo: "o/r", Base: "v5", ID: 1, RunID: 2, Attempt: 1, Head: strings.Repeat("a", 40), RunnerID: 3,
				Status: tc.status, Conclusion: tc.conclusion, StartedAt: epoch, CompletedAt: tc.end, ObservedAt: through}
			if job.CompletedAt.After(job.ObservedAt) {
				job.ObservedAt = job.CompletedAt
			}

			report := cost(map[string]Job{job.Key(): job}, coverage, "o/r", "v5", from, through)
			if report.Complete != tc.complete || report.ObservedRunnerMinutes != tc.minutes {
				t.Fatalf("incorrect actual execution: %+v", report)
			}
			if tc.complete && (report.RunnerMinutes == nil || *report.RunnerMinutes != tc.minutes) || !tc.complete && report.RunnerMinutes != nil {
				t.Fatalf("unknown presented as known: %+v", report)
			}
		})
	}
}

package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationExecutionAccountingObservesDuplicateAndHistoricalAttemptsWithoutProof(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, test := range []struct {
		name     string
		workflow int
		jobHead  string
		total    int
		wantErr  bool
	}{
		{"historical-attempt", 5, head, 1, false},
		{"different-workflow", 6, head, 1, true},
		{"different-job-head", 5, strings.Repeat("b", 40), 1, true},
		{"truncated-jobs", 5, head, 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("accounting attempted mutation: %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/repos/o/r/actions/runs/10":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "workflow_id": test.workflow, "head_sha": head, "run_attempt": 2, "repository": map[string]any{"full_name": "o/r"}})
				case "/repos/o/r/actions/runs/10/attempts/1/jobs":
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": test.total, "jobs": []any{map[string]any{
						"id": 20, "run_id": 10, "head_sha": test.jobHead, "runner_id": 30, "name": "technical",
						"status": "completed", "conclusion": "cancelled", "started_at": "2026-10-31T04:00:00Z", "completed_at": "2026-10-31T04:01:00Z"}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			observed, err := client.ReadIntegrationExecutionJobs(context.Background(), "o/r", 5, 10, 1, time.Now())
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v observation=%+v", err, observed)
			}
			if !test.wantErr && (len(observed.Jobs) != 1 || observed.Jobs[0].Attempt != 1 || observed.Jobs[0].Conclusion != "cancelled" || observed.JobCompleteness != "complete") {
				t.Fatalf("lost actual old-attempt canceled cost: %+v", observed)
			}
			if observed.State != "unknown" || len(observed.Payload) != 0 || observed.Token != "" || !observed.Identity.VerifiedAt.IsZero() {
				t.Fatal("cost observation minted promotion proof")
			}
		})
	}
}

func TestIntegrationExecutionDiscoveryRequiresWholeHeadCohort(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, truncated := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("head_sha") != head || r.URL.Query().Get("per_page") != "100" {
				t.Errorf("not bounded exact-head discovery: %s", r.URL)
			}
			total := 1
			if truncated {
				total++
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "workflow_runs": []any{map[string]any{"id": 10, "head_sha": head}}})
		}))
		client := testClient(t, server.URL)
		runs, err := client.ReadIntegrationExecutionCandidateRuns(context.Background(), "o/r", 5, head)
		server.Close()
		if (err != nil) != truncated || !truncated && len(runs) != 1 {
			t.Fatalf("truncated=%v runs=%v err=%v", truncated, runs, err)
		}
		if _, err := client.ReadIntegrationExecutionCandidateRuns(context.Background(), "o/r", 0, head); err == nil {
			t.Fatal("missing workflow accepted")
		}
	}
}

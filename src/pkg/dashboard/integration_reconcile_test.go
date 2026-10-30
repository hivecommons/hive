package dashboard

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/integration"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestIntegrationReconcilesOldCompletedRunsAndEveryAttempt(t *testing.T) {
	for _, scenario := range []string{"complete", "overflow", "missing-attempt", "job-fanout", "unknown-base", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			head := strings.Repeat("a", 40)
			attemptReads := 0
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/repos/o/r/pulls":
					_, _ = w.Write([]byte("[]"))
				case r.URL.Path == "/repos/o/r/actions/runs":
					if r.URL.Query().Get("created") != "" {
						t.Error("creation window can omit old completed execution")
					}
					if scenario == "unavailable" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					if scenario == "overflow" {
						w.Header().Set("Link", `<http://example.invalid?page=2>; rel="next"`)
					}
					attempt, branch := 2, "v5"
					if scenario == "missing-attempt" {
						attempt = 0
					}
					if scenario == "unknown-base" {
						branch = "unidentified-feature"
					}
					_, _ = fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":10,"run_attempt":%d,"status":"completed","head_sha":%q,"head_branch":%q,"path":".github/workflows/test.yml","created_at":%q,"updated_at":%q}]}`, attempt, head, branch, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-20*time.Minute).Format(time.RFC3339))
				case strings.Contains(r.URL.Path, "/attempts/"):
					attemptReads++
					attempt, start, end, conclusion := 1, now.Add(-70*time.Minute), now.Add(-50*time.Minute), "cancelled"
					if strings.Contains(r.URL.Path, "/attempts/2/") {
						attempt, start, end, conclusion = 2, now.Add(-40*time.Minute), now.Add(-20*time.Minute), "success"
					}
					if scenario == "job-fanout" {
						w.Header().Set("Link", `<http://example.invalid?page=2>; rel="next"`)
					}
					_, _ = fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":%d,"run_id":10,"head_sha":%q,"runner_id":30,"status":"completed","conclusion":%q,"started_at":%q,"completed_at":%q}]}`, 100+attempt, head, conclusion, start.Format(time.RFC3339), end.Format(time.RFC3339))
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			server := orchestrationServer(t, config.IntegrationAdvisory)
			server.deps.GHClient = ghpkg.NewClient("token", "o", []string{"r"}, nil, api.URL)
			err := server.ReconcileIntegration(context.Background())
			if (err != nil) != (scenario == "unavailable") {
				t.Fatalf("%v", err)
			}
			store, storeErr := server.integrationStore()
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			snapshot := store.Snapshot(now)
			if len(snapshot.Coverage) != 1 || snapshot.Coverage[0].Complete != (scenario == "complete") {
				t.Fatalf("%+v", snapshot.Coverage)
			}
			if scenario == "complete" {
				cost := store.Cost("O/R", "v5", snapshot.Coverage[0].From, snapshot.Coverage[0].Through)
				if cost.RunnerMinutes == nil || math.Abs(*cost.RunnerMinutes-30) > 0.05 || cost.Jobs != 2 || attemptReads != 2 {
					t.Fatalf("cost=%+v reads=%d", cost, attemptReads)
				}
				if len(snapshot.Jobs) != 2 || snapshot.Jobs[0].Attempt == snapshot.Jobs[1].Attempt {
					t.Fatalf("%+v", snapshot.Jobs)
				}
			}
			reads := attemptReads
			if err := server.ReconcileIntegration(context.Background()); err != nil {
				t.Fatal(err)
			}
			if attemptReads != reads {
				t.Fatal("minimum batch interval ignored")
			}
		})
	}
}

func TestIntegrationObserverOffAndUnavailableRemainExplicit(t *testing.T) {
	server := orchestrationServer(t, config.IntegrationOff)
	if err := server.ReconcileIntegration(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := server.observeIntegrationWebhook("workflow_run", "unused", []byte("malformed"), time.Now()); err != nil {
		t.Fatal("OFF parsed payload", err)
	}
	if server.integration.store != nil {
		t.Fatal("OFF opened observation store")
	}
	server.deps.Config.IntegrationOrchestration.Mode = config.IntegrationEnforce
	if server.ReconcileIntegration(context.Background()) == nil {
		t.Fatal("missing telemetry client silently succeeded")
	}
	var nilServer *Server
	if err := nilServer.ReconcileIntegration(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationMergedWebhookHasSeparateLifecycleReceipt(t *testing.T) {
	server := orchestrationServer(t, config.IntegrationAdvisory)
	body := []byte(`{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"merged_at":"2026-10-30T20:46:06Z","merge_commit_sha":"` + strings.Repeat("a", 40) + `","head":{"sha":"` + strings.Repeat("b", 40) + `"}}}`)
	if err := server.observeIntegrationWebhook("pull_request", "merged", body, time.Now()); err != nil {
		t.Fatal(err)
	}
	store, err := server.integrationStore()
	if err != nil {
		t.Fatal(err)
	}
	if receipt, exists := store.Integrated("o/r", 7); !exists || receipt.Source != "github-signed-merged-webhook-v1" {
		t.Fatalf("%+v %v", receipt, exists)
	}
	if len(store.Snapshot(time.Now()).Evidence) != 0 {
		t.Fatal("merge receipt invented passing integration CI proof")
	}
	unknown := integration.Subject{Version: 1, Repo: "o/r", Authority: "run/1"}
	receipt := integration.Evidence{Subject: unknown, State: integration.Unknown, ObservedAt: time.Now(), Source: "github"}
	if err := store.Append(integration.Event{Version: 1, ID: "unknown", At: time.Now(), Evidence: &receipt}); err != nil {
		t.Fatal(err)
	}
}

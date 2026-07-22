package integrated

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

func TestRecoverWorkflowDispatchRetryIsPlannedAuditedAndSameCorrelation(t *testing.T) {
	stateDir, store, intent := ambiguousDispatchTestState(t)
	server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
	})
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	plan, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Planned || plan.Applied || plan.RequestDigest != intent.RequestDigest || plan.PlanDigest == "" ||
		plan.Actor != "operator" || !strings.Contains(plan.ApplyCommand, "--request-digest "+intent.RequestDigest) {
		t.Fatalf("recovery plan is not exactly bound: %+v", plan)
	}
	unchanged, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists || unchanged != intent {
		t.Fatalf("planning mutated dispatch state: exists=%t intent=%+v err=%v", exists, unchanged, err)
	}

	applied, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID,
		ExpectedRequestDigest: plan.RequestDigest, ExpectedPlanDigest: plan.PlanDigest, PlannedAt: plan.PlannedAt,
		Reason: "verified provider status and approved one same-correlation retry", GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Revoked || applied.CorrelationID != intent.CorrelationID {
		t.Fatalf("retry recovery was not applied: %+v", applied)
	}
	recovered, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists {
		t.Fatalf("load recovered intent: exists=%t err=%v", exists, err)
	}
	if recovered.CorrelationID != intent.CorrelationID || !recovered.DispatchAttemptedAt.IsZero() ||
		recovered.RecoveryRequestDigest != intent.RequestDigest || recovered.RecoveryPlanDigest != plan.PlanDigest ||
		recovered.RecoveryActor != "operator" || recovered.RecoveryCount != 1 || recovered.RecoveryOriginalAttemptedAt != intent.DispatchAttemptedAt {
		t.Fatalf("retry authorization lost exact bindings: %+v", recovered)
	}
	audit, err := os.ReadFile(filepath.Join(stateDir, "integrated", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{intent.CorrelationID, intent.RequestDigest, plan.PlanDigest, "actor=operator", "action=retry"} {
		if !strings.Contains(string(audit), exact) {
			t.Errorf("recovery audit is missing %q: %s", exact, audit)
		}
	}
}

func TestRecoverWorkflowDispatchRejectsExistingRunAndInconclusiveDiscovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		response   func(http.ResponseWriter, *http.Request, WorkflowDispatchIntent)
		want       string
		matchFound bool
	}{
		{
			name: "matching run",
			response: func(writer http.ResponseWriter, _ *http.Request, intent WorkflowDispatchIntent) {
				_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":77,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/77"}]}`, intent.ExpectedDisplayTitle)
			},
			want: "exact matching workflow run 77 exists", matchFound: true,
		},
		{
			name: "inconclusive discovery",
			response: func(writer http.ResponseWriter, _ *http.Request, _ WorkflowDispatchIntent) {
				http.Error(writer, "provider unavailable", http.StatusServiceUnavailable)
			},
			want: "discovery is inconclusive",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir, store, intent := ambiguousDispatchTestState(t)
			server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
				test.response(writer, request, intent)
			})
			defer server.Close()
			client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
			result, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
				StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) || result.Discovery.MatchFound != test.matchFound {
				t.Fatalf("unsafe recovery was not rejected: result=%+v err=%v", result, err)
			}
			unchanged, exists, loadErr := store.LoadWorkflowDispatchIntent()
			if loadErr != nil || !exists || unchanged != intent {
				t.Fatalf("rejected recovery mutated state: exists=%t intent=%+v err=%v", exists, unchanged, loadErr)
			}
		})
	}
}

func TestRecoverWorkflowDispatchRevokeRequiresFreshPlan(t *testing.T) {
	stateDir, store, intent := ambiguousDispatchTestState(t)
	server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
	})
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	plan, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRevoke, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRevoke, CorrelationID: intent.CorrelationID,
		ExpectedRequestDigest: plan.RequestDigest, ExpectedPlanDigest: plan.PlanDigest, PlannedAt: plan.PlannedAt,
		Reason: "provider cannot prove the old request; retire it and create a fresh correlation", GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.Revoked || !strings.Contains(result.NextCommand, "hive run") {
		t.Fatalf("revoke result is incomplete: %+v", result)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("revoked intent remains: exists=%t err=%v", exists, err)
	}
}

func TestRecoverWorkflowDispatchExhaustsPaginationBeforePlanning(t *testing.T) {
	stateDir, _, intent := ambiguousDispatchTestState(t)
	server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("page") == "2" {
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":79,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"queued","html_url":"https://example.test/runs/79"}]}`, intent.ExpectedDisplayTitle)
			return
		}
		writer.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, request.Host, request.URL.Path))
		_, _ = io.WriteString(writer, `{"total_count":1,"workflow_runs":[]}`)
	})
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	result, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRevoke, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
	})
	if err == nil || !strings.Contains(err.Error(), "exact matching workflow run 79 exists") {
		t.Fatalf("later-page match did not prevent recovery: result=%+v err=%v", result, err)
	}
	if result.Discovery.PagesScanned != 2 || result.Discovery.RunsScanned != 1 || !result.Discovery.MatchFound {
		t.Fatalf("discovery was not exhaustive: %+v", result.Discovery)
	}
}

func TestRecoveredRetryRechecksForDelayedOriginalBeforePosting(t *testing.T) {
	stateDir, store, intent := ambiguousDispatchTestState(t)
	var mu sync.Mutex
	exposeRun := false
	posts := 0
	server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.Method == http.MethodPost {
			posts++
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		if exposeRun {
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":88,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/88","head_sha":"late-head"}]}`, intent.ExpectedDisplayTitle)
			return
		}
		_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
	})
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	plan, err := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
		StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID,
		ExpectedRequestDigest: plan.RequestDigest, ExpectedPlanDigest: plan.PlanDigest, PlannedAt: plan.PlannedAt,
		Reason: "approved retry after exact discovery", GitHub: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	exposeRun = true
	mu.Unlock()
	selected, recovered, err := dispatchAndWaitAttempt(context.Background(), client, store, dispatchTestConfig(stateDir), "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	if selected.GetID() != 88 || recovered.RunID != 88 || recovered.CorrelationID != intent.CorrelationID || posts != 0 {
		t.Fatalf("delayed original was duplicated: selected=%+v intent=%+v posts=%d", selected, recovered, posts)
	}
}

func TestResumedAmbiguousDispatchReturnsImmediateRecoveryCommand(t *testing.T) {
	stateDir, store, intent := ambiguousDispatchTestState(t)
	server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
	})
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	started := time.Now()
	_, unchanged, err := dispatchAndWaitAttempt(context.Background(), client, store, dispatchTestConfig(stateDir), "owner", "repo", "hive-visual-hive.yml", "main")
	if err == nil || !strings.Contains(err.Error(), "hive recover-dispatch") || !strings.Contains(err.Error(), "--plan") {
		t.Fatalf("ambiguous resume did not return an actionable recovery error: %v", err)
	}
	if time.Since(started) > time.Second || unchanged != intent {
		t.Fatalf("ambiguous resume waited or mutated state: elapsed=%s intent=%+v", time.Since(started), unchanged)
	}
}

func TestWorkflowDispatchTransportFailureRecovery(t *testing.T) {
	for _, acceptedBeforeLoss := range []bool{false, true} {
		name := "before send"
		if acceptedBeforeLoss {
			name = "accepted response lost"
		}
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			config := dispatchTestConfig(stateDir)
			store, err := NewStore(filepath.Join(stateDir, "integrated"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(config); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			correlation := ""
			accepted := false
			posts := 0
			server := dispatchRecoveryTestServer(t, func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch request.Method {
				case http.MethodPost:
					posts++
					var body struct {
						Inputs map[string]string `json:"inputs"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					correlation = body.Inputs[workflowDispatchInput]
					accepted = true
					if acceptedBeforeLoss {
						connection, _, hijackErr := writer.(http.Hijacker).Hijack()
						if hijackErr != nil {
							t.Error(hijackErr)
							return
						}
						_ = connection.Close()
						return
					}
					writer.WriteHeader(http.StatusNoContent)
				case http.MethodGet:
					if accepted {
						_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":99,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/99","head_sha":"accepted"}]}`, workflowDispatchDisplayTitle(correlation))
					} else {
						_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
					}
				}
			})
			defer server.Close()
			clientURL := server.URL
			if !acceptedBeforeLoss {
				unavailable := httptest.NewServer(http.NotFoundHandler())
				clientURL = unavailable.URL
				unavailable.Close()
			}
			client := hivegithub.NewClientForTest(clientURL, "owner", []string{"repo"}, slog.Default())
			_, _, err = dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
			if err == nil || !strings.Contains(err.Error(), "dispatch Visual Hive production workflow") {
				t.Fatalf("transport ambiguity was not retained: %v", err)
			}
			intent, exists, loadErr := store.LoadWorkflowDispatchIntent()
			if loadErr != nil || !exists || !WorkflowDispatchNeedsRecovery(intent) || intent.RequestDigest == "" {
				t.Fatalf("ambiguous request checkpoint is incomplete: exists=%t intent=%+v err=%v", exists, intent, loadErr)
			}
			liveURL, parseErr := url.Parse(server.URL + "/")
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			client.GoGitHub().BaseURL = liveURL
			if acceptedBeforeLoss {
				_, planErr := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
					StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
				})
				if planErr == nil || !strings.Contains(planErr.Error(), "exact matching workflow run 99 exists") {
					t.Fatalf("accepted request was unsafe to recover: %v", planErr)
				}
				selected, bound, bindErr := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
				if bindErr != nil || selected.GetID() != 99 || bound.RunID != 99 || posts != 1 {
					t.Fatalf("accepted request was redispatched: selected=%+v intent=%+v posts=%d err=%v", selected, bound, posts, bindErr)
				}
				return
			}
			plan, planErr := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
				StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID, PlanOnly: true, GitHub: client,
			})
			if planErr != nil {
				t.Fatal(planErr)
			}
			_, applyErr := RecoverWorkflowDispatch(context.Background(), RecoverWorkflowDispatchOptions{
				StateDir: stateDir, Action: WorkflowDispatchRecoveryRetry, CorrelationID: intent.CorrelationID,
				ExpectedRequestDigest: plan.RequestDigest, ExpectedPlanDigest: plan.PlanDigest, PlannedAt: plan.PlannedAt,
				Reason: "transport proved no request bytes were sent", GitHub: client,
			})
			if applyErr != nil {
				t.Fatal(applyErr)
			}
			selected, bound, retryErr := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
			if retryErr != nil || selected.GetID() != 99 || bound.RunID != 99 || posts != 1 {
				t.Fatalf("authorized retry failed: selected=%+v intent=%+v posts=%d err=%v", selected, bound, posts, retryErr)
			}
		})
	}
}

func ambiguousDispatchTestState(t *testing.T) (string, *Store, WorkflowDispatchIntent) {
	t.Helper()
	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	intent, err := newWorkflowDispatchIntent(config, "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	intent.DispatchAttemptedAt = time.Now().UTC().Add(-time.Minute)
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		t.Fatal(err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists {
		t.Fatalf("load ambiguous dispatch: exists=%t err=%v", exists, err)
	}
	return stateDir, store, intent
}

func dispatchRecoveryTestServer(t *testing.T, workflowHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/owner/repo":
			_, _ = io.WriteString(writer, `{"id":123,"full_name":"owner/repo"}`)
		case "/user":
			_, _ = io.WriteString(writer, `{"login":"operator","id":9002,"type":"User"}`)
		case "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs",
			"/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			workflowHandler(writer, request)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
}

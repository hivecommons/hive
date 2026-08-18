package integrated

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestDispatchConsumesOnlyExactCorrelationAndArtifacts(t *testing.T) {
	var correlation string
	dispatches := 0
	exactHead := strings.Repeat("e", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Ref    string         `json:"ref"`
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			if body.Ref != "main" || !workflowDispatchCorrelationPattern.MatchString(correlation) {
				t.Errorf("dispatch was not exactly correlated: ref=%q correlation=%q", body.Ref, correlation)
			}
			dispatches++
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			manual := `{"id":22,"display_title":"manual production run","event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/22","head_sha":"manual"}`
			exact := fmt.Sprintf(`{"id":21,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/21","head_sha":%q}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), exactHead)
			_, _ = fmt.Fprintf(writer, `{"total_count":2,"workflow_runs":[%s,%s]}`, manual, exact)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/21":
			_, _ = fmt.Fprintf(writer, `{"id":21,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"success","repository":{"id":123,"full_name":"owner/repo"}}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), exactHead)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/12":
			_, _ = fmt.Fprintf(writer, `{"id":12,"name":%q,"path":%q,"state":"active"}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/21/jobs":
			_, _ = fmt.Fprintf(writer, `{"total_count":3,"jobs":[{"id":700,"run_id":21,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-production","workflow_name":%q},{"id":701,"run_id":21,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive","workflow_name":%q},{"id":702,"run_id":21,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-execution","workflow_name":%q}]}`, exactHead, visualHiveProductionWorkflowName, exactHead, visualHiveProductionWorkflowName, exactHead, visualHiveProductionWorkflowName)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/21/artifacts":
			_, _ = io.WriteString(writer, `{"total_count":4,"artifacts":[{"id":901,"name":"visual-hive-evidence-decoy"},{"id":902,"name":"visual-hive-bundle-decoy"},{"id":101,"name":"visual-hive-evidence-21"},{"id":102,"name":"visual-hive-bundle-21"}]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	workflow, err := dispatchAndWait(context.Background(), client, config)
	if err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 || workflow.RunID != 21 || workflow.HeadSHA != exactHead || workflow.CorrelationID != correlation || workflow.RepositoryTestOverall != 0 || len(workflow.RepositoryTestDigest) != 64 {
		t.Fatalf("wrong workflow consumed: dispatches=%d workflow=%+v", dispatches, workflow)
	}
	if workflow.EvidenceArtifact != 101 || workflow.BundleArtifact != 102 {
		t.Fatalf("artifact selection was not exact-run-bound: %+v", workflow)
	}
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists || intent.RunID != workflow.RunID || intent.CorrelationID != workflow.CorrelationID {
		t.Fatalf("exact dispatch binding was not durable: exists=%t intent=%+v err=%v", exists, intent, err)
	}
	if err := consumeWorkflowDispatch(stateDir, workflow); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("consumed dispatch intent remains: exists=%t err=%v", exists, err)
	}
}

func TestDispatchAcceptsOnlyRepositoryTestCausedWorkflowFailure(t *testing.T) {
	var correlation string
	head := strings.Repeat("f", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":41,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://example.test/runs/41"}]}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/41":
			_, _ = fmt.Fprintf(writer, `{"id":41,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","repository":{"id":123,"full_name":"owner/repo"}}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/12":
			_, _ = fmt.Fprintf(writer, `{"id":12,"name":%q,"path":%q,"state":"active"}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/41/jobs":
			_, _ = fmt.Fprintf(writer, `{"total_count":4,"jobs":[{"id":700,"run_id":41,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-production","workflow_name":%q},{"id":701,"run_id":41,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive","workflow_name":%q},{"id":702,"run_id":41,"head_sha":%q,"status":"completed","conclusion":"failure","name":"Hive repository test 001","workflow_name":%q,"steps":[{"name":"Set up job","status":"completed","conclusion":"success","number":1},{"name":"Execute exact repository test command","status":"completed","conclusion":"failure","number":2}]},{"id":703,"run_id":41,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-execution","workflow_name":%q}]}`, head, visualHiveProductionWorkflowName, head, visualHiveProductionWorkflowName, head, visualHiveProductionWorkflowName, head, visualHiveProductionWorkflowName)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/41/artifacts":
			_, _ = io.WriteString(writer, `{"total_count":2,"artifacts":[{"id":101,"name":"visual-hive-evidence-41"},{"id":102,"name":"visual-hive-bundle-41"}]}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	config := dispatchTestConfig(t.TempDir())
	config.TestCommands = [][]string{{"npm", "test"}}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	workflow, err := dispatchAndWait(context.Background(), client, config)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Conclusion != "failure" || workflow.RepositoryTestOverall != 1 || len(workflow.RepositoryTests) != 1 || workflow.RepositoryTests[0].ExitCode != 1 || len(workflow.RepositoryTestDigest) != 64 {
		t.Fatalf("repository-test-caused workflow failure was not preserved as trusted lifecycle evidence: %+v", workflow)
	}
}

func TestDispatchRetiresExactFailedProducerRunBeforeRetry(t *testing.T) {
	var correlation string
	head := strings.Repeat("d", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":61,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://example.test/runs/61"}]}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/61":
			_, _ = fmt.Fprintf(writer, `{"id":61,"workflow_id":12,"name":%q,"path":%q,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","head_sha":%q,"status":"completed","conclusion":"failure","repository":{"id":123,"full_name":"owner/repo"}}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath, workflowDispatchDisplayTitle(correlation), head)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/12":
			_, _ = fmt.Fprintf(writer, `{"id":12,"name":%q,"path":%q,"state":"active"}`, visualHiveProductionWorkflowName, visualHiveProductionWorkflowPath)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/61/jobs":
			_, _ = fmt.Fprintf(writer, `{"total_count":3,"jobs":[{"id":700,"run_id":61,"head_sha":%q,"status":"completed","conclusion":"failure","name":"visual-hive-production","workflow_name":%q},{"id":701,"run_id":61,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive","workflow_name":%q},{"id":702,"run_id":61,"head_sha":%q,"status":"completed","conclusion":"success","name":"visual-hive-execution","workflow_name":%q}]}`, head, visualHiveProductionWorkflowName, head, visualHiveProductionWorkflowName, head, visualHiveProductionWorkflowName)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if _, err := dispatchAndWait(context.Background(), client, config); err == nil || !strings.Contains(err.Error(), "visual-hive-production") {
		t.Fatalf("failed producer run was accepted: %v", err)
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LoadWorkflowDispatchIntent(); err != nil || exists {
		t.Fatalf("exact failed producer dispatch remained replayable: exists=%t err=%v", exists, err)
	}
}

func TestDispatchRecoveryReusesCorrelationWithoutRedispatch(t *testing.T) {
	var mu sync.Mutex
	correlation := ""
	dispatches := 0
	exposeRun := false
	listObserved := make(chan struct{})
	var listOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			dispatches++
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			if !exposeRun {
				_, _ = io.WriteString(writer, `{"total_count":0,"workflow_runs":[]}`)
				listOnce.Do(func() { close(listObserved) })
				return
			}
			_, _ = fmt.Fprintf(writer, `{"total_count":1,"workflow_runs":[{"id":31,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/31","head_sha":"head"}]}`, workflowDispatchDisplayTitle(correlation))
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		_, _, dispatchErr := dispatchAndWaitAttempt(firstCtx, client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
		firstDone <- dispatchErr
	}()
	select {
	case <-listObserved:
	case firstErr := <-firstDone:
		t.Fatalf("dispatch returned before its acknowledged run search: %v", firstErr)
	case <-time.After(5 * time.Second):
		t.Fatal("acknowledged dispatch did not begin its exact run search")
	}
	cancel()
	var firstErr error
	select {
	case firstErr = <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled dispatch did not stop its exact run search")
	}
	if firstErr == nil || !errors.Is(firstErr, context.Canceled) || !strings.Contains(firstErr.Error(), "exactly correlated") {
		t.Fatalf("unobserved acknowledged dispatch should retain a recoverable exact-correlation error, got %v", firstErr)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	mu.Lock()
	dispatchedCorrelation := correlation
	mu.Unlock()
	if err != nil || !exists || intent.CorrelationID != dispatchedCorrelation || intent.RunID != 0 || intent.DispatchAcknowledgedAt.IsZero() {
		t.Fatalf("unobserved dispatch was not durably recoverable: exists=%t intent=%+v err=%v", exists, intent, err)
	}

	mu.Lock()
	exposeRun = true
	mu.Unlock()
	selected, recovered, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	dispatchCount, recoveredCorrelation := dispatches, correlation
	mu.Unlock()
	if dispatchCount != 1 || selected.GetID() != 31 || recovered.CorrelationID != recoveredCorrelation || recovered.RunID != 31 {
		t.Fatalf("recovery redispatched or selected the wrong run: dispatches=%d selected=%+v intent=%+v", dispatchCount, selected, recovered)
	}
}

func TestDispatchUsesReturnedRunIDWithoutRecencySearch(t *testing.T) {
	var correlation string
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches":
			if version := request.Header.Get("X-GitHub-Api-Version"); version != "2022-11-28" {
				t.Errorf("self-hosted test endpoint API version = %q", version)
			}
			var body struct {
				Inputs map[string]any `json:"inputs"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			correlation, _ = body.Inputs[workflowDispatchInput].(string)
			_, _ = io.WriteString(writer, `{"workflow_run_id":51,"run_url":"https://api.example.test/runs/51","html_url":"https://example.test/runs/51"}`)
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/51":
			_, _ = fmt.Fprintf(writer, `{"id":51,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/51","head_sha":"exact"}`, workflowDispatchDisplayTitle(correlation))
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/runs":
			listCalls++
			http.Error(writer, "run search must not be used when dispatch returns an exact ID", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()

	stateDir := t.TempDir()
	config := dispatchTestConfig(stateDir)
	store, err := NewStore(stateDir + "/integrated")
	if err != nil {
		t.Fatal(err)
	}
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	selected, intent, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 0 || selected.GetID() != 51 || intent.RunID != 51 || intent.CorrelationID != correlation {
		t.Fatalf("dispatch response was not bound directly: listCalls=%d selected=%+v intent=%+v", listCalls, selected, intent)
	}
}

func TestWorkflowDispatchUsesOptionalAppWhileReadsStayOnCoreApp(t *testing.T) {
	correlation := ""
	visualPosts := 0
	coreReads := 0
	visualServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodPost || request.URL.Path != "/repos/owner/repo/actions/workflows/hive-visual-hive.yml/dispatches" {
			http.Error(writer, "optional App received non-dispatch request", http.StatusForbidden)
			return
		}
		var body struct {
			Inputs map[string]any `json:"inputs"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		correlation, _ = body.Inputs[workflowDispatchInput].(string)
		visualPosts++
		_, _ = io.WriteString(writer, `{"workflow_run_id":73,"html_url":"https://example.test/runs/73"}`)
	}))
	t.Cleanup(visualServer.Close)
	coreServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			t.Errorf("core Hive App received Visual Hive dispatch mutation: %s", request.URL.Path)
			http.Error(writer, "wrong writer", http.StatusForbidden)
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/actions/runs/73" {
			coreReads++
			_, _ = fmt.Fprintf(writer, `{"id":73,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success","html_url":"https://example.test/runs/73","head_sha":"exact"}`, workflowDispatchDisplayTitle(correlation))
			return
		}
		http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(coreServer.Close)

	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	core := hivegithub.NewClientForTest(coreServer.URL, "owner", []string{"repo"}, slog.Default())
	visual := hivegithub.NewClientForTest(visualServer.URL, "owner", []string{"repo"}, slog.Default())
	ctx := WithVisualHiveGitHubClients(context.Background(), visual, visual)
	config := dispatchTestConfig(stateDir)
	config.SetupAuthorizationAppID = 1
	config.VisualHiveGitHubAppID = 2
	selected, intent, err := dispatchAndWaitAttempt(ctx, core, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	if visualPosts != 1 || coreReads != 1 || selected.GetID() != 73 || intent.RunID != 73 {
		t.Fatalf("split GitHub capability routing failed: visual_posts=%d core_reads=%d selected=%+v intent=%+v", visualPosts, coreReads, selected, intent)
	}
}

func TestWorkflowDispatchFailsBeforeMutationWithoutDedicatedApp(t *testing.T) {
	stateDir := t.TempDir()
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	config := dispatchTestConfig(stateDir)
	config.SetupAuthorizationAppID = 1
	config.VisualHiveGitHubAppID = 2
	client := hivegithub.NewClientForTest("https://example.invalid", "owner", []string{"repo"}, slog.Default())
	_, intent, err := dispatchAndWaitAttempt(context.Background(), client, store, config, "owner", "repo", "hive-visual-hive.yml", "main")
	if err == nil || !strings.Contains(err.Error(), "dedicated Visual Hive workflow App runtime is unavailable") {
		t.Fatalf("missing optional App did not fail closed: %v", err)
	}
	if !intent.DispatchAttemptedAt.IsZero() || intent.RequestDigest != "" {
		t.Fatalf("missing optional App was recorded as an ambiguous mutation: %+v", intent)
	}
	persisted, exists, loadErr := store.LoadWorkflowDispatchIntent()
	if loadErr != nil || !exists || !persisted.DispatchAttemptedAt.IsZero() || persisted.RequestDigest != "" {
		t.Fatalf("precondition failure contaminated dispatch recovery state: exists=%t intent=%+v err=%v", exists, persisted, loadErr)
	}
}

func TestExactWorkflowRunBindingStabilizesTransientDisplayTitle(t *testing.T) {
	intent := WorkflowDispatchIntent{
		RunID:                51,
		Ref:                  "main",
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(strings.Repeat("a", 64)),
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet || request.URL.Path != "/repos/owner/repo/actions/runs/51" {
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
			return
		}
		readCount := reads.Add(1)
		displayTitle := visualHiveProductionWorkflowName
		status := "queued"
		conclusion := ""
		if readCount >= 3 {
			displayTitle = intent.ExpectedDisplayTitle
			status = "completed"
			conclusion = "success"
		}
		_, _ = fmt.Fprintf(writer, `{"id":51,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":%q,"conclusion":%q}`, displayTitle, status, conclusion)
	}))
	defer server.Close()

	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	selected, err := waitForExactWorkflowRunWithTiming(context.Background(), client, "owner", "repo", intent, nil, 10*time.Millisecond, time.Millisecond, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if readCount := reads.Load(); readCount != 3 || selected.GetID() != intent.RunID || selected.GetDisplayTitle() != intent.ExpectedDisplayTitle {
		t.Fatalf("transient binding did not stabilize on the exact run: reads=%d selected=%+v", readCount, selected)
	}
}

func TestExactWorkflowRunBindingStaticWorkflowTitleTimesOutFailClosed(t *testing.T) {
	intent := WorkflowDispatchIntent{
		RunID:                62,
		Ref:                  "main",
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(strings.Repeat("d", 64)),
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		reads.Add(1)
		_, _ = fmt.Fprintf(writer, `{"id":62,"display_title":%q,"event":"workflow_dispatch","head_branch":"main","status":"completed","conclusion":"success"}`, visualHiveProductionWorkflowName)
	}))
	defer server.Close()

	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := waitForExactWorkflowRunWithTiming(context.Background(), client, "owner", "repo", intent, nil, 10*time.Millisecond, 2*time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not stabilize") || !strings.Contains(err.Error(), "display_title") || !strings.Contains(err.Error(), "remains bound to run 62") {
		t.Fatalf("persistent static workflow title did not fail closed after bounded stabilization: %v", err)
	}
	if readCount := reads.Load(); readCount < 2 {
		t.Fatalf("static workflow title was not retried before timeout: reads=%d", readCount)
	}
}

func TestExactWorkflowRunBindingRejectsPermanentMismatch(t *testing.T) {
	intent := WorkflowDispatchIntent{
		RunID:                51,
		Ref:                  "main",
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(strings.Repeat("b", 64)),
	}
	exact := func() *gh.WorkflowRun {
		return &gh.WorkflowRun{
			ID:           gh.Ptr(intent.RunID),
			DisplayTitle: gh.Ptr(intent.ExpectedDisplayTitle),
			Event:        gh.Ptr("workflow_dispatch"),
			HeadBranch:   gh.Ptr(intent.Ref),
			Status:       gh.Ptr("completed"),
			Conclusion:   gh.Ptr("success"),
		}
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reads.Add(1)
		http.Error(writer, "permanent mismatch must not be retried", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	for _, test := range []struct {
		name   string
		mutate func(*gh.WorkflowRun)
		want   string
	}{
		{name: "id", mutate: func(run *gh.WorkflowRun) { run.ID = gh.Ptr(int64(52)) }, want: "persisted run id"},
		{name: "display title", mutate: func(run *gh.WorkflowRun) { run.DisplayTitle = gh.Ptr("different correlation") }, want: "display_title"},
		{name: "event", mutate: func(run *gh.WorkflowRun) { run.Event = gh.Ptr("push") }, want: "event"},
		{name: "ref", mutate: func(run *gh.WorkflowRun) { run.HeadBranch = gh.Ptr("other") }, want: "head branch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads.Store(0)
			run := exact()
			test.mutate(run)
			_, err := waitForExactWorkflowRunWithTiming(context.Background(), client, "owner", "repo", intent, run, 10*time.Millisecond, time.Millisecond, 100*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "violates its exact durable dispatch binding") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("permanent mismatch was not rejected: %v", err)
			}
			if readCount := reads.Load(); readCount != 0 {
				t.Fatalf("permanent mismatch triggered %d retry reads", readCount)
			}
		})
	}
}

func TestExactWorkflowRunBindingStabilizationTimeoutIsActionable(t *testing.T) {
	intent := WorkflowDispatchIntent{
		RunID:                61,
		Ref:                  "main",
		ExpectedDisplayTitle: workflowDispatchDisplayTitle(strings.Repeat("c", 64)),
	}
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		reads.Add(1)
		_, _ = io.WriteString(writer, `{"id":61,"display_title":"","event":"workflow_dispatch","head_branch":"main","status":"queued"}`)
	}))
	defer server.Close()

	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := waitForExactWorkflowRunWithTiming(context.Background(), client, "owner", "repo", intent, nil, 10*time.Millisecond, 2*time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not stabilize") || !strings.Contains(err.Error(), "display_title") || !strings.Contains(err.Error(), "remains bound to run 61") || !strings.Contains(err.Error(), "will not select a different run") {
		t.Fatalf("persistent incomplete binding did not return an actionable fail-closed timeout: %v", err)
	}
	if readCount := reads.Load(); readCount < 2 {
		t.Fatalf("incomplete binding was not retried before timeout: reads=%d", readCount)
	}
}

func TestWorkflowDispatchAPIVersionSelection(t *testing.T) {
	for _, test := range []struct {
		host string
		want bool
	}{
		{host: "api.github.com", want: true},
		{host: "API.GITHUB.COM", want: true},
		{host: "api.enterprise.ghe.com", want: true},
		{host: "github.example.test", want: false},
		{host: "127.0.0.1", want: false},
	} {
		if got := usesCurrentWorkflowDispatchAPI(test.host); got != test.want {
			t.Errorf("usesCurrentWorkflowDispatchAPI(%q) = %t, want %t", test.host, got, test.want)
		}
	}
}

func TestDuplicateExactWorkflowCorrelationFailsClosed(t *testing.T) {
	config := dispatchTestConfig(t.TempDir())
	intent, err := newWorkflowDispatchIntent(config, "hive-visual-hive.yml", "main")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"total_count":2,"workflow_runs":[{"id":41,"display_title":%q,"event":"workflow_dispatch","head_branch":"main"},{"id":42,"display_title":%q,"event":"workflow_dispatch","head_branch":"main"}]}`, intent.ExpectedDisplayTitle, intent.ExpectedDisplayTitle)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	if selected, err := findCorrelatedWorkflowRun(context.Background(), client, "owner", "repo", "hive-visual-hive.yml", "main", intent); err == nil || selected != nil || !strings.Contains(err.Error(), "multiple run IDs") {
		t.Fatalf("duplicate exact correlation was not rejected: selected=%+v err=%v", selected, err)
	}
}

func TestProductionWorkflowRequiresExactDispatchCorrelation(t *testing.T) {
	value := workflow(Config{VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40), ACMMLevel: 4})
	for _, required := range []string{
		"hive_dispatch_id:",
		"hive_operation:",
		"setup-baseline-capture",
		"required: true",
		"type: string",
		`run-name: "Hive Visual Hive Production [${{ inputs.hive_dispatch_id }}]"`,
		"  visual-hive-production:",
		"  visual-hive:",
		`test "$HIVE_EVENT_NAME" = "workflow_dispatch"`,
		`test "$HIVE_REF" = "refs/heads/$HIVE_DEFAULT_BRANCH"`,
		`ref: ${{ github.event.repository.default_branch }}`,
		`test "$(git rev-parse HEAD)" = "$HIVE_DISPATCH_SHA"`,
	} {
		if !strings.Contains(value, required) {
			t.Fatalf("production workflow missing exact dispatch correlation %q:\n%s", required, value)
		}
	}
	if strings.Count(value, "  visual-hive:\n") != 1 {
		t.Fatalf("production workflow must contain exactly one guarded PR-context seed:\n%s", value)
	}
	seed := value[strings.Index(value, "  # GitHub allows an App-bound context"):]
	if !strings.Contains(seed, "\n    if: ${{ inputs.hive_operation == 'production' }}") || strings.Contains(seed, "\n      if:") {
		t.Fatalf("activation seed must run actively in production and be excluded only from the dedicated capture lane:\n%s", seed)
	}
}

func TestPullRequestWorkflowHasNoManualDispatchAndOwnsProtectedContext(t *testing.T) {
	value := pullRequestWorkflow(Config{VisualHiveRepo: "owner/visual-hive", VisualHiveRef: strings.Repeat("a", 40)})
	if strings.Contains(value, "workflow_dispatch") || !strings.Contains(value, "on:\n  pull_request:") || !strings.Contains(value, "  pull_request_target:\n") || !strings.Contains(value, "  visual-hive:\n") {
		t.Fatalf("PR workflow triggers or check context are unsafe:\n%s", value)
	}
	if !strings.Contains(value, "github.event_name == 'pull_request'") || !strings.Contains(value, "operation == 'uninstall'") {
		t.Fatalf("PR target jobs and protected aggregator are not explicitly event-gated:\n%s", value)
	}
	for _, forbidden := range []string{"github.ref }}", "origin/$HIVE_DEFAULT_BRANCH", "visual-hive-production:"} {
		if strings.Contains(value, forbidden) {
			t.Fatalf("PR workflow retains manual/production fallback %q:\n%s", forbidden, value)
		}
	}
}

func dispatchTestConfig(stateDir string) Config {
	return Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", StateDir: stateDir,
	}
}

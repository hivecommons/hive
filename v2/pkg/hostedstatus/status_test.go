package hostedstatus

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const (
	testHeadSHA  = "1111111111111111111111111111111111111111"
	testStateSHA = "2222222222222222222222222222222222222222"
)

type serverScenario struct {
	workflow          string
	workflowStatus    int
	stateStatus       int
	secretStatus      int
	active            map[string][]map[string]any
	completed         []map[string]any
	cycleJobs         map[int64]bool
	executedJobs      map[int64]string
	receiptOperation  string
	receiptPaused     bool
	receiptStateAfter string
	receiptRelease    integrated.HostedReleaseIdentity
	receiptOverride   []byte
	artifactRunID     int64
	artifactHead      string
	noArtifactRuns    map[int64]bool
	defaultHead       string
}

func TestInspectGreen(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)
	scenario := greenScenario(t, config, now)
	result := inspectScenario(t, config, now, scenario)
	if !result.ProductionReady {
		t.Fatalf("expected production ready, checks: %#v", result.Checks)
	}
	if result.LatestCompletedCycle == nil || result.LatestCompletedCycle.ID != 101 {
		t.Fatalf("unexpected latest cycle: %#v", result.LatestCompletedCycle)
	}
	if result.StateBranchHeadSHA != testStateSHA || result.ManagedWorkflowSHA256 == "" || result.FreshnessWindowSeconds != int64((45*time.Minute)/time.Second) {
		t.Fatalf("missing exact hosted evidence: %#v", result)
	}
	for _, name := range []string{CheckManagedWorkflow, CheckStateBranch, CheckStateSecret, CheckControllerIdentities, CheckControllerOverlap, CheckLatestCycleSuccess, CheckLatestCycleFresh, CheckLatestCycleHead, CheckRemotePauseState} {
		if !findCheck(t, result, name).Passed {
			t.Fatalf("expected %s to pass: %#v", name, findCheck(t, result, name))
		}
	}
}

func TestInspectUsesExactRemotePauseAndResumeReceipts(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)

	pause := greenScenario(t, config, now)
	pause.completed = []map[string]any{
		testRun(102, "workflow_dispatch", "completed", "success", now.Add(-2*time.Minute)),
		testRun(101, "schedule", "completed", "success", now.Add(-10*time.Minute)),
	}
	pause.executedJobs[102] = "control"
	pause.receiptOperation, pause.receiptPaused = "pause", true
	paused := inspectScenario(t, config, now, pause)
	if !paused.Paused || paused.ProductionReady || paused.LatestStateReceipt == nil || !paused.LatestStateReceipt.Paused {
		t.Fatalf("pause receipt did not fail readiness authoritatively: %#v", paused)
	}
	if findCheck(t, paused, CheckRemotePauseState).Passed {
		t.Fatal("paused remote state must not be production ready")
	}

	resume := greenScenario(t, config, now)
	resume.completed = []map[string]any{
		testRun(103, "workflow_dispatch", "completed", "success", now.Add(-time.Minute)),
		testRun(101, "schedule", "completed", "success", now.Add(-10*time.Minute)),
	}
	resume.executedJobs[103] = "control"
	resume.receiptOperation = "resume"
	resumed := inspectScenario(t, config, now, resume)
	if resumed.Paused || !resumed.ProductionReady || resumed.LatestStateReceipt == nil || resumed.LatestStateReceipt.Operation != "resume" {
		t.Fatalf("resume receipt did not restore readiness: %#v", resumed)
	}
}

func TestInspectRejectsMalformedOldAndWrongRunPauseReceipts(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)
	tests := []struct {
		name   string
		mutate func(*serverScenario)
	}{
		{name: "malformed", mutate: func(s *serverScenario) { s.receiptOverride = []byte(`{"schema_version":`) }},
		{name: "old state", mutate: func(s *serverScenario) { s.receiptStateAfter = strings.Repeat("8", 40) }},
		{name: "wrong run", mutate: func(s *serverScenario) { s.artifactRunID = 999 }},
		{name: "wrong head", mutate: func(s *serverScenario) { s.artifactHead = strings.Repeat("9", 40) }},
		{name: "wrong release", mutate: func(s *serverScenario) { s.receiptRelease.DistributionManifestSHA256 = strings.Repeat("9", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := greenScenario(t, config, now)
			test.mutate(&scenario)
			result := inspectScenario(t, config, now, scenario)
			if result.ProductionReady || !result.Paused || findCheck(t, result, CheckRemotePauseState).Passed {
				t.Fatalf("untrusted receipt was accepted: %#v", result)
			}
		})
	}
}

func TestInspectManualCycleUsesExecutedCycleJob(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)
	scenario := greenScenario(t, config, now)
	scenario.completed = []map[string]any{
		testRun(103, "workflow_dispatch", "completed", "success", now.Add(-2*time.Minute)),
		testRun(102, "workflow_dispatch", "completed", "failure", now.Add(-3*time.Minute)),
		testRun(101, "schedule", "completed", "success", now.Add(-10*time.Minute)),
	}
	scenario.cycleJobs = map[int64]bool{103: false, 102: true}
	result := inspectScenario(t, config, now, scenario)
	if result.LatestCompletedCycle == nil || result.LatestCompletedCycle.ID != 102 {
		t.Fatalf("expected newest dispatch with an executed cycle job, got %#v", result.LatestCompletedCycle)
	}
	if findCheck(t, result, CheckLatestCycleSuccess).Passed {
		t.Fatalf("failed manual cycle must make readiness fail")
	}
}

func TestInspectReadinessFailures(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)
	tests := []struct {
		name      string
		mutate    func(*serverScenario)
		wantCheck string
	}{
		{name: "stale cycle", wantCheck: CheckLatestCycleFresh, mutate: func(s *serverScenario) {
			s.completed = []map[string]any{testRun(101, "schedule", "completed", "success", now.Add(-2*time.Hour))}
		}},
		{name: "failed cycle", wantCheck: CheckLatestCycleSuccess, mutate: func(s *serverScenario) {
			s.completed = []map[string]any{testRun(101, "schedule", "completed", "failure", now.Add(-10*time.Minute))}
		}},
		{name: "stale default head", wantCheck: CheckLatestCycleHead, mutate: func(s *serverScenario) { s.defaultHead = strings.Repeat("9", 40) }},
		{name: "malformed run identity", wantCheck: CheckControllerIdentities, mutate: func(s *serverScenario) {
			run := testRun(101, "schedule", "completed", "success", now.Add(-10*time.Minute))
			run["head_sha"] = "not-a-commit"
			s.completed = []map[string]any{run}
		}},
		{name: "overlapping runs", wantCheck: CheckControllerOverlap, mutate: func(s *serverScenario) {
			s.active["in_progress"] = []map[string]any{
				testRun(201, "schedule", "in_progress", "", now.Add(-time.Minute)),
				testRun(202, "workflow_dispatch", "in_progress", "", now.Add(-time.Minute)),
			}
		}},
		{name: "tampered workflow", wantCheck: CheckManagedWorkflow, mutate: func(s *serverScenario) { s.workflow += "\n# tampered\n" }},
		{name: "missing state", wantCheck: CheckStateBranch, mutate: func(s *serverScenario) { s.stateStatus = http.StatusNotFound }},
		{name: "missing secret", wantCheck: CheckStateSecret, mutate: func(s *serverScenario) { s.secretStatus = http.StatusNotFound }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scenario := greenScenario(t, config, now)
			test.mutate(&scenario)
			result := inspectScenario(t, config, now, scenario)
			if result.ProductionReady {
				t.Fatalf("expected readiness failure")
			}
			if findCheck(t, result, test.wantCheck).Passed {
				t.Fatalf("expected %s failure, checks: %#v", test.wantCheck, result.Checks)
			}
		})
	}
}

func TestInspectRejectsInvalidInputsAndExternalFailure(t *testing.T) {
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	config := testConfig(t)
	client := gh.NewClient(nil)
	if _, err := Inspect(context.Background(), nil, config, now); err == nil {
		t.Fatal("expected nil-client error")
	}
	local := config
	local.ExecutionMode = integrated.ExecutionLocal
	if _, err := Inspect(context.Background(), client, local, now); err == nil {
		t.Fatal("expected local-mode error")
	}
	badInterval := config
	badInterval.RunIntervalSeconds = 0
	if _, err := Inspect(context.Background(), client, badInterval, now); err == nil {
		t.Fatal("expected interval error")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	api := testClient(t, server.URL)
	if _, err := Inspect(context.Background(), api, config, now); err == nil || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("expected transport/API workflow error, got %v", err)
	}
}

func greenScenario(t *testing.T, config integrated.Config, now time.Time) serverScenario {
	t.Helper()
	workflow, err := integrated.GenerateHostedControllerWorkflow(integrated.HostedWorkflowConfig{
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		ScheduleCron: config.HostedSchedule, StateBranch: config.HostedStateBranch, StateKeySecret: stateSecretName,
		ReleaseRepository: config.HiveReleaseRepository, ReleaseTag: config.HiveReleaseVersion,
		HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
		DistributionManifestSHA256: config.DistributionManifestSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	return serverScenario{
		workflow: workflow, workflowStatus: http.StatusOK, stateStatus: http.StatusOK, secretStatus: http.StatusOK,
		active:           map[string][]map[string]any{},
		completed:        []map[string]any{testRun(101, "schedule", "completed", "success", now.Add(-10*time.Minute))},
		cycleJobs:        map[int64]bool{},
		executedJobs:     map[int64]string{},
		receiptOperation: "cycle",
		receiptRelease:   testHostedRelease(config),
		defaultHead:      testHeadSHA,
	}
}

func inspectScenario(t *testing.T, config integrated.Config, now time.Time, scenario serverScenario) Result {
	t.Helper()
	server := httptest.NewServer(scenario.handler(t, config, now))
	defer server.Close()
	result, err := Inspect(context.Background(), testClient(t, server.URL), config, now)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return result
}

func (scenario serverScenario) handler(t *testing.T, config integrated.Config, now time.Time) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/contents/"):
			if request.URL.Query().Get("ref") != config.DefaultBranch {
				t.Errorf("workflow ref = %q", request.URL.Query().Get("ref"))
			}
			if scenario.workflowStatus != http.StatusOK {
				writeAPIError(w, scenario.workflowStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "file", "path": integrated.HostedControllerWorkflowPath, "sha": testHeadSHA,
				"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(scenario.workflow)),
			})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/branches/"+config.DefaultBranch):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": config.DefaultBranch, "commit": map[string]any{"sha": scenario.defaultHead},
			})
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/git/ref/"):
			if scenario.stateStatus != http.StatusOK {
				writeAPIError(w, scenario.stateStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ref":    "refs/heads/" + config.HostedStateBranch,
				"object": map[string]any{"type": "commit", "sha": testStateSHA},
			})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/actions/secrets/"+stateSecretName):
			if scenario.secretStatus != http.StatusOK {
				writeAPIError(w, scenario.secretStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": stateSecretName, "created_at": now.Add(-time.Hour).Format(time.RFC3339), "updated_at": now.Add(-time.Hour).Format(time.RFC3339),
			})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/jobs"):
			var runID int64
			_, _ = fmt.Sscanf(request.URL.Path, "/repos/acme/widget/actions/runs/%d/jobs", &runID)
			executed := scenario.executedJobs[runID]
			if executed == "" && scenario.cycleJobs[runID] {
				executed = "cycle"
			}
			if executed == "" && scenario.runEvent(runID) == "schedule" {
				executed = "cycle"
			}
			jobs := make([]map[string]any, 0, 3)
			for index, name := range []string{"control", "status", "cycle"} {
				conclusion := "skipped"
				if name == executed {
					conclusion = "success"
					if scenario.cycleJobs[runID] {
						conclusion = "failure"
					}
				}
				jobs = append(jobs, map[string]any{"id": runID*10 + int64(index+1), "name": name, "status": "completed", "conclusion": conclusion})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(jobs), "jobs": jobs})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/artifacts"):
			var runID int64
			_, _ = fmt.Sscanf(request.URL.Path, "/repos/acme/widget/actions/runs/%d/artifacts", &runID)
			if scenario.noArtifactRuns[runID] {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "artifacts": []any{}})
				return
			}
			job := scenario.executedJobs[runID]
			if job == "" {
				job = "cycle"
			}
			name := controllerArtifactName(job, runID, 1)
			data := scenario.receiptZip(runID, job)
			boundRunID := runID
			if scenario.artifactRunID != 0 {
				boundRunID = scenario.artifactRunID
			}
			boundHead := testHeadSHA
			if scenario.artifactHead != "" {
				boundHead = scenario.artifactHead
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "artifacts": []map[string]any{{
				"id": runID * 100, "name": name, "size_in_bytes": len(data), "expired": false,
				"workflow_run": map[string]any{"id": boundRunID, "repository_id": 12345, "head_branch": "main", "head_sha": boundHead},
			}}})
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/actions/artifacts/") && strings.HasSuffix(request.URL.Path, "/zip"):
			var artifactID int64
			_, _ = fmt.Sscanf(request.URL.Path, "/repos/acme/widget/actions/artifacts/%d/zip", &artifactID)
			w.Header().Set("Location", fmt.Sprintf("http://%s/downloads/%d", request.Host, artifactID))
			w.WriteHeader(http.StatusFound)
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/downloads/"):
			var artifactID int64
			_, _ = fmt.Sscanf(request.URL.Path, "/downloads/%d", &artifactID)
			runID := artifactID / 100
			job := scenario.executedJobs[runID]
			if job == "" {
				job = "cycle"
			}
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(scenario.receiptZip(runID, job))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/runs"):
			if request.URL.Query().Get("branch") != config.DefaultBranch || request.URL.Query().Get("exclude_pull_requests") != "true" {
				t.Errorf("unexpected workflow run query: %s", request.URL.RawQuery)
			}
			status := request.URL.Query().Get("status")
			var runs []map[string]any
			if status == "completed" {
				runs = scenario.completed
			} else {
				runs = scenario.active[status]
			}
			if runs == nil {
				runs = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
		default:
			t.Errorf("unexpected GitHub request: %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
			writeAPIError(w, http.StatusNotFound)
		}
	})
}

func (scenario serverScenario) runEvent(runID int64) string {
	for _, run := range scenario.completed {
		if value, ok := run["id"].(int64); ok && value == runID {
			if event, ok := run["event"].(string); ok {
				return event
			}
		}
	}
	return ""
}

func (scenario serverScenario) receiptZip(runID int64, job string) []byte {
	operation := scenario.receiptOperation
	if operation == "" {
		operation = map[string]string{"cycle": "cycle", "control": "resume", "status": "status"}[job]
	}
	stateAfter := scenario.receiptStateAfter
	if stateAfter == "" {
		stateAfter = testStateSHA
	}
	receipt := scenario.receiptOverride
	if receipt == nil {
		receipt, _ = json.Marshal(map[string]any{
			"schema_version": "hive.hosted-cycle.v1", "repository": "acme/widget",
			"operation": operation, "request_id": fmt.Sprintf("request-%d", runID),
			"state_branch": "hive/state-12345", "state_before": strings.Repeat("3", 40),
			"state_after": stateAfter, "sequence": 3, "paused": scenario.receiptPaused, "release": scenario.receiptRelease,
		})
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, _ := archive.Create(controllerArtifactName(job, runID, 1) + ".json")
	_, _ = file.Write(receipt)
	_ = archive.Close()
	return buffer.Bytes()
}

func testRun(id int64, event, status, conclusion string, updated time.Time) map[string]any {
	created := updated.Add(-5 * time.Minute)
	return map[string]any{
		"id": id, "html_url": fmt.Sprintf("https://github.com/acme/widget/actions/runs/%d", id),
		"event": event, "status": status, "conclusion": conclusion,
		"path": integrated.HostedControllerWorkflowPath, "head_branch": "main", "head_sha": testHeadSHA,
		"run_number": int(id), "run_attempt": 1, "workflow_id": 77,
		"created_at": created.Format(time.RFC3339), "run_started_at": created.Add(time.Minute).Format(time.RFC3339), "updated_at": updated.Format(time.RFC3339),
		"repository": map[string]any{"full_name": "acme/widget"},
	}
}

func testConfig(t *testing.T) integrated.Config {
	t.Helper()
	config := integrated.Config{
		Repository: "acme/widget", RepositoryID: "12345", DefaultBranch: "main", ExecutionMode: integrated.ExecutionHosted,
		RunIntervalSeconds: 900, HostedSchedule: "*/15 * * * *", HostedStateBranch: "hive/state-12345",
		HiveReleaseRepository: "DavidDiaz0317/hive", HiveReleaseVersion: "v0.4.1-integrated.19",
		HiveCommit: strings.Repeat("a", 40), VisualHiveRef: strings.Repeat("b", 40), DistributionManifestSHA256: strings.Repeat("c", 64),
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}
	workflow, err := integrated.GenerateHostedControllerWorkflow(integrated.HostedWorkflowConfig{
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		ScheduleCron: config.HostedSchedule, StateBranch: config.HostedStateBranch, StateKeySecret: stateSecretName,
		ReleaseRepository: config.HiveReleaseRepository, ReleaseTag: config.HiveReleaseVersion,
		HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef, DistributionManifestSHA256: config.DistributionManifestSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(workflow))
	config.HostedWorkflowSHA256 = hex.EncodeToString(digest[:])
	return config
}

func testHostedRelease(config integrated.Config) integrated.HostedReleaseIdentity {
	return integrated.HostedReleaseIdentity{
		Version: config.HiveReleaseVersion, HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
		DistributionManifestSHA256: config.DistributionManifestSHA256, HostedControllerProtocol: config.HostedControllerProtocol,
	}
}

func testClient(t *testing.T, serverURL string) *gh.Client {
	t.Helper()
	client := gh.NewClient(nil)
	base, err := url.Parse(serverURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	client.UploadURL = base
	return client
}

func writeAPIError(writer http.ResponseWriter, status int) {
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(`{"message":"not found"}`))
}

func findCheck(t *testing.T, result Result, name string) Check {
	t.Helper()
	for _, check := range result.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("missing check %q in %#v", name, result.Checks)
	return Check{}
}

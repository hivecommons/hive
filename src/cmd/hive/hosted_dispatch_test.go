package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/hostedstatus"
	"github.com/kubestellar/hive/pkg/integrated"
)

func TestHostedStatusAndDoctorUseRemoteReceiptNotStaleLocalPauseState(t *testing.T) {
	localActive := integrated.Config{Paused: false, ExecutionMode: integrated.ExecutionHosted}
	head := strings.Repeat("a", 40)
	receipt := hostedstatus.OperationResult{StateReceiptEvidence: hostedstatus.StateReceiptEvidence{StateAfter: head}, Conclusion: "success"}
	remoteLifecycle := map[string]any{"schema_version": "hive.lifecycle-status.v1", "production_ready": true}
	remoteDoctor := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": true}
	remotePaused := hostedstatus.Result{ProductionReady: false, Paused: true, StateBranchHeadSHA: head, Checks: []hostedstatus.Check{{Name: hostedstatus.CheckRemotePauseState, Details: "paused"}}}
	status, ready := hostedStatusDocument("state", localActive, remotePaused, receipt, remoteLifecycle)
	if ready || status["paused"] != true || status["production_ready"] != false {
		t.Fatalf("status trusted stale active local config: %#v", status)
	}
	doctor, _, ready := hostedDoctorDocument(remotePaused, receipt, remoteDoctor, nil)
	if ready || doctor["production_ready"] != false {
		t.Fatalf("doctor trusted stale active local config: %#v", doctor)
	}

	localPaused := integrated.Config{Paused: true, ExecutionMode: integrated.ExecutionHosted}
	remoteActive := hostedstatus.Result{ProductionReady: true, Paused: false, StateBranchHeadSHA: head, Checks: []hostedstatus.Check{{Name: hostedstatus.CheckRemotePauseState, Passed: true, Details: "active"}}}
	status, ready = hostedStatusDocument("state", localPaused, remoteActive, receipt, remoteLifecycle)
	if !ready || status["paused"] != false || status["production_ready"] != true {
		t.Fatalf("status ignored authoritative remote resume: %#v", status)
	}
	doctor, _, ready = hostedDoctorDocument(remoteActive, receipt, remoteDoctor, nil)
	if !ready || doctor["production_ready"] != true {
		t.Fatalf("doctor ignored authoritative remote resume: %#v", doctor)
	}
}

func TestDispatchHostedOperationUsesManagedWorkflowAndExactIdempotencyKey(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{
		Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main",
		ExecutionMode: integrated.ExecutionHosted, HostedStateBranch: "hive/state-42",
	}); err != nil {
		t.Fatal(err)
	}
	var method, path string
	var request struct {
		Ref    string                 `json:"ref"`
		Inputs map[string]interface{} `json:"inputs"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method == http.MethodGet && strings.HasSuffix(incoming.URL.Path, "/branches/main") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": strings.Repeat("a", 40)}})
			return
		}
		method, path = incoming.Method, incoming.URL.Path
		if err := json.NewDecoder(incoming.Body).Decode(&request); err != nil {
			t.Errorf("decode dispatch request: %v", err)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("HIVE_GITHUB_TOKEN", "test-token")
	priorWait := waitForHostedControllerOperation
	waitForHostedControllerOperation = func(_ context.Context, _ *gh.Client, config integrated.Config, operation, requestID, head string, issuedAt time.Time) (hostedstatus.OperationResult, error) {
		if config.Repository != "owner/repository" || operation != "pause" || requestID != "operator-pause-1" || head != strings.Repeat("a", 40) || issuedAt.IsZero() {
			t.Fatalf("unexpected completion wait binding: config=%+v operation=%s request=%s head=%s issued=%s", config, operation, requestID, head, issuedAt)
		}
		return hostedstatus.OperationResult{StateReceiptEvidence: hostedstatus.StateReceiptEvidence{Operation: operation, RequestID: requestID, Paused: true}, Conclusion: "success"}, nil
	}
	defer func() { waitForHostedControllerOperation = priorWait }()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = writer
	code := dispatchHostedOperation(stateDir, "pause", "operator-pause-1", "HIVE_GITHUB_TOKEN", server.URL, true)
	_ = writer.Close()
	os.Stdout = prior
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if code != 0 {
		t.Fatalf("dispatch exit=%d output=%s", code, output)
	}
	var result hostedDispatchResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode dispatch result: %v: %s", err, output)
	}
	if !result.Accepted || !result.Completed || result.Receipt == nil || result.RequestID != "operator-pause-1" || result.Operation != "pause" || result.Ref != "main" {
		t.Fatalf("unexpected hosted dispatch result: %+v", result)
	}
	if method != http.MethodPost || request.Ref != "main" || request.Inputs["operation"] != "pause" || request.Inputs["request_id"] != "operator-pause-1" {
		t.Fatalf("unexpected hosted dispatch request: method=%s path=%s body=%+v", method, path, request)
	}
	if path != "/repos/owner/repository/actions/workflows/"+integrated.HostedControllerWorkflowPath+"/dispatches" {
		t.Fatalf("dispatch used unexpected workflow path %q", path)
	}
}

func TestDispatchHostedOperationReportsAmbiguousRetryIdentity(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(integrated.Config{Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main", ExecutionMode: integrated.ExecutionHosted}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method == http.MethodGet && strings.HasSuffix(incoming.URL.Path, "/branches/main") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": strings.Repeat("a", 40)}})
			return
		}
		http.Error(writer, `{"message":"ambiguous"}`, http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("HIVE_GITHUB_TOKEN", "test-token")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prior := os.Stdout
	os.Stdout = writer
	code := dispatchHostedOperation(stateDir, "cycle", "same-cycle-id", "HIVE_GITHUB_TOKEN", server.URL, true)
	_ = writer.Close()
	os.Stdout = prior
	output, _ := io.ReadAll(reader)
	_ = reader.Close()
	if code == 0 {
		t.Fatalf("ambiguous transport unexpectedly succeeded: %s", output)
	}
	var result hostedDispatchResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode failure result: %v: %s", err, output)
	}
	if result.RequestID != "same-cycle-id" || result.RetryCommand == "" || result.Error == "" || result.Accepted {
		t.Fatalf("ambiguous dispatch lost its exact recovery identity: %+v", result)
	}
}

func TestExecuteHostedControllerOperationUsesExplicitCandidateWithoutOutput(t *testing.T) {
	target := integrated.Config{
		Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main",
		ExecutionMode: integrated.ExecutionHosted, HostedStateBranch: "hive/state-42",
		HiveReleaseVersion: "v0.4.1-integrated.20", HiveCommit: strings.Repeat("b", 40),
		VisualHiveRef: strings.Repeat("c", 40), DistributionManifestSHA256: strings.Repeat("d", 64),
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method == http.MethodGet && strings.HasSuffix(incoming.URL.Path, "/branches/main") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": strings.Repeat("a", 40)}})
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")

	priorWait := waitForHostedControllerOperation
	waitForHostedControllerOperation = func(_ context.Context, _ *gh.Client, config integrated.Config, operation, requestID, head string, issuedAt time.Time) (hostedstatus.OperationResult, error) {
		if config.HiveReleaseVersion != target.HiveReleaseVersion || config.HiveCommit != target.HiveCommit ||
			config.VisualHiveRef != target.VisualHiveRef || config.DistributionManifestSHA256 != target.DistributionManifestSHA256 ||
			config.HostedControllerProtocol != target.HostedControllerProtocol {
			t.Fatalf("controller wait did not use exact candidate config: %+v", config)
		}
		return hostedstatus.OperationResult{StateReceiptEvidence: hostedstatus.StateReceiptEvidence{
			Operation: operation, RequestID: requestID, Paused: true, StateAfter: head,
		}, Conclusion: "success"}, nil
	}
	defer func() { waitForHostedControllerOperation = priorWait }()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	priorStdout := os.Stdout
	os.Stdout = writer
	result, runErr := executeHostedControllerOperation(context.Background(), client, target, "pause", "transition-target-pause-1", time.Now().UTC())
	_ = writer.Close()
	os.Stdout = priorStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if runErr != nil || !result.Accepted || !result.Completed || result.Receipt == nil {
		t.Fatalf("explicit candidate operation failed: result=%+v err=%v", result, runErr)
	}
	if len(output) != 0 {
		t.Fatalf("non-emitting controller helper wrote stdout: %q", output)
	}
}

func TestExecuteHostedControllerReadOperationUsesDurableIdentityWithoutOutput(t *testing.T) {
	target := integrated.Config{
		Repository: "owner/repository", RepositoryID: "42", DefaultBranch: "main",
		ExecutionMode: integrated.ExecutionHosted, HostedStateBranch: "hive/state-42",
		HiveReleaseVersion: "v0.4.1-integrated.20", HiveCommit: strings.Repeat("b", 40),
		VisualHiveRef: strings.Repeat("c", 40), DistributionManifestSHA256: strings.Repeat("d", 64),
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}
	defaultHead := strings.Repeat("a", 40)
	stateHead := strings.Repeat("e", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method == http.MethodGet && strings.HasSuffix(incoming.URL.Path, "/branches/main") {
			_ = json.NewEncoder(writer).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": defaultHead}})
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := gh.NewClient(nil)
	client.BaseURL, _ = client.BaseURL.Parse(server.URL + "/")

	priorWait, priorInspect := waitForHostedReadOperation, inspectHostedControllerStatus
	waitForHostedReadOperation = func(_ context.Context, _ *gh.Client, config integrated.Config, operation, requestID, head string, issuedAt time.Time) (hostedstatus.OperationResult, error) {
		if config.HiveReleaseVersion != target.HiveReleaseVersion || operation != "status" || requestID != "transition-preflight-status-1" || head != defaultHead || issuedAt.IsZero() {
			t.Fatalf("unexpected exact read binding: config=%+v operation=%s request=%s head=%s issued=%s", config, operation, requestID, head, issuedAt)
		}
		return hostedstatus.OperationResult{StateReceiptEvidence: hostedstatus.StateReceiptEvidence{
			Operation: operation, RequestID: requestID, StateAfter: stateHead, Release: integrated.HostedReleaseIdentity{
				Version: target.HiveReleaseVersion, HiveCommit: target.HiveCommit, VisualHiveCommit: target.VisualHiveRef,
				DistributionManifestSHA256: target.DistributionManifestSHA256, HostedControllerProtocol: target.HostedControllerProtocol,
			}, StatusResult: json.RawMessage(`{"schema_version":"hive.lifecycle-status.v1","production_ready":false,"quiescent":true}`),
		}, Conclusion: "success"}, nil
	}
	inspectHostedControllerStatus = func(_ context.Context, _ *gh.Client, config integrated.Config, checkedAt time.Time) (hostedstatus.Result, error) {
		if config.HiveCommit != target.HiveCommit || checkedAt.IsZero() {
			t.Fatalf("unexpected inspected candidate config: %+v", config)
		}
		return hostedstatus.Result{Repository: config.Repository, DefaultHeadSHA: defaultHead, StateBranchHeadSHA: stateHead, Paused: true}, nil
	}
	defer func() {
		waitForHostedReadOperation = priorWait
		inspectHostedControllerStatus = priorInspect
	}()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	priorStdout := os.Stdout
	os.Stdout = writer
	receipt, controller, runErr := executeHostedControllerReadOperation(context.Background(), client, target, "status", "transition-preflight-status-1", time.Now().UTC())
	_ = writer.Close()
	os.Stdout = priorStdout
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if runErr != nil || receipt.StateAfter != stateHead || controller.StateBranchHeadSHA != stateHead {
		t.Fatalf("explicit read failed: receipt=%+v controller=%+v err=%v", receipt, controller, runErr)
	}
	if len(output) != 0 {
		t.Fatalf("non-emitting read helper wrote stdout: %q", output)
	}
}

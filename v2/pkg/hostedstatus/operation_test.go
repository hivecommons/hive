package hostedstatus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestWaitForReadOperationReturnsOnlyExactStatusReceipt(t *testing.T) {
	now := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	config := testConfig(t)
	exact := testRun(501, "workflow_dispatch", "completed", "success", now.Add(6*time.Minute))
	exact["display_title"] = "Hive status status-request-501"
	unrelated := testRun(502, "workflow_dispatch", "completed", "success", now.Add(7*time.Minute))
	unrelated["display_title"] = "Hive doctor unrelated-request"
	scenario := greenScenario(t, config, now.Add(8*time.Minute))
	scenario.completed = []map[string]any{unrelated, exact, testRun(101, "schedule", "completed", "success", now.Add(-time.Minute))}
	scenario.executedJobs[501] = "status"
	scenario.receiptOperation = "status"
	scenario.receiptOverride, _ = json.Marshal(map[string]any{
		"schema_version": "hive.hosted-cycle.v1", "repository": config.Repository, "operation": "status", "request_id": "status-request-501",
		"state_branch": config.HostedStateBranch, "state_before": testStateSHA, "state_after": testStateSHA, "sequence": 7, "paused": false,
		"release": testHostedRelease(config),
		"status":  map[string]any{"schema_version": "hive.lifecycle-status.v1", "repository": config.Repository, "production_ready": true},
	})
	server := httptest.NewServer(scenario.handler(t, config, now.Add(8*time.Minute)))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := WaitForReadOperation(ctx, testClient(t, server.URL), config, "status", "status-request-501", testHeadSHA, now)
	if err != nil {
		t.Fatalf("wait for exact hosted status: %v", err)
	}
	if result.Run.ID != 501 || result.Job != "status" || result.Operation != "status" || result.RequestID != "status-request-501" || result.StateAfter != testStateSHA || result.Conclusion != "success" {
		t.Fatalf("unexpected exact status result: %+v", result)
	}
	if !strings.Contains(string(result.StatusResult), "hive.lifecycle-status.v1") {
		t.Fatalf("exact status result was lost: %s", result.StatusResult)
	}
}

func TestValidateRequestedOperationReceiptRejectsReleaseMismatch(t *testing.T) {
	config := testConfig(t)
	paused := false
	receipt := controllerReceipt{
		SchemaVersion: "hive.hosted-cycle.v1", Repository: config.Repository, Operation: "status", RequestID: "status-request-identity",
		StateBranch: config.HostedStateBranch, StateBefore: testStateSHA, StateAfter: testStateSHA, Sequence: 7, Paused: &paused,
		Release: testHostedRelease(config), Status: json.RawMessage(`{"schema_version":"hive.lifecycle-status.v1"}`),
	}
	run := RunEvidence{Event: "workflow_dispatch", RunAttempt: 1, DisplayTitle: "Hive status status-request-identity", HeadSHA: testHeadSHA}
	if err := validateRequestedOperationReceipt(context.Background(), nil, "acme", "widget", receipt, config, run, "status", testStateSHA, "status", "status-request-identity", "", testHeadSHA); err != nil {
		t.Fatalf("exact receipt release rejected: %v", err)
	}
	for name, mutate := range map[string]func(*integrated.HostedReleaseIdentity){
		"manifest": func(release *integrated.HostedReleaseIdentity) {
			release.DistributionManifestSHA256 = strings.Repeat("9", 64)
		},
		"protocol": func(release *integrated.HostedReleaseIdentity) { release.HostedControllerProtocol++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed.Release)
			if err := validateRequestedOperationReceipt(context.Background(), nil, "acme", "widget", changed, config, run, "status", testStateSHA, "status", "status-request-identity", "", testHeadSHA); err == nil || !strings.Contains(err.Error(), "exact immutable hosted release") {
				t.Fatalf("mismatched receipt release was accepted: %v", err)
			}
		})
	}
}

func TestValidateHostedOperatorTerminalReceiptRejectsDigestAndActorMismatch(t *testing.T) {
	result := json.RawMessage(`{"schema_version":"hive.approve-merge.v1","planned":true}`)
	terminal := hostedOperatorTerminalReceipt{SchemaVersion: "hive.hosted-operator-terminal.v1", Outcome: "succeeded", Result: result}
	encoded, _ := json.Marshal(terminal)
	receipt := controllerReceipt{
		Operator: result, OperatorStatus: "succeeded", OperatorTerminalSHA256: digestHex(encoded), OperatorLedgerSequence: 8,
		Sequence: 8,
	}
	if err := validateHostedOperatorTerminalReceipt(receipt); err != nil {
		t.Fatalf("valid terminal receipt rejected: %v", err)
	}
	receipt.OperatorTerminalSHA256 = strings.Repeat("0", 64)
	if err := validateHostedOperatorTerminalReceipt(receipt); err == nil {
		t.Fatal("wrong terminal digest was accepted")
	}
}

func TestWaitForControllerOperationProvesExactPauseCompletion(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)
	config := testConfig(t)
	exact := testRun(601, "workflow_dispatch", "completed", "success", now.Add(6*time.Minute))
	exact["display_title"] = "Hive pause pause-request-601"
	scenario := greenScenario(t, config, now.Add(3*time.Minute))
	scenario.completed = []map[string]any{exact}
	scenario.executedJobs[601] = "control"
	scenario.receiptOperation = "pause"
	scenario.receiptOverride, _ = json.Marshal(map[string]any{
		"schema_version": "hive.hosted-cycle.v1", "repository": config.Repository, "operation": "pause", "request_id": "pause-request-601",
		"state_branch": config.HostedStateBranch, "state_before": strings.Repeat("3", 40), "state_after": testStateSHA,
		"sequence": 8, "paused": true, "release": testHostedRelease(config),
	})
	server := httptest.NewServer(scenario.handler(t, config, now.Add(3*time.Minute)))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := WaitForControllerOperation(ctx, testClient(t, server.URL), config, "pause", "pause-request-601", testHeadSHA, now)
	if err != nil {
		t.Fatalf("wait for exact pause completion: %v", err)
	}
	if result.Run.ID != 601 || result.Job != "control" || !result.Paused || result.Operation != "pause" || result.Conclusion != "success" {
		t.Fatalf("unexpected exact pause receipt: %+v", result)
	}
}

func TestWaitForControllerOperationClassifiesExactRunWithoutReceipt(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 30, 0, 0, time.UTC)
	config := testConfig(t)
	exact := testRun(602, "workflow_dispatch", "completed", "success", now.Add(6*time.Minute))
	exact["display_title"] = "Hive resume resume-request-602"
	scenario := greenScenario(t, config, now.Add(3*time.Minute))
	scenario.completed = []map[string]any{exact}
	scenario.executedJobs[602] = "control"
	scenario.noArtifactRuns = map[int64]bool{602: true}
	server := httptest.NewServer(scenario.handler(t, config, now.Add(3*time.Minute)))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := WaitForControllerOperation(ctx, testClient(t, server.URL), config, "resume", "resume-request-602", testHeadSHA, now)
	if !errors.Is(err, ErrOperationRunTerminatedWithoutReceipt) {
		t.Fatalf("exact terminal run without artifact was not classified for bounded recovery: %v", err)
	}
}

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

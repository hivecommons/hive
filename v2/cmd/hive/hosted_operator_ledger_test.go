package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/hostedstate"
	"github.com/kubestellar/hive/v2/pkg/hostedstatus"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

func TestHostedOperatorLedgerAuthenticatesResumesAndReplaysExactTerminalResult(t *testing.T) {
	root := t.TempDir()
	secret := []byte(strings.Repeat("operator-ledger-secret-", 2))
	config := hostedOperatorLedgerConfig()
	now := time.Date(2026, 7, 16, 1, 2, 3, 0, time.UTC)
	request := hostedOperatorLedgerRequest(t, config, now)
	t.Setenv("GITHUB_SHA", request.DefaultHeadSHA)
	_, digest, requestBytes, err := encodeHostedOperationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	actor := request.Actor
	controller := hostedstate.ControllerIdentity{RunID: 91, Attempt: 1, WorkflowPath: integrated.HostedControllerWorkflowPath, WorkflowSHA: strings.Repeat("4", 40)}

	accepted, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, actor, request.ExpectedStateHeadSHA, 5, controller, now)
	if err != nil || !accepted.New || accepted.Status != "accepted" {
		t.Fatalf("accept request: result=%+v err=%v", accepted, err)
	}
	t.Setenv("GITHUB_SHA", strings.Repeat("7", 40))
	resumed, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, actor, strings.Repeat("8", 40), 6, controller, now.Add(time.Hour))
	if err != nil || !resumed.Resume || resumed.New || resumed.Terminal {
		t.Fatalf("resume accepted request after head/expiry changed: result=%+v err=%v", resumed, err)
	}

	typed := map[string]any{"schema_version": "hive.approve-merge.v1", "planned": true, "pull_request": 7}
	terminal, err := finishHostedOperatorRequest(root, secret, config, request, digest, actor, typed, nil, 6, controller, now.Add(time.Minute))
	if err != nil || terminal.Status != "succeeded" || terminal.TerminalSequence != 7 || terminal.TerminalSHA256 == "" {
		t.Fatalf("finish request: result=%+v err=%v", terminal, err)
	}
	replay, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, actor, strings.Repeat("9", 40), 12, controller, now.Add(24*time.Hour))
	if err != nil || !replay.Terminal || replay.Status != "succeeded" || replay.TerminalSHA256 != terminal.TerminalSHA256 || replay.TerminalSequence != terminal.TerminalSequence {
		t.Fatalf("replay terminal request: result=%+v err=%v", replay, err)
	}
	var got map[string]any
	if err := json.Unmarshal(replay.Result, &got); err != nil || got["schema_version"] != "hive.approve-merge.v1" || got["pull_request"] != float64(7) {
		t.Fatalf("terminal replay changed typed result: %s err=%v", replay.Result, err)
	}

	changed := request
	changed.Operation = hostedApproveBaselinePlan
	if _, err := prepareHostedOperatorRequest(root, secret, config, changed, requestBytes, digest, actor, request.ExpectedStateHeadSHA, 12, controller, now); err == nil {
		t.Fatal("same request ID was rebound to a different operation")
	}

	path := filepath.Join(root, "integrated", hostedOperatorLedgerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadHostedOperatorLedger(root, secret, config); err == nil {
		t.Fatal("tampered operator ledger authenticated")
	}
}

func TestHostedOperatorLedgerReplaysTerminalFailureWithoutExecution(t *testing.T) {
	root := t.TempDir()
	secret := []byte(strings.Repeat("failure-ledger-secret--", 2))
	config := hostedOperatorLedgerConfig()
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	request := hostedOperatorLedgerRequest(t, config, now)
	t.Setenv("GITHUB_SHA", request.DefaultHeadSHA)
	_, digest, requestBytes, _ := encodeHostedOperationRequest(request)
	controller := hostedstate.ControllerIdentity{RunID: 92, Attempt: 1, WorkflowPath: integrated.HostedControllerWorkflowPath, WorkflowSHA: strings.Repeat("5", 40)}
	if _, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, request.Actor, request.ExpectedStateHeadSHA, 10, controller, now); err != nil {
		t.Fatal(err)
	}
	terminal, err := finishHostedOperatorRequest(root, secret, config, request, digest, request.Actor, map[string]any{"planned": false}, assertError("live pull request moved"), 11, controller, now.Add(time.Minute))
	if err != nil || terminal.Status != "failed" {
		t.Fatalf("finish failure: %+v %v", terminal, err)
	}
	replay, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, request.Actor, strings.Repeat("6", 40), 20, controller, now.Add(time.Hour))
	if err != nil || replay.Error != "live pull request moved" || replay.Status != "failed" {
		t.Fatalf("failure was not replayed exactly: %+v %v", replay, err)
	}
}

func TestHostedOperatorLedgerSerializesAcceptedRequests(t *testing.T) {
	root := t.TempDir()
	secret := []byte(strings.Repeat("serialized-ledger-secret", 2))
	config := hostedOperatorLedgerConfig()
	now := time.Date(2026, 7, 16, 2, 30, 0, 0, time.UTC)
	first := hostedOperatorLedgerRequest(t, config, now)
	t.Setenv("GITHUB_SHA", first.DefaultHeadSHA)
	_, firstDigest, firstBytes, err := encodeHostedOperationRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	controller := hostedstate.ControllerIdentity{RunID: 93, Attempt: 1, WorkflowPath: integrated.HostedControllerWorkflowPath, WorkflowSHA: strings.Repeat("6", 40)}
	if _, err := prepareHostedOperatorRequest(root, secret, config, first, firstBytes, firstDigest, first.Actor, first.ExpectedStateHeadSHA, 20, controller, now); err != nil {
		t.Fatal(err)
	}
	second := first
	second.RequestID = "operator-request-2"
	second.Operation = hostedApproveMergeApply
	second.Arguments, err = marshalHostedArguments(hostedMergeArguments{PRNumber: 7, HeadSHA: strings.Repeat("d", 40), BaseSHA: strings.Repeat("a", 40), DiffDigest: strings.Repeat("b", 64), Reason: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	_, secondDigest, secondBytes, err := encodeHostedOperationRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHostedOperatorRequest(root, secret, config, second, secondBytes, secondDigest, second.Actor, second.ExpectedStateHeadSHA, 20, controller, now); err == nil || !strings.Contains(err.Error(), first.RequestID) {
		t.Fatalf("second accepted request was not blocked by exact pending identity: %v", err)
	}
}

func TestHostedOperatorLedgerCompactsOnlyExpiredTerminalResults(t *testing.T) {
	root := t.TempDir()
	secret := []byte(strings.Repeat("compaction-ledger-secret-", 2))
	config := hostedOperatorLedgerConfig()
	start := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	controller := hostedstate.ControllerIdentity{Attempt: 1, WorkflowPath: integrated.HostedControllerWorkflowPath, WorkflowSHA: strings.Repeat("6", 40)}
	const total = hostedOperatorExpiredRetained + 20
	for index := 0; index < total; index++ {
		now := start.Add(time.Duration(index) * time.Minute)
		request := hostedOperatorLedgerRequest(t, config, now)
		request.RequestID = fmt.Sprintf("operator-compact-%03d", index)
		t.Setenv("GITHUB_SHA", request.DefaultHeadSHA)
		_, digest, requestBytes, err := encodeHostedOperationRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		controller.RunID = uint64(1000 + index)
		sequence := uint64(index*2 + 1)
		if _, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, request.Actor, request.ExpectedStateHeadSHA, sequence, controller, now); err != nil {
			t.Fatalf("prepare %d: %v", index, err)
		}
		if _, err := finishHostedOperatorRequest(root, secret, config, request, digest, request.Actor, map[string]any{"planned": true, "index": index}, nil, sequence+1, controller, now.Add(time.Second)); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
	}
	ledger, err := loadHostedOperatorLedger(root, secret, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Requests) >= total || len(ledger.Requests) > hostedOperatorExpiredRetained+int(hostedOperationLifetime/time.Minute)+1 {
		t.Fatalf("expired terminal results were not compacted within the bounded recent window: %d", len(ledger.Requests))
	}
	for _, record := range ledger.Requests {
		if record.Status == "accepted" {
			t.Fatalf("terminal-only compaction invented an active request: %+v", record)
		}
	}
	request := hostedOperatorLedgerRequest(t, config, start.Add(2*time.Hour))
	request.RequestID = "operator-after-compaction"
	t.Setenv("GITHUB_SHA", request.DefaultHeadSHA)
	_, digest, requestBytes, _ := encodeHostedOperationRequest(request)
	controller.RunID++
	prepared, err := prepareHostedOperatorRequest(root, secret, config, request, requestBytes, digest, request.Actor, request.ExpectedStateHeadSHA, 999, controller, request.IssuedAt)
	if err != nil || !prepared.New {
		t.Fatalf("ledger did not admit a new request after deterministic compaction: prepared=%+v err=%v", prepared, err)
	}
}

func TestHostedOutboundRequestReusesExactBytesAcrossAmbiguousRetry(t *testing.T) {
	stateDir := t.TempDir()
	config := hostedOperatorLedgerConfig()
	actor := integrated.HostedOperatorIdentity{ID: 42, Login: "maintainer"}
	arguments, err := marshalHostedArguments(hostedMergeArguments{PRNumber: 7, HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	first, encoded, digest, created, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, "same-request", actor, arguments, strings.Repeat("1", 40), strings.Repeat("2", 40), now)
	if err != nil || !created {
		t.Fatalf("create outbound request: created=%t err=%v", created, err)
	}
	second, encodedAgain, digestAgain, createdAgain, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, "same-request", actor, arguments, strings.Repeat("8", 40), strings.Repeat("9", 40), now.Add(time.Hour))
	if err != nil || createdAgain || encodedAgain != encoded || digestAgain != digest || !second.IssuedAt.Equal(first.IssuedAt) || second.DefaultHeadSHA != first.DefaultHeadSHA || second.ExpectedStateHeadSHA != first.ExpectedStateHeadSHA {
		t.Fatalf("ambiguous retry changed exact request: first=%+v second=%+v created=%t digest=%s/%s err=%v", first, second, createdAgain, digest, digestAgain, err)
	}
	different, _ := marshalHostedArguments(hostedMergeArguments{PRNumber: 8, HeadSHA: strings.Repeat("a", 40)})
	if _, _, _, _, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, "same-request", actor, different, strings.Repeat("1", 40), strings.Repeat("2", 40), now); err == nil {
		t.Fatal("outbound request ID was rebound to different arguments")
	}
}

func TestHostedOutboundJournalRetiresTerminalPayloadsWithoutRedispatchAuthority(t *testing.T) {
	stateDir := t.TempDir()
	config := hostedOperatorLedgerConfig()
	actor := integrated.HostedOperatorIdentity{ID: 42, Login: "maintainer"}
	arguments, err := marshalHostedArguments(hostedMergeArguments{PRNumber: 7, HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 7, 16, 7, 0, 0, 0, time.UTC)
	var firstDigest string
	for index := 0; index < hostedOutboundRecentTerminals+20; index++ {
		requestID := fmt.Sprintf("outbound-terminal-%03d", index)
		now := start.Add(time.Duration(index) * time.Minute)
		_, _, digest, created, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, requestID, actor, arguments, strings.Repeat("1", 40), strings.Repeat("2", 40), now)
		if err != nil || !created {
			t.Fatalf("create outbound %d: created=%t err=%v", index, created, err)
		}
		if index == 0 {
			firstDigest = digest
		}
		receipt := hostedstatus.OperationResult{StateReceiptEvidence: hostedstatus.StateReceiptEvidence{
			RequestID: requestID, RequestSHA256: digest, OperatorStatus: "succeeded",
			OperatorLedgerSequence: uint64(index + 1), OperatorTerminalSHA256: fmt.Sprintf("%064x", index+1),
		}, Conclusion: "success"}
		if err := markHostedOutboundTerminal(stateDir, config, requestID, digest, receipt, now.Add(time.Second)); err != nil {
			t.Fatalf("mark outbound terminal %d: %v", index, err)
		}
	}
	ledger, err := loadHostedOutboundLedger(stateDir, config)
	if err != nil {
		t.Fatal(err)
	}
	retired := 0
	for _, record := range ledger.Requests {
		if record.Status == "retired" {
			retired++
			if record.RequestPayload != "" {
				t.Fatal("retired outbound record retained executable request bytes")
			}
		}
	}
	if retired == 0 {
		t.Fatalf("expired terminal requests were not retired: %+v", ledger)
	}
	if _, _, _, _, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, "outbound-terminal-000", actor, arguments, strings.Repeat("8", 40), strings.Repeat("9", 40), start.Add(3*time.Hour)); !errors.Is(err, errHostedOutboundTerminalRetired) {
		t.Fatalf("retired request regained redispatch authority: digest=%s err=%v", firstDigest, err)
	}
	_, _, _, created, err := loadOrCreateHostedOutboundRequest(stateDir, config, hostedApproveMergePlan, "outbound-after-retirement", actor, arguments, strings.Repeat("8", 40), strings.Repeat("9", 40), start.Add(3*time.Hour))
	if err != nil || !created {
		t.Fatalf("journal did not admit a new request after retirement: created=%t err=%v", created, err)
	}
}

func hostedOperatorLedgerConfig() integrated.Config {
	return integrated.Config{
		Repository: "owner/repo", RepositoryID: "123", DefaultBranch: "main", ExecutionMode: integrated.ExecutionHosted,
		HostedStateBranch: "hive/state-123", HiveReleaseRepository: "DavidDiaz0317/hive", HiveReleaseVersion: "v0.4.1-integrated.19",
		HiveCommit: strings.Repeat("a", 40), VisualHiveRef: strings.Repeat("b", 40), DistributionManifestSHA256: strings.Repeat("c", 64),
		HostedControllerProtocol: integrated.HostedControllerProtocol,
	}
}

func hostedOperatorLedgerRequest(t *testing.T, config integrated.Config, now time.Time) hostedOperationRequest {
	t.Helper()
	arguments, err := marshalHostedArguments(hostedMergeArguments{PRNumber: 7, HeadSHA: strings.Repeat("d", 40)})
	if err != nil {
		t.Fatal(err)
	}
	return hostedOperationRequest{
		SchemaVersion: hostedOperationRequestSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
		WorkflowPath: integrated.HostedControllerWorkflowPath, DefaultBranch: config.DefaultBranch, DefaultHeadSHA: strings.Repeat("e", 40),
		StateBranch: config.HostedStateBranch, ExpectedStateHeadSHA: strings.Repeat("f", 40),
		Release: hostedOperationRelease{
			Version: config.HiveReleaseVersion, HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
			DistributionManifestSHA256: config.DistributionManifestSHA256, HostedControllerProtocol: config.HostedControllerProtocol,
		},
		Operation: hostedApproveMergePlan, RequestID: "operator-request-1", IssuedAt: now, ExpiresAt: now.Add(hostedOperationLifetime),
		Actor: integrated.HostedOperatorIdentity{ID: 42, Login: "maintainer"}, Arguments: arguments,
	}
}

type assertError string

func (value assertError) Error() string { return string(value) }

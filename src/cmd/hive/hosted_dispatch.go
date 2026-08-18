package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hostedstatus"
	"github.com/kubestellar/hive/pkg/integrated"
)

type hostedDispatchResult struct {
	SchemaVersion string                        `json:"schema_version"`
	Repository    string                        `json:"repository"`
	Workflow      string                        `json:"workflow"`
	Ref           string                        `json:"ref"`
	Operation     string                        `json:"operation"`
	RequestID     string                        `json:"request_id"`
	Accepted      bool                          `json:"accepted"`
	Completed     bool                          `json:"completed"`
	Receipt       *hostedstatus.OperationResult `json:"receipt,omitempty"`
	Error         string                        `json:"error,omitempty"`
	RetryCommand  string                        `json:"retry_command,omitempty"`
}

type hostedOperatorCommandResult struct {
	SchemaVersion string                        `json:"schema_version"`
	Repository    string                        `json:"repository"`
	Operation     string                        `json:"operation"`
	RequestID     string                        `json:"request_id"`
	RequestSHA256 string                        `json:"request_sha256,omitempty"`
	Accepted      bool                          `json:"accepted"`
	Receipt       *hostedstatus.OperationResult `json:"receipt,omitempty"`
	Result        any                           `json:"result,omitempty"`
	Error         string                        `json:"error,omitempty"`
	RetryCommand  string                        `json:"retry_command,omitempty"`
}

type hostedRemoteOperatorRequest struct {
	RequestID        string                            `json:"request_id"`
	Operation        string                            `json:"operation"`
	RequestSHA256    string                            `json:"request_sha256"`
	Actor            integrated.HostedOperatorIdentity `json:"actor"`
	Status           string                            `json:"status,omitempty"`
	AcceptedAt       time.Time                         `json:"accepted_at"`
	AcceptedSequence uint64                            `json:"accepted_sequence"`
	TerminalAt       time.Time                         `json:"terminal_at,omitempty"`
	TerminalSequence uint64                            `json:"terminal_sequence,omitempty"`
	TerminalSHA256   string                            `json:"terminal_sha256,omitempty"`
}

type hostedRemoteOperatorLedger struct {
	SchemaVersion    string                        `json:"schema_version"`
	Pending          int                           `json:"pending"`
	Terminal         int                           `json:"terminal"`
	Retained         int                           `json:"retained"`
	Capacity         int                           `json:"capacity"`
	PendingRequests  []hostedRemoteOperatorRequest `json:"pending_requests"`
	TerminalRequests []hostedRemoteOperatorRequest `json:"terminal_requests"`
}

var (
	waitForHostedControllerOperation = hostedstatus.WaitForControllerOperation
	waitForHostedReadOperation       = hostedstatus.WaitForReadOperation
	inspectHostedControllerStatus    = hostedstatus.Inspect
)

func inspectHostedController(config integrated.Config, githubTokenEnv, githubAPIURL string) (hostedstatus.Result, error) {
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		return hostedstatus.Result{}, errors.New("GitHub authorization is required for hosted status")
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return hostedstatus.Inspect(ctx, client.GoGitHub(), config, time.Now().UTC())
}

func runHostedStatusCommand(stateDir string, config integrated.Config, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	receipt, controller, err := runHostedReadOperation(config, "status", githubTokenEnv, githubAPIURL)
	if err != nil {
		if jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.status.v1", "state_dir": stateDir, "repository": config.Repository, "execution_mode": integrated.ExecutionHosted, "production_ready": false, "error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "Hosted Hive status failed:", err)
		}
		return 1
	}
	remote, err := decodeHostedReadDocument(receipt.StatusResult, "hive.lifecycle-status.v1")
	if err != nil {
		if jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.status.v1", "state_dir": stateDir, "repository": config.Repository, "execution_mode": integrated.ExecutionHosted, "production_ready": false, "operation_receipt": receipt, "error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "Hosted Hive status failed:", err)
		}
		return 1
	}
	status, ready := hostedStatusDocument(stateDir, config, controller, receipt, remote)
	if jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("%s: hosted production_ready=%t latest_cycle=%v\n", config.Repository, ready, controller.LatestCompletedCycle)
	if !ready {
		return 1
	}
	return 0
}

func hostedStatusDocument(stateDir string, config integrated.Config, controller hostedstatus.Result, receipt hostedstatus.OperationResult, remote map[string]any) (map[string]any, bool) {
	remoteReady, _ := remote["production_ready"].(bool)
	ready := controller.ProductionReady && !controller.Paused && remoteReady && receipt.StateAfter == controller.StateBranchHeadSHA && receipt.Conclusion == "success" && receipt.Error == ""
	return map[string]any{
		"schema_version": "hive.status.v1", "state_dir": stateDir, "repository": config.Repository,
		"execution_mode": integrated.ExecutionHosted, "paused": controller.Paused, "production_ready": ready,
		"hosted_controller": controller, "remote_lifecycle": remote, "operation_receipt": receipt,
		"daemon_ready": true, "daemon": map[string]any{"required": false, "reason": "repository-owned hosted cadence"},
	}, ready
}

func runHostedDoctorCommand(config integrated.Config, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	receipt, controller, err := runHostedReadOperation(config, "doctor", githubTokenEnv, githubAPIURL)
	var remote map[string]any
	if err == nil {
		remote, err = decodeHostedReadDocument(receipt.DoctorResult, "hive.doctor.v1")
	}
	output, checks, ready := hostedDoctorDocument(controller, receipt, remote, err)
	if jsonOutput {
		_ = encodeJSON(output)
	} else {
		for _, check := range checks {
			fmt.Printf("[%s] %s: %s\n", ternary(check.OK, "ok", "fail"), check.Name, check.Message)
		}
	}
	if !ready {
		return 1
	}
	return 0
}

func hostedDoctorDocument(controller hostedstatus.Result, receipt hostedstatus.OperationResult, remote map[string]any, inspectErr error) (map[string]any, []doctorCheck, bool) {
	checks := []doctorCheck{{Name: "config", OK: true, Message: "persistent hosted config loaded"}}
	if inspectErr != nil {
		checks = append(checks, doctorCheck{Name: "hosted_controller", OK: false, Message: inspectErr.Error()})
	} else {
		for _, check := range controller.Checks {
			checks = append(checks, doctorCheck{Name: "hosted_" + check.Name, OK: check.Passed, Message: check.Details})
		}
	}
	paused := true
	if inspectErr == nil {
		paused = controller.Paused
	}
	checks = append(checks,
		doctorCheck{Name: "automation_active", OK: !paused, Message: ternary(paused, "hosted automation is paused or its remote state is unverified", "hosted automation is active")},
		doctorCheck{Name: "persistent_scheduler", OK: true, Message: "repository-owned GitHub cadence is installed; no bootstrap computer or local daemon is required"},
	)
	remoteReady, _ := remote["production_ready"].(bool)
	ready := inspectErr == nil && controller.ProductionReady && !paused && doctorChecksReady(checks) && remoteReady &&
		receipt.StateAfter == controller.StateBranchHeadSHA && receipt.Conclusion == "success" && receipt.Error == ""
	output := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": ready, "checks": checks, "hosted_controller": controller,
		"remote_doctor": remote, "operation_receipt": receipt}
	if inspectErr != nil {
		output["error"] = inspectErr.Error()
	}
	return output, checks, ready
}

func runHostedReadOperation(config integrated.Config, operation, githubTokenEnv, githubAPIURL string) (hostedstatus.OperationResult, hostedstatus.Result, error) {
	var receipt hostedstatus.OperationResult
	var controller hostedstatus.Result
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		return receipt, controller, errors.New("GitHub authorization is required for hosted status")
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return receipt, controller, errors.New("hosted repository identity is invalid")
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return receipt, controller, errors.New("generate hosted read request identity")
	}
	requestID := "cli-" + operation + "-" + hex.EncodeToString(random[:])
	return executeHostedControllerReadOperation(ctx, client.GoGitHub(), config, operation, requestID, time.Now().UTC())
}

// executeHostedControllerReadOperation performs an exact status/doctor read
// against the supplied release-bound config without writing terminal output.
// Callers may persist requestID and issuedAt before invocation for crash-safe
// hosted release transitions.
func executeHostedControllerReadOperation(ctx context.Context, client *gh.Client, config integrated.Config, operation, requestID string, issuedAt time.Time) (hostedstatus.OperationResult, hostedstatus.Result, error) {
	var receipt hostedstatus.OperationResult
	var controller hostedstatus.Result
	if ctx == nil || client == nil {
		return receipt, controller, errors.New("hosted controller client is required")
	}
	if config.ExecutionMode != integrated.ExecutionHosted {
		return receipt, controller, errors.New("installation uses the local runtime")
	}
	if operation != "status" && operation != "doctor" {
		return receipt, controller, errors.New("hosted read operation is invalid")
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return receipt, controller, errors.New("hosted request ID is invalid")
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return receipt, controller, errors.New("hosted repository identity is invalid")
	}
	branch, _, err := client.Repositories.GetBranch(ctx, owner, repository, config.DefaultBranch, 0)
	if err != nil || branch.GetCommit() == nil {
		return receipt, controller, errors.New("read exact hosted default-branch head")
	}
	head := strings.ToLower(strings.TrimSpace(branch.GetCommit().GetSHA()))
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(head) {
		return receipt, controller, errors.New("hosted default-branch head is invalid")
	}
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	_, err = client.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, gh.CreateWorkflowDispatchEventRequest{
		Ref: config.DefaultBranch, Inputs: map[string]interface{}{"operation": operation, "request_id": requestID},
	})
	if err != nil {
		return receipt, controller, fmt.Errorf("dispatch hosted %s (the request may have been accepted): %w", operation, err)
	}
	receipt, err = waitForHostedReadOperation(ctx, client, config, operation, requestID, head, issuedAt)
	if err != nil {
		return receipt, controller, err
	}
	controller, err = inspectHostedControllerStatus(ctx, client, config, time.Now().UTC())
	if err != nil {
		return receipt, controller, err
	}
	if receipt.StateAfter != controller.StateBranchHeadSHA {
		return receipt, controller, errors.New("hosted state advanced after the exact read receipt; rerun the command")
	}
	if receipt.Error != "" || receipt.Conclusion != "success" {
		if receipt.Error != "" {
			return receipt, controller, errors.New(receipt.Error)
		}
		return receipt, controller, errors.New("hosted read workflow did not conclude successfully")
	}
	return receipt, controller, nil
}

func decodeHostedReadDocument(raw json.RawMessage, schema string) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, errors.New("hosted read result is empty or oversized")
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("decode hosted read result")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || document["schema_version"] != schema {
		return nil, errors.New("hosted read result has trailing content or the wrong schema")
	}
	if _, ok := document["production_ready"].(bool); !ok {
		return nil, errors.New("hosted read result lacks an explicit production readiness value")
	}
	return document, nil
}

func hostedConfig(stateDir string) (integrated.Config, bool, error) {
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return integrated.Config{}, false, err
	}
	config, err := store.Load()
	if err != nil {
		return integrated.Config{}, false, err
	}
	return config, config.ExecutionMode == integrated.ExecutionHosted, nil
}

func dispatchHostedOperation(stateDir, operation, requestID, githubTokenEnv, githubAPIURL string, jsonOutput bool) int {
	config, hosted, err := hostedConfig(stateDir)
	if err != nil {
		return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Operation: operation}, jsonOutput, err)
	}
	if !hosted {
		return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository, Operation: operation}, jsonOutput, errors.New("installation uses the local runtime"))
	}
	if requestID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return emitHostedDispatchFailure(hostedDispatchResult{SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository, Operation: operation}, jsonOutput, errors.New("generate hosted request identity"))
		}
		requestID = "cli-" + operation + "-" + hex.EncodeToString(random[:])
	}
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		result := newHostedDispatchResult(config, operation, requestID)
		return emitHostedDispatchFailure(result, jsonOutput, errors.New("GitHub authorization is required"))
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := executeHostedControllerOperation(ctx, client.GoGitHub(), config, operation, requestID, time.Now().UTC())
	if err != nil {
		return emitHostedDispatchFailure(result, jsonOutput, err)
	}
	if jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Hosted Hive %s completed for %s (request %s).\n", operation, config.Repository, requestID)
	return 0
}

func newHostedDispatchResult(config integrated.Config, operation, requestID string) hostedDispatchResult {
	return hostedDispatchResult{
		SchemaVersion: "hive.hosted-dispatch.v1", Repository: config.Repository,
		Workflow: integrated.HostedControllerWorkflowPath, Ref: config.DefaultBranch,
		Operation: operation, RequestID: requestID,
	}
}

// executeHostedControllerOperation performs one exact controller mutation for
// an explicitly supplied release-bound config. It deliberately performs no
// terminal output so a hosted release transition can checkpoint and verify the
// target release before promoting it to the active local config.
func executeHostedControllerOperation(ctx context.Context, client *gh.Client, config integrated.Config, operation, requestID string, issuedAt time.Time) (hostedDispatchResult, error) {
	result := newHostedDispatchResult(config, operation, requestID)
	if ctx == nil || client == nil {
		return result, errors.New("hosted controller client is required")
	}
	if config.ExecutionMode != integrated.ExecutionHosted {
		return result, errors.New("installation uses the local runtime")
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return result, errors.New("hosted request ID is invalid")
	}
	if operation != "cycle" && operation != "pause" && operation != "resume" {
		return result, errors.New("hosted operation is invalid")
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return result, errors.New("hosted repository identity is invalid")
	}
	defaultBranch, _, err := client.Repositories.GetBranch(ctx, owner, repository, config.DefaultBranch, 0)
	if err != nil || defaultBranch.GetCommit() == nil {
		return result, errors.New("read exact hosted default-branch head")
	}
	head := strings.ToLower(strings.TrimSpace(defaultBranch.GetCommit().GetSHA()))
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(head) {
		return result, errors.New("hosted default-branch head is invalid")
	}
	if issuedAt.IsZero() {
		issuedAt = time.Now().UTC()
	}
	_, err = client.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, gh.CreateWorkflowDispatchEventRequest{
		Ref: config.DefaultBranch,
		Inputs: map[string]interface{}{
			"operation":  operation,
			"request_id": requestID,
		},
	})
	if err != nil {
		result.RetryCommand = fmt.Sprintf("rerun the same command with --request-id %s", requestID)
		return result, fmt.Errorf("dispatch hosted controller (the request may have been accepted; retry only with the same request ID): %w", err)
	}
	result.Accepted = true
	receipt, err := waitForHostedControllerOperation(ctx, client, config, operation, requestID, head, issuedAt)
	if err != nil {
		result.RetryCommand = fmt.Sprintf("rerun the same command with --request-id %s", requestID)
		return result, err
	}
	result.Receipt = &receipt
	if receipt.Error != "" || receipt.Conclusion != "success" {
		message := receipt.Error
		if message == "" {
			message = "hosted controller operation did not conclude successfully"
		}
		return result, errors.New(message)
	}
	result.Completed = true
	return result, nil
}

func runHostedOperatorCommand(stateDir, operation, requestID, githubTokenEnv, githubAPIURL string, arguments any, jsonOutput bool) int {
	output := hostedOperatorCommandResult{SchemaVersion: "hive.hosted-operator-command.v1", Operation: operation, RequestID: requestID}
	config, hosted, err := hostedConfig(stateDir)
	if err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	output.Repository = config.Repository
	if !hosted || !isHostedOperatorOperation(operation) {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("installation or operation is not hosted operator state"))
	}
	if requestID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return emitHostedOperatorCommand(output, jsonOutput, errors.New("generate hosted request identity"))
		}
		requestID = "cli-" + operation + "-" + hex.EncodeToString(random[:])
		output.RequestID = requestID
	}
	if !hostedRequestPattern.MatchString(requestID) {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("hosted request ID is invalid"))
	}
	token := resolveGitHubToken(githubTokenEnv)
	if token == "" {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("GitHub authorization is required"))
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("hosted repository identity is invalid"))
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	actor, err := client.AuthenticatedNumericUser(ctx)
	if err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	permission, _, err := client.GoGitHub().Repositories.GetPermissionLevel(ctx, owner, repository, actor.Login)
	if err != nil || !hostedCommandPermissionAllowed(permission.GetPermission()) {
		if err == nil {
			err = errors.New("hosted operator requires current write, maintain, or administrator repository permission")
		}
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	defaultBranch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, repository, config.DefaultBranch, 0)
	if err != nil || defaultBranch.GetCommit() == nil || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(strings.ToLower(defaultBranch.GetCommit().GetSHA())) {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("read exact hosted default-branch head"))
	}
	stateRef, _, err := client.GoGitHub().Git.GetRef(ctx, owner, repository, "refs/heads/"+config.HostedStateBranch)
	if err != nil || stateRef.GetObject() == nil || stateRef.GetObject().GetType() != "commit" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(strings.ToLower(stateRef.GetObject().GetSHA())) {
		return emitHostedOperatorCommand(output, jsonOutput, errors.New("read exact hosted state-branch head"))
	}
	argumentBytes, err := marshalHostedArguments(arguments)
	if err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	request, encoded, digest, created, err := loadOrCreateHostedOutboundRequest(stateDir, config, operation, requestID,
		integrated.HostedOperatorIdentity{ID: actor.ID, Login: actor.Login}, argumentBytes,
		strings.ToLower(defaultBranch.GetCommit().GetSHA()), strings.ToLower(stateRef.GetObject().GetSHA()), time.Now().UTC())
	if err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	output.RequestSHA256 = digest
	if !created {
		receipt, waitErr := hostedstatus.WaitForOperation(ctx, client.GoGitHub(), config, operation, requestID, digest, request.IssuedAt)
		if waitErr == nil {
			output.Accepted = true
			return finishHostedOperatorCommand(stateDir, config, output, receipt, jsonOutput)
		}
		if !errors.Is(waitErr, hostedstatus.ErrOperationRunNotFound) && !errors.Is(waitErr, hostedstatus.ErrOperationRunTerminatedWithoutReceipt) {
			return emitHostedOperatorCommand(output, jsonOutput, waitErr)
		}
		remoteBound, remoteErr := verifyRemoteHostedOperatorRequest(config, operation, requestID, digest,
			integrated.HostedOperatorIdentity{ID: actor.ID, Login: actor.Login}, githubTokenEnv, githubAPIURL)
		if remoteErr != nil && errors.Is(waitErr, hostedstatus.ErrOperationRunTerminatedWithoutReceipt) {
			return emitHostedOperatorCommand(output, jsonOutput, fmt.Errorf("%v; authoritative hosted request recovery failed: %w", waitErr, remoteErr))
		}
		if remoteErr != nil && !errors.Is(waitErr, hostedstatus.ErrOperationRunNotFound) {
			return emitHostedOperatorCommand(output, jsonOutput, remoteErr)
		}
		if remoteBound {
			// The current exact status receipt proves the HMAC-authenticated
			// durable request binding. Redispatching the identical locally
			// persisted bytes resumes or replays it without fresh authority.
			goto dispatchExactHostedOperator
		}
		if errors.Is(waitErr, hostedstatus.ErrOperationRunTerminatedWithoutReceipt) {
			return emitHostedOperatorCommand(output, jsonOutput, errors.New("exact hosted run terminated without a receipt and current hosted status does not prove the request binding"))
		}
		freshDefault, _, defaultErr := client.GoGitHub().Repositories.GetBranch(ctx, owner, repository, config.DefaultBranch, 0)
		freshState, _, stateErr := client.GoGitHub().Git.GetRef(ctx, owner, repository, "refs/heads/"+config.HostedStateBranch)
		if defaultErr != nil || stateErr != nil || freshDefault.GetCommit() == nil || freshState.GetObject() == nil ||
			time.Now().UTC().After(request.ExpiresAt) || request.DefaultHeadSHA != strings.ToLower(freshDefault.GetCommit().GetSHA()) ||
			request.ExpectedStateHeadSHA != strings.ToLower(freshState.GetObject().GetSHA()) {
			return emitHostedOperatorCommand(output, jsonOutput, errors.New("stored hosted request has expired or its bound repository state advanced; use a new request ID after reconciliation"))
		}
	}

dispatchExactHostedOperator:
	_, err = client.GoGitHub().Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repository, integrated.HostedControllerWorkflowPath, gh.CreateWorkflowDispatchEventRequest{
		Ref: config.DefaultBranch, Inputs: map[string]interface{}{"operation": operation, "request_id": requestID, "request_payload": encoded, "request_sha256": digest},
	})
	if err != nil {
		output.RetryCommand = fmt.Sprintf("rerun the same command with --request-id %s before %s", requestID, request.ExpiresAt.Format(time.RFC3339))
		return emitHostedOperatorCommand(output, jsonOutput, fmt.Errorf("dispatch hosted operator (the request may have been accepted): %w", err))
	}
	output.Accepted = true
	receipt, err := hostedstatus.WaitForOperation(ctx, client.GoGitHub(), config, operation, requestID, digest, request.IssuedAt)
	if err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, err)
	}
	return finishHostedOperatorCommand(stateDir, config, output, receipt, jsonOutput)
}

func verifyRemoteHostedOperatorRequest(config integrated.Config, operation, requestID, digest string, actor integrated.HostedOperatorIdentity, githubTokenEnv, githubAPIURL string) (bool, error) {
	receipt, _, err := runHostedReadOperation(config, "status", githubTokenEnv, githubAPIURL)
	if err != nil {
		return false, err
	}
	remote, err := decodeHostedReadDocument(receipt.StatusResult, "hive.lifecycle-status.v1")
	if err != nil {
		return false, err
	}
	rawSummary, ok := remote["hosted_operator_requests"]
	if !ok {
		return false, errors.New("authoritative hosted status omitted operator request state")
	}
	data, err := json.Marshal(rawSummary)
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return false, errors.New("authoritative hosted operator request state is invalid or oversized")
	}
	var summary hostedRemoteOperatorLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&summary); err != nil {
		return false, errors.New("decode authoritative hosted operator request state")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || summary.SchemaVersion != hostedOperatorLedgerSchema ||
		summary.Pending != len(summary.PendingRequests) || summary.Terminal != len(summary.TerminalRequests) ||
		summary.Retained != summary.Pending+summary.Terminal || summary.Capacity != hostedOperatorLedgerMaxItems || summary.Retained > summary.Capacity {
		return false, errors.New("authoritative hosted operator request summary has invalid identity or counts")
	}
	matches := 0
	for _, record := range append(append([]hostedRemoteOperatorRequest(nil), summary.PendingRequests...), summary.TerminalRequests...) {
		if record.RequestID != requestID {
			continue
		}
		matches++
		if record.Operation != operation || record.RequestSHA256 != digest || record.Actor.ID != actor.ID ||
			!strings.EqualFold(record.Actor.Login, actor.Login) || record.AcceptedAt.IsZero() || record.AcceptedSequence == 0 {
			return false, errors.New("authoritative hosted request ID is bound to different bytes, operation, actor, or sequence")
		}
		if record.Status != "" && record.Status != "succeeded" && record.Status != "failed" {
			return false, errors.New("authoritative hosted terminal request has an invalid status")
		}
	}
	if matches > 1 {
		return false, errors.New("authoritative hosted status contains duplicate operator request identities")
	}
	return matches == 1, nil
}

func finishHostedOperatorCommand(stateDir string, config integrated.Config, output hostedOperatorCommandResult, receipt hostedstatus.OperationResult, jsonOutput bool) int {
	output.Receipt = &receipt
	if len(receipt.OperatorResult) != 0 {
		output.Result = receipt.OperatorResult
	}
	if err := markHostedOutboundTerminal(stateDir, config, output.RequestID, output.RequestSHA256, receipt, time.Now().UTC()); err != nil {
		return emitHostedOperatorCommand(output, jsonOutput, fmt.Errorf("persist exact hosted operator terminal receipt: %w", err))
	}
	if receipt.Error != "" || receipt.Conclusion != "success" {
		if receipt.Error == "" {
			receipt.Error = "hosted operator workflow did not conclude successfully"
		}
		return emitHostedOperatorCommand(output, jsonOutput, errors.New(receipt.Error))
	}
	if jsonOutput {
		return encodeJSON(output)
	}
	fmt.Printf("Hosted Hive %s completed for %s (request %s).\n", output.Operation, output.Repository, output.RequestID)
	return 0
}

func hostedCommandPermissionAllowed(permission string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "admin", "maintain", "write", "push":
		return true
	default:
		return false
	}
}

func emitHostedOperatorCommand(output hostedOperatorCommandResult, jsonOutput bool, err error) int {
	if err != nil {
		output.Error = err.Error()
	}
	if jsonOutput {
		_ = encodeJSON(output)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "Hosted Hive operator request failed:", err)
	}
	if err != nil {
		return 1
	}
	return 0
}

func emitHostedDispatchFailure(result hostedDispatchResult, jsonOutput bool, err error) int {
	result.Error = err.Error()
	if jsonOutput {
		_ = encodeJSON(result)
	} else {
		fmt.Println("Hosted Hive request failed:", err)
	}
	return 1
}

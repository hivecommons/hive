package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/hostedstate"
	"github.com/kubestellar/hive/pkg/integrated"
)

var hostedOperatorCommitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

const (
	hostedOperatorLedgerSchema    = "hive.hosted-operator-requests.v1"
	hostedOperatorTerminalSchema  = "hive.hosted-operator-terminal.v1"
	hostedOperatorLedgerFile      = "hosted-operator-requests.json"
	hostedOperatorLedgerMaxBytes  = 16 << 20
	hostedOperatorResultMaxBytes  = 256 << 10
	hostedOperatorLedgerMaxItems  = 512
	hostedOperatorExpiredRetained = 32
	hostedOperatorErrorMaxBytes   = 4096
	hostedOperatorLedgerDomain    = "hive.hosted-operator-requests.v1\x00"
)

type hostedOperatorLedger struct {
	SchemaVersion string                       `json:"schema_version"`
	Repository    string                       `json:"repository"`
	RepositoryID  string                       `json:"repository_id"`
	StateBranch   string                       `json:"state_branch"`
	Requests      []hostedOperatorLedgerRecord `json:"requests"`
	Signature     string                       `json:"signature"`
}

type hostedOperatorLedgerUnsigned struct {
	SchemaVersion string                       `json:"schema_version"`
	Repository    string                       `json:"repository"`
	RepositoryID  string                       `json:"repository_id"`
	StateBranch   string                       `json:"state_branch"`
	Requests      []hostedOperatorLedgerRecord `json:"requests"`
}

type hostedOperatorControllerIdentity struct {
	RunID        uint64 `json:"run_id"`
	RunAttempt   uint64 `json:"run_attempt"`
	WorkflowPath string `json:"workflow_path"`
	WorkflowSHA  string `json:"workflow_sha"`
}

type hostedOperatorLedgerRecord struct {
	RequestID                string                            `json:"request_id"`
	Operation                string                            `json:"operation"`
	RequestSHA256            string                            `json:"request_sha256"`
	RequestBase64            string                            `json:"request_base64"`
	Actor                    integrated.HostedOperatorIdentity `json:"actor"`
	Status                   string                            `json:"status"`
	AcceptedAt               time.Time                         `json:"accepted_at"`
	AcceptedPriorStateCommit string                            `json:"accepted_prior_state_commit"`
	AcceptedSequence         uint64                            `json:"accepted_sequence"`
	AcceptedController       hostedOperatorControllerIdentity  `json:"accepted_controller"`
	TerminalAt               time.Time                         `json:"terminal_at,omitempty"`
	TerminalSequence         uint64                            `json:"terminal_sequence,omitempty"`
	TerminalController       *hostedOperatorControllerIdentity `json:"terminal_controller,omitempty"`
	TerminalSHA256           string                            `json:"terminal_sha256,omitempty"`
	TerminalBase64           string                            `json:"terminal_base64,omitempty"`
}

type hostedOperatorTerminal struct {
	SchemaVersion string          `json:"schema_version"`
	Outcome       string          `json:"outcome"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type hostedOperatorPreparation struct {
	New              bool
	Resume           bool
	Terminal         bool
	Status           string
	Result           json.RawMessage
	Error            string
	TerminalSHA256   string
	TerminalSequence uint64
}

func hostedOperatorLedgerStatus(root string, secret []byte, config integrated.Config) (map[string]any, bool, error) {
	ledger, err := loadHostedOperatorLedger(root, secret, config)
	if err != nil {
		return nil, false, err
	}
	pending := make([]map[string]any, 0)
	terminal := make([]map[string]any, 0)
	for _, record := range ledger.Requests {
		if record.Status == "accepted" {
			pending = append(pending, map[string]any{
				"request_id": record.RequestID, "operation": record.Operation, "request_sha256": record.RequestSHA256,
				"actor": record.Actor, "accepted_at": record.AcceptedAt, "accepted_sequence": record.AcceptedSequence,
			})
		} else {
			terminal = append(terminal, map[string]any{
				"request_id": record.RequestID, "operation": record.Operation, "request_sha256": record.RequestSHA256,
				"actor": record.Actor, "status": record.Status, "accepted_at": record.AcceptedAt,
				"accepted_sequence": record.AcceptedSequence, "terminal_at": record.TerminalAt,
				"terminal_sequence": record.TerminalSequence, "terminal_sha256": record.TerminalSHA256,
			})
		}
	}
	return map[string]any{
		"schema_version": hostedOperatorLedgerSchema, "pending": len(pending), "terminal": len(terminal),
		"retained": len(ledger.Requests), "capacity": hostedOperatorLedgerMaxItems,
		"pending_requests": pending, "terminal_requests": terminal,
	}, len(pending) != 0, nil
}

func prepareHostedOperatorRequest(root string, secret []byte, config integrated.Config, request hostedOperationRequest, requestBytes []byte, requestSHA256 string, actor integrated.HostedOperatorIdentity, stateHead string, sequence uint64, controller hostedstate.ControllerIdentity, now time.Time) (hostedOperatorPreparation, error) {
	var result hostedOperatorPreparation
	ledger, err := loadHostedOperatorLedger(root, secret, config)
	if err != nil {
		return result, err
	}
	encodedRequest := base64.RawStdEncoding.EncodeToString(requestBytes)
	for _, record := range ledger.Requests {
		if record.RequestID != request.RequestID {
			continue
		}
		if record.Operation != request.Operation || record.RequestSHA256 != requestSHA256 || record.RequestBase64 != encodedRequest ||
			record.Actor.ID != actor.ID || !strings.EqualFold(record.Actor.Login, actor.Login) || record.AcceptedPriorStateCommit != request.ExpectedStateHeadSHA {
			return result, errors.New("hosted operator request ID is already bound to different bytes, operation, actor, or prior state")
		}
		switch record.Status {
		case "accepted":
			result.Resume, result.Status = true, record.Status
			return result, nil
		case "succeeded", "failed":
			terminal, err := decodeHostedOperatorTerminal(record)
			if err != nil {
				return result, err
			}
			result.Terminal, result.Status = true, record.Status
			result.Result, result.Error = terminal.Result, terminal.Error
			result.TerminalSHA256, result.TerminalSequence = record.TerminalSHA256, record.TerminalSequence
			return result, nil
		default:
			return result, errors.New("hosted operator ledger contains an invalid request status")
		}
	}
	for _, record := range ledger.Requests {
		if record.Status == "accepted" {
			return result, fmt.Errorf("hosted operator request %s (%s) is already accepted; resume that exact request before starting another", record.RequestID, record.Operation)
		}
	}
	ledger.Requests = compactHostedOperatorLedger(ledger.Requests, now.UTC())
	if len(ledger.Requests) >= hostedOperatorLedgerMaxItems {
		return result, errors.New("hosted operator ledger reached its fail-closed retention bound")
	}
	if err := validateHostedOperationRequestFresh(request, stateHead, osEnv("GITHUB_SHA"), now); err != nil {
		return result, err
	}
	record := hostedOperatorLedgerRecord{
		RequestID: request.RequestID, Operation: request.Operation, RequestSHA256: requestSHA256, RequestBase64: encodedRequest,
		Actor: actor, Status: "accepted", AcceptedAt: now.UTC(), AcceptedPriorStateCommit: stateHead, AcceptedSequence: sequence + 1,
		AcceptedController: hostedOperatorControllerIdentityFrom(controller),
	}
	ledger.Requests = append(ledger.Requests, record)
	if err := saveHostedOperatorLedger(root, secret, ledger); err != nil {
		return result, err
	}
	result.New, result.Status = true, record.Status
	return result, nil
}

func finishHostedOperatorRequest(root string, secret []byte, config integrated.Config, request hostedOperationRequest, requestSHA256 string, actor integrated.HostedOperatorIdentity, operator any, operationErr error, sequence uint64, controller hostedstate.ControllerIdentity, now time.Time) (hostedOperatorPreparation, error) {
	var result hostedOperatorPreparation
	ledger, err := loadHostedOperatorLedger(root, secret, config)
	if err != nil {
		return result, err
	}
	index := -1
	for candidate := range ledger.Requests {
		record := ledger.Requests[candidate]
		if record.RequestID == request.RequestID {
			if record.Operation != request.Operation || record.RequestSHA256 != requestSHA256 || record.Actor.ID != actor.ID || !strings.EqualFold(record.Actor.Login, actor.Login) {
				return result, errors.New("hosted operator terminal result does not match its accepted request binding")
			}
			index = candidate
			break
		}
	}
	if index < 0 || ledger.Requests[index].Status != "accepted" {
		return result, errors.New("hosted operator ledger lost its accepted request")
	}
	var resultBytes json.RawMessage
	if operator != nil {
		encoded, err := json.Marshal(operator)
		if err != nil || len(encoded) == 0 || len(encoded) > hostedOperatorResultMaxBytes {
			return result, errors.New("hosted operator typed result is invalid or oversized")
		}
		resultBytes = encoded
	}
	terminal := hostedOperatorTerminal{SchemaVersion: hostedOperatorTerminalSchema, Outcome: "succeeded", Result: resultBytes}
	if operationErr != nil {
		terminal.Outcome = "failed"
		terminal.Error = operationErr.Error()
		if len(terminal.Error) == 0 || len(terminal.Error) > hostedOperatorErrorMaxBytes {
			return result, errors.New("hosted operator terminal error is empty or oversized")
		}
	} else if len(resultBytes) == 0 {
		return result, errors.New("successful hosted operator request returned no typed result")
	}
	terminalBytes, err := json.Marshal(terminal)
	if err != nil || len(terminalBytes) == 0 || len(terminalBytes) > hostedOperatorResultMaxBytes+hostedOperatorErrorMaxBytes {
		return result, errors.New("encode hosted operator terminal result")
	}
	digest := sha256.Sum256(terminalBytes)
	record := &ledger.Requests[index]
	record.Status, record.TerminalAt, record.TerminalSequence = terminal.Outcome, now.UTC(), sequence+1
	record.TerminalController = ptrHostedOperatorControllerIdentity(hostedOperatorControllerIdentityFrom(controller))
	record.TerminalSHA256, record.TerminalBase64 = hex.EncodeToString(digest[:]), base64.RawStdEncoding.EncodeToString(terminalBytes)
	ledger.Requests = compactHostedOperatorLedger(ledger.Requests, now.UTC())
	if err := saveHostedOperatorLedger(root, secret, ledger); err != nil {
		return result, err
	}
	result.Status, result.Result, result.Error = record.Status, resultBytes, terminal.Error
	result.TerminalSHA256, result.TerminalSequence = record.TerminalSHA256, record.TerminalSequence
	return result, nil
}

func compactHostedOperatorLedger(records []hostedOperatorLedgerRecord, now time.Time) []hostedOperatorLedgerRecord {
	active := make([]hostedOperatorLedgerRecord, 0, 1)
	unexpired := make([]hostedOperatorLedgerRecord, 0, len(records))
	expired := make([]hostedOperatorLedgerRecord, 0, len(records))
	for _, record := range records {
		if record.Status == "accepted" {
			active = append(active, record)
			continue
		}
		if now.Before(record.AcceptedAt.Add(hostedOperationLifetime)) {
			unexpired = append(unexpired, record)
		} else {
			expired = append(expired, record)
		}
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].TerminalSequence == expired[j].TerminalSequence {
			return expired[i].RequestID < expired[j].RequestID
		}
		return expired[i].TerminalSequence < expired[j].TerminalSequence
	})
	if len(expired) > hostedOperatorExpiredRetained {
		expired = expired[len(expired)-hostedOperatorExpiredRetained:]
	}
	result := make([]hostedOperatorLedgerRecord, 0, len(unexpired)+len(expired)+len(active))
	result = append(result, expired...)
	result = append(result, unexpired...)
	result = append(result, active...)
	return result
}

func loadHostedOperatorLedger(root string, secret []byte, config integrated.Config) (hostedOperatorLedger, error) {
	path := filepath.Join(root, "integrated", hostedOperatorLedgerFile)
	data, err := readBoundedRegularFile(path, hostedOperatorLedgerMaxBytes)
	if os.IsNotExist(err) {
		return hostedOperatorLedger{SchemaVersion: hostedOperatorLedgerSchema, Repository: config.Repository, RepositoryID: config.RepositoryID, StateBranch: config.HostedStateBranch}, nil
	}
	if err != nil {
		return hostedOperatorLedger{}, err
	}
	var ledger hostedOperatorLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return hostedOperatorLedger{}, fmt.Errorf("decode hosted operator ledger: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return hostedOperatorLedger{}, errors.New("hosted operator ledger has trailing content")
	}
	if ledger.SchemaVersion != hostedOperatorLedgerSchema || ledger.Repository != config.Repository || ledger.RepositoryID != config.RepositoryID ||
		ledger.StateBranch != config.HostedStateBranch || len(ledger.Requests) > hostedOperatorLedgerMaxItems || !hostedOperationDigestPattern.MatchString(ledger.Signature) {
		return hostedOperatorLedger{}, errors.New("hosted operator ledger identity, size, or signature is invalid")
	}
	expected, err := signHostedOperatorLedger(secret, ledger)
	if err != nil || !hmac.Equal([]byte(expected), []byte(ledger.Signature)) {
		return hostedOperatorLedger{}, errors.New("hosted operator ledger authentication failed")
	}
	seen := map[string]bool{}
	for _, record := range ledger.Requests {
		if seen[record.RequestID] {
			return hostedOperatorLedger{}, errors.New("hosted operator ledger contains a duplicate request ID")
		}
		seen[record.RequestID] = true
		if err := validateHostedOperatorLedgerRecord(record); err != nil {
			return hostedOperatorLedger{}, err
		}
	}
	return ledger, nil
}

func saveHostedOperatorLedger(root string, secret []byte, ledger hostedOperatorLedger) error {
	ledger.Signature = ""
	signature, err := signHostedOperatorLedger(secret, ledger)
	if err != nil {
		return err
	}
	ledger.Signature = signature
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil || len(data) > hostedOperatorLedgerMaxBytes {
		return errors.New("hosted operator ledger exceeds its strict encoded bound")
	}
	directory := filepath.Join(root, "integrated")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".hosted-operator-requests-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(directory, hostedOperatorLedgerFile))
}

func signHostedOperatorLedger(secret []byte, ledger hostedOperatorLedger) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("hosted operator ledger authentication key is invalid")
	}
	unsigned := hostedOperatorLedgerUnsigned{SchemaVersion: ledger.SchemaVersion, Repository: ledger.Repository, RepositoryID: ledger.RepositoryID, StateBranch: ledger.StateBranch, Requests: ledger.Requests}
	data, err := json.Marshal(unsigned)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(hostedOperatorLedgerDomain))
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func validateHostedOperatorLedgerRecord(record hostedOperatorLedgerRecord) error {
	requestBytes, err := base64.RawStdEncoding.DecodeString(record.RequestBase64)
	if err != nil || len(requestBytes) == 0 || len(requestBytes) > hostedOperationMaxBytes || !hostedRequestPattern.MatchString(record.RequestID) ||
		!isHostedOperatorOperation(record.Operation) || !hostedOperationDigestPattern.MatchString(record.RequestSHA256) || record.Actor.ID <= 0 || strings.TrimSpace(record.Actor.Login) == "" ||
		record.AcceptedAt.IsZero() || !hostedOperatorCommitPattern.MatchString(record.AcceptedPriorStateCommit) || record.AcceptedSequence == 0 || !validHostedOperatorController(record.AcceptedController) {
		return errors.New("hosted operator ledger contains an invalid accepted request")
	}
	digest := sha256.Sum256(requestBytes)
	if hex.EncodeToString(digest[:]) != record.RequestSHA256 {
		return errors.New("hosted operator ledger request digest does not match its exact bytes")
	}
	switch record.Status {
	case "accepted":
		if !record.TerminalAt.IsZero() || record.TerminalSequence != 0 || record.TerminalController != nil || record.TerminalSHA256 != "" || record.TerminalBase64 != "" {
			return errors.New("accepted hosted operator request contains terminal fields")
		}
	case "succeeded", "failed":
		if record.TerminalAt.IsZero() || record.TerminalSequence <= record.AcceptedSequence || record.TerminalController == nil || !validHostedOperatorController(*record.TerminalController) {
			return errors.New("terminal hosted operator request lacks its controller or sequence binding")
		}
		if _, err := decodeHostedOperatorTerminal(record); err != nil {
			return err
		}
	default:
		return errors.New("hosted operator ledger request status is invalid")
	}
	return nil
}

func decodeHostedOperatorTerminal(record hostedOperatorLedgerRecord) (hostedOperatorTerminal, error) {
	var terminal hostedOperatorTerminal
	if !hostedOperationDigestPattern.MatchString(record.TerminalSHA256) {
		return terminal, errors.New("hosted operator terminal digest is invalid")
	}
	data, err := base64.RawStdEncoding.DecodeString(record.TerminalBase64)
	if err != nil || len(data) == 0 || len(data) > hostedOperatorResultMaxBytes+hostedOperatorErrorMaxBytes {
		return terminal, errors.New("hosted operator terminal encoding is invalid or oversized")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != record.TerminalSHA256 {
		return terminal, errors.New("hosted operator terminal digest does not match its exact bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&terminal); err != nil {
		return terminal, errors.New("decode hosted operator terminal result")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || terminal.SchemaVersion != hostedOperatorTerminalSchema || terminal.Outcome != record.Status {
		return terminal, errors.New("hosted operator terminal result has trailing content or the wrong identity")
	}
	if terminal.Outcome == "succeeded" && (len(terminal.Result) == 0 || terminal.Error != "") || terminal.Outcome == "failed" && (terminal.Error == "" || len(terminal.Error) > hostedOperatorErrorMaxBytes) {
		return terminal, errors.New("hosted operator terminal outcome, result, or error is invalid")
	}
	return terminal, nil
}

func hostedOperatorControllerIdentityFrom(controller hostedstate.ControllerIdentity) hostedOperatorControllerIdentity {
	return hostedOperatorControllerIdentity{RunID: controller.RunID, RunAttempt: controller.Attempt, WorkflowPath: controller.WorkflowPath, WorkflowSHA: controller.WorkflowSHA}
}

func ptrHostedOperatorControllerIdentity(value hostedOperatorControllerIdentity) *hostedOperatorControllerIdentity {
	return &value
}

func validHostedOperatorController(value hostedOperatorControllerIdentity) bool {
	return value.RunID > 0 && value.RunAttempt == 1 && value.WorkflowPath == integrated.HostedControllerWorkflowPath && hostedOperatorCommitPattern.MatchString(value.WorkflowSHA)
}

func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("hosted operator ledger is linked, non-regular, empty, or oversized")
	}
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	opened, err := handle.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("hosted operator ledger changed before open")
	}
	data, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("hosted operator ledger could not be read within its strict bound")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("hosted operator ledger changed while read")
	}
	return data, nil
}

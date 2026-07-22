package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/hostedstatus"
	"github.com/kubestellar/hive/v2/pkg/integrated"
)

const (
	hostedOutboundSchema          = "hive.hosted-outbound-requests.v2"
	hostedOutboundLegacySchema    = "hive.hosted-outbound-requests.v1"
	hostedOutboundFile            = "hosted-outbound-requests.json"
	hostedOutboundMaxBytes        = 8 << 20
	hostedOutboundMaxItems        = 4096
	hostedOutboundRecentTerminals = 32
	hostedOutboundRetiredLimit    = 2048
	hostedOutboundRetiredLifetime = 90 * 24 * time.Hour
)

var errHostedOutboundTerminalRetired = errors.New("the exact hosted operator request completed and its full retry payload has been retired; use hosted status for its durable terminal receipt")
var hostedOutboundReleaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`)

type hostedOutboundLedger struct {
	SchemaVersion string                 `json:"schema_version"`
	Repository    string                 `json:"repository"`
	RepositoryID  string                 `json:"repository_id"`
	Requests      []hostedOutboundRecord `json:"requests"`
}

type hostedOutboundRecord struct {
	RequestID        string                            `json:"request_id"`
	Operation        string                            `json:"operation"`
	RequestSHA256    string                            `json:"request_sha256"`
	RequestPayload   string                            `json:"request_payload,omitempty"`
	ArgumentsSHA256  string                            `json:"arguments_sha256"`
	Actor            integrated.HostedOperatorIdentity `json:"actor"`
	IssuedAt         time.Time                         `json:"issued_at"`
	ExpiresAt        time.Time                         `json:"expires_at"`
	Release          hostedOperationRelease            `json:"release"`
	Status           string                            `json:"status"`
	TerminalAt       time.Time                         `json:"terminal_at,omitempty"`
	TerminalSequence uint64                            `json:"terminal_sequence,omitempty"`
	TerminalSHA256   string                            `json:"terminal_sha256,omitempty"`
}

func loadOrCreateHostedOutboundRequest(stateDir string, config integrated.Config, operation, requestID string, actor integrated.HostedOperatorIdentity,
	arguments json.RawMessage, defaultHead, stateHead string, now time.Time) (hostedOperationRequest, string, string, bool, error) {
	ledger, err := loadHostedOutboundLedger(stateDir, config)
	if err != nil {
		return hostedOperationRequest{}, "", "", false, err
	}
	argumentDigest := sha256.Sum256(arguments)
	argumentSHA256 := hex.EncodeToString(argumentDigest[:])
	for _, record := range ledger.Requests {
		if record.RequestID != requestID {
			continue
		}
		if record.Operation != operation || record.Actor.ID != actor.ID || !strings.EqualFold(record.Actor.Login, actor.Login) || record.ArgumentsSHA256 != argumentSHA256 ||
			record.Release.Version != config.HiveReleaseVersion || record.Release.HiveCommit != config.HiveCommit || record.Release.VisualHiveCommit != config.VisualHiveRef ||
			record.Release.DistributionManifestSHA256 != config.DistributionManifestSHA256 || record.Release.HostedControllerProtocol != config.HostedControllerProtocol {
			return hostedOperationRequest{}, "", "", false, errors.New("hosted request ID is already bound to different command bytes, actor, repository, or release")
		}
		if record.Status == "retired" {
			return hostedOperationRequest{}, "", "", false, errHostedOutboundTerminalRetired
		}
		request, _, err := decodeHostedOperationRequest(record.RequestPayload, record.RequestSHA256)
		if err != nil {
			return hostedOperationRequest{}, "", "", false, errors.New("stored hosted outbound request is invalid")
		}
		if request.Operation != operation || request.RequestID != requestID || !bytes.Equal(request.Arguments, arguments) ||
			request.Repository != config.Repository || request.RepositoryID != config.RepositoryID || request.WorkflowPath != integrated.HostedControllerWorkflowPath ||
			request.DefaultBranch != config.DefaultBranch || request.StateBranch != config.HostedStateBranch ||
			request.Release.Version != config.HiveReleaseVersion || request.Release.HiveCommit != config.HiveCommit || request.Release.VisualHiveCommit != config.VisualHiveRef ||
			request.Release.DistributionManifestSHA256 != config.DistributionManifestSHA256 || request.Release.HostedControllerProtocol != config.HostedControllerProtocol {
			return hostedOperationRequest{}, "", "", false, errors.New("hosted request ID is already bound to different command bytes, actor, repository, or release")
		}
		return request, record.RequestPayload, record.RequestSHA256, false, nil
	}
	ledger.Requests = compactHostedOutboundLedger(ledger.Requests, now.UTC())
	if len(ledger.Requests) >= hostedOutboundMaxItems {
		return hostedOperationRequest{}, "", "", false, errors.New("hosted outbound request journal reached its fail-closed retention bound")
	}
	request := hostedOperationRequest{
		SchemaVersion: hostedOperationRequestSchema, Repository: config.Repository, RepositoryID: config.RepositoryID,
		WorkflowPath: integrated.HostedControllerWorkflowPath, DefaultBranch: config.DefaultBranch,
		DefaultHeadSHA: strings.ToLower(defaultHead), StateBranch: config.HostedStateBranch,
		ExpectedStateHeadSHA: strings.ToLower(stateHead),
		Release: hostedOperationRelease{Version: config.HiveReleaseVersion, HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
			DistributionManifestSHA256: config.DistributionManifestSHA256, HostedControllerProtocol: config.HostedControllerProtocol},
		Operation: operation, RequestID: requestID, IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(hostedOperationLifetime), Actor: actor, Arguments: arguments,
	}
	encoded, digest, _, err := encodeHostedOperationRequest(request)
	if err != nil {
		return hostedOperationRequest{}, "", "", false, err
	}
	ledger.Requests = append(ledger.Requests, hostedOutboundRecord{
		RequestID: requestID, Operation: operation, RequestSHA256: digest, RequestPayload: encoded, Actor: actor,
		ArgumentsSHA256: argumentSHA256, IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt,
		Release: request.Release, Status: "pending",
	})
	if err := saveHostedOutboundLedger(stateDir, ledger); err != nil {
		return hostedOperationRequest{}, "", "", false, err
	}
	return request, encoded, digest, true, nil
}

func loadHostedOutboundLedger(stateDir string, config integrated.Config) (hostedOutboundLedger, error) {
	path := filepath.Join(stateDir, "integrated", hostedOutboundFile)
	data, err := readBoundedRegularFile(path, hostedOutboundMaxBytes)
	if os.IsNotExist(err) {
		return hostedOutboundLedger{SchemaVersion: hostedOutboundSchema, Repository: config.Repository, RepositoryID: config.RepositoryID}, nil
	}
	if err != nil {
		return hostedOutboundLedger{}, err
	}
	var identity struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		return hostedOutboundLedger{}, errors.New("decode hosted outbound request journal")
	}
	var ledger hostedOutboundLedger
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return hostedOutboundLedger{}, errors.New("decode hosted outbound request journal")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || (identity.SchemaVersion != hostedOutboundSchema && identity.SchemaVersion != hostedOutboundLegacySchema) ||
		ledger.Repository != config.Repository || ledger.RepositoryID != config.RepositoryID || len(ledger.Requests) > hostedOutboundMaxItems {
		return hostedOutboundLedger{}, errors.New("hosted outbound request journal identity, size, or trailing content is invalid")
	}
	ledger.SchemaVersion = hostedOutboundSchema
	seen := map[string]bool{}
	for index := range ledger.Requests {
		record := &ledger.Requests[index]
		if identity.SchemaVersion == hostedOutboundLegacySchema {
			request, _, decodeErr := decodeHostedOperationRequest(record.RequestPayload, record.RequestSHA256)
			if decodeErr != nil {
				return hostedOutboundLedger{}, errors.New("legacy hosted outbound request journal contains an invalid payload")
			}
			digest := sha256.Sum256(request.Arguments)
			record.ArgumentsSHA256 = hex.EncodeToString(digest[:])
			record.Release, record.Status = request.Release, "pending"
		}
		if seen[record.RequestID] || !hostedRequestPattern.MatchString(record.RequestID) || !isHostedOperatorOperation(record.Operation) ||
			!hostedOperationDigestPattern.MatchString(record.RequestSHA256) || !hostedOperationDigestPattern.MatchString(record.ArgumentsSHA256) ||
			record.Actor.ID <= 0 || strings.TrimSpace(record.Actor.Login) == "" || record.IssuedAt.IsZero() ||
			!record.ExpiresAt.Equal(record.IssuedAt.Add(hostedOperationLifetime)) || !validHostedOutboundRelease(record.Release) {
			return hostedOutboundLedger{}, errors.New("hosted outbound request journal contains an invalid or duplicate record")
		}
		seen[record.RequestID] = true
		switch record.Status {
		case "pending", "terminal":
			request, _, err := decodeHostedOperationRequest(record.RequestPayload, record.RequestSHA256)
			if err != nil || request.RequestID != record.RequestID || request.Operation != record.Operation || !request.IssuedAt.Equal(record.IssuedAt) ||
				!request.ExpiresAt.Equal(record.ExpiresAt) || request.Release != record.Release {
				return hostedOutboundLedger{}, errors.New("hosted outbound request journal record does not match its exact payload")
			}
			digest := sha256.Sum256(request.Arguments)
			if hex.EncodeToString(digest[:]) != record.ArgumentsSHA256 {
				return hostedOutboundLedger{}, errors.New("hosted outbound argument digest does not match its exact payload")
			}
			if record.Status == "pending" && (!record.TerminalAt.IsZero() || record.TerminalSequence != 0 || record.TerminalSHA256 != "") {
				return hostedOutboundLedger{}, errors.New("pending hosted outbound request contains terminal fields")
			}
		case "retired":
			if record.RequestPayload != "" {
				return hostedOutboundLedger{}, errors.New("retired hosted outbound request retained executable payload bytes")
			}
		default:
			return hostedOutboundLedger{}, errors.New("hosted outbound request journal contains an invalid status")
		}
		if record.Status != "pending" && (record.TerminalAt.IsZero() || record.TerminalSequence == 0 || !hostedOperationDigestPattern.MatchString(record.TerminalSHA256)) {
			return hostedOutboundLedger{}, errors.New("terminal hosted outbound request lacks its exact durable receipt binding")
		}
	}
	return ledger, nil
}

func validHostedOutboundRelease(release hostedOperationRelease) bool {
	return hostedOutboundReleaseTagPattern.MatchString(release.Version) && hostedOperatorCommitPattern.MatchString(strings.ToLower(release.HiveCommit)) &&
		hostedOperatorCommitPattern.MatchString(strings.ToLower(release.VisualHiveCommit)) && hostedOperationDigestPattern.MatchString(strings.ToLower(release.DistributionManifestSHA256)) &&
		release.HostedControllerProtocol == integrated.HostedControllerProtocol
}

func markHostedOutboundTerminal(stateDir string, config integrated.Config, requestID, requestSHA256 string, receipt hostedstatus.OperationResult, now time.Time) error {
	if receipt.RequestID != requestID || receipt.RequestSHA256 != requestSHA256 || receipt.OperatorLedgerSequence == 0 ||
		!hostedOperationDigestPattern.MatchString(receipt.OperatorTerminalSHA256) || (receipt.OperatorStatus != "succeeded" && receipt.OperatorStatus != "failed") {
		return errors.New("hosted operator receipt cannot retire an outbound request because its exact terminal binding is invalid")
	}
	ledger, err := loadHostedOutboundLedger(stateDir, config)
	if err != nil {
		return err
	}
	for index := range ledger.Requests {
		record := &ledger.Requests[index]
		if record.RequestID != requestID {
			continue
		}
		if record.RequestSHA256 != requestSHA256 {
			return errors.New("hosted outbound terminal receipt digest does not match its request")
		}
		if record.Status != "pending" {
			if record.TerminalSequence == receipt.OperatorLedgerSequence && record.TerminalSHA256 == receipt.OperatorTerminalSHA256 {
				return nil
			}
			return errors.New("hosted outbound terminal receipt conflicts with its durable journal")
		}
		record.Status, record.TerminalAt = "terminal", now.UTC()
		record.TerminalSequence, record.TerminalSHA256 = receipt.OperatorLedgerSequence, receipt.OperatorTerminalSHA256
		ledger.Requests = compactHostedOutboundLedger(ledger.Requests, now.UTC())
		return saveHostedOutboundLedger(stateDir, ledger)
	}
	return errors.New("hosted outbound request journal lost the terminal request")
}

func compactHostedOutboundLedger(records []hostedOutboundRecord, now time.Time) []hostedOutboundRecord {
	fullTerminal := make([]int, 0)
	for index := range records {
		if records[index].Status == "terminal" && !now.Before(records[index].ExpiresAt) {
			fullTerminal = append(fullTerminal, index)
		}
	}
	sort.Slice(fullTerminal, func(i, j int) bool {
		left, right := records[fullTerminal[i]], records[fullTerminal[j]]
		if left.TerminalSequence == right.TerminalSequence {
			return left.RequestID < right.RequestID
		}
		return left.TerminalSequence < right.TerminalSequence
	})
	if len(fullTerminal) > hostedOutboundRecentTerminals {
		for _, index := range fullTerminal[:len(fullTerminal)-hostedOutboundRecentTerminals] {
			records[index].Status, records[index].RequestPayload = "retired", ""
		}
	}
	retired := make([]hostedOutboundRecord, 0)
	retained := make([]hostedOutboundRecord, 0, len(records))
	for _, record := range records {
		if record.Status == "retired" {
			retired = append(retired, record)
		} else {
			retained = append(retained, record)
		}
	}
	sort.Slice(retired, func(i, j int) bool {
		if retired[i].TerminalAt.Equal(retired[j].TerminalAt) {
			return retired[i].RequestID < retired[j].RequestID
		}
		return retired[i].TerminalAt.Before(retired[j].TerminalAt)
	})
	for len(retired) > hostedOutboundRetiredLimit && now.Sub(retired[0].TerminalAt) >= hostedOutboundRetiredLifetime {
		retired = retired[1:]
	}
	return append(retired, retained...)
}

func saveHostedOutboundLedger(stateDir string, ledger hostedOutboundLedger) error {
	data, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil || len(data) > hostedOutboundMaxBytes {
		return errors.New("hosted outbound request journal exceeds its strict encoded bound")
	}
	directory := filepath.Join(stateDir, "integrated")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".hosted-outbound-requests-*.tmp")
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
	return os.Rename(name, filepath.Join(directory, hostedOutboundFile))
}

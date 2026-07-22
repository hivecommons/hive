package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
)

const (
	hostedOperationRequestSchema = "hive.hosted-operation-request.v2"
	hostedOperationMaxBytes      = 32 << 10
	hostedOperationLifetime      = 10 * time.Minute

	hostedApproveBaselinePlan  = "approve-baseline-plan"
	hostedApproveBaselineApply = "approve-baseline-apply"
	hostedApproveMergePlan     = "approve-merge-plan"
	hostedApproveMergeApply    = "approve-merge-apply"
	hostedRetryRepair          = "retry-repair"
	hostedRecoverDispatchPlan  = "recover-dispatch-plan"
	hostedRecoverDispatchApply = "recover-dispatch-apply"
)

var hostedOperationDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type hostedOperationRelease struct {
	Version                    string `json:"version"`
	HiveCommit                 string `json:"hive_commit"`
	VisualHiveCommit           string `json:"visual_hive_commit"`
	DistributionManifestSHA256 string `json:"distribution_manifest_sha256"`
	HostedControllerProtocol   int    `json:"hosted_controller_protocol"`
}

type hostedOperationRequest struct {
	SchemaVersion        string                            `json:"schema_version"`
	Repository           string                            `json:"repository"`
	RepositoryID         string                            `json:"repository_id"`
	WorkflowPath         string                            `json:"workflow_path"`
	DefaultBranch        string                            `json:"default_branch"`
	DefaultHeadSHA       string                            `json:"default_head_sha"`
	StateBranch          string                            `json:"state_branch"`
	ExpectedStateHeadSHA string                            `json:"expected_state_head_sha"`
	Release              hostedOperationRelease            `json:"release"`
	Operation            string                            `json:"operation"`
	RequestID            string                            `json:"request_id"`
	IssuedAt             time.Time                         `json:"issued_at"`
	ExpiresAt            time.Time                         `json:"expires_at"`
	Actor                integrated.HostedOperatorIdentity `json:"actor"`
	Arguments            json.RawMessage                   `json:"arguments"`
}

type hostedBaselineArguments struct {
	RepositoryID    string `json:"repository_id,omitempty"`
	RunID           int64  `json:"run_id,omitempty"`
	ArtifactID      int64  `json:"artifact_id,omitempty"`
	PRNumber        int    `json:"pr_number,omitempty"`
	HeadSHA         string `json:"head_sha,omitempty"`
	BaseSHA         string `json:"base_sha,omitempty"`
	DiffDigest      string `json:"diff_digest,omitempty"`
	CandidateDigest string `json:"candidate_digest,omitempty"`
	ActorID         int64  `json:"actor_id,omitempty"`
	PlanDigest      string `json:"plan_digest,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type hostedMergeArguments struct {
	PRNumber   int    `json:"pr_number"`
	HeadSHA    string `json:"head_sha"`
	BaseSHA    string `json:"base_sha,omitempty"`
	DiffDigest string `json:"diff_digest,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type hostedRetryArguments struct {
	Finding      string `json:"finding"`
	Recurrence   int    `json:"recurrence"`
	Attempt      int    `json:"attempt"`
	FailureClass string `json:"failure_class"`
	FailureID    string `json:"failure_id"`
	Reason       string `json:"reason"`
}

type hostedRecoveryArguments struct {
	Action        string    `json:"action"`
	Correlation   string    `json:"correlation"`
	RequestDigest string    `json:"request_digest,omitempty"`
	PlanDigest    string    `json:"plan_digest,omitempty"`
	PlannedAt     time.Time `json:"planned_at,omitempty"`
	Reason        string    `json:"reason,omitempty"`
}

func marshalHostedArguments(value any) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > hostedOperationMaxBytes {
		return nil, errors.New("hosted operation arguments exceed their strict bound")
	}
	return data, nil
}

func encodeHostedOperationRequest(request hostedOperationRequest) (string, string, []byte, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", "", nil, err
	}
	if len(data) == 0 || len(data) > hostedOperationMaxBytes {
		return "", "", nil, errors.New("hosted operation request exceeds its strict bound")
	}
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(data), hex.EncodeToString(digest[:]), data, nil
}

func decodeHostedOperationRequest(encoded, expectedDigest string) (hostedOperationRequest, []byte, error) {
	var request hostedOperationRequest
	if !hostedOperationDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(expectedDigest))) {
		return request, nil, errors.New("hosted operation request digest is invalid")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(data) == 0 || len(data) > hostedOperationMaxBytes {
		return request, nil, errors.New("hosted operation request encoding is invalid or oversized")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != strings.ToLower(strings.TrimSpace(expectedDigest)) {
		return request, nil, errors.New("hosted operation request digest does not match its exact bytes")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, nil, fmt.Errorf("decode hosted operation request: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return request, nil, errors.New("hosted operation request has trailing content")
	}
	return request, data, nil
}

func validateHostedOperationRequestEnvelope(request hostedOperationRequest, config integrated.Config, identity installedReleaseIdentity, operation, requestID string) error {
	if request.SchemaVersion != hostedOperationRequestSchema || request.Repository != config.Repository || request.RepositoryID != config.RepositoryID ||
		request.WorkflowPath != integrated.HostedControllerWorkflowPath || request.DefaultBranch != config.DefaultBranch || request.StateBranch != config.HostedStateBranch ||
		request.Operation != operation || request.RequestID != requestID {
		return errors.New("hosted operation request does not match the signed repository, workflow, operation, or request identity")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(request.DefaultHeadSHA) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(request.ExpectedStateHeadSHA) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(strings.ToLower(osEnv("GITHUB_SHA"))) {
		return errors.New("hosted operation request default or state head is invalid")
	}
	if request.Release.Version != identity.Version || request.Release.HiveCommit != identity.HiveCommit || request.Release.VisualHiveCommit != identity.VisualHiveCommit ||
		request.Release.DistributionManifestSHA256 != identity.ManifestSHA256 || request.Release.HostedControllerProtocol != identity.HostedControllerProtocol ||
		request.Release.HostedControllerProtocol != integrated.HostedControllerProtocol {
		return errors.New("hosted operation request release identity differs from the installed immutable release")
	}
	if request.IssuedAt.IsZero() || request.ExpiresAt.IsZero() || !request.ExpiresAt.Equal(request.IssuedAt.Add(hostedOperationLifetime)) {
		return errors.New("hosted operation request has an invalid lifetime")
	}
	if request.Actor.ID <= 0 || strings.TrimSpace(request.Actor.Login) == "" || len(request.Arguments) == 0 || len(request.Arguments) > hostedOperationMaxBytes {
		return errors.New("hosted operation request actor or arguments are incomplete")
	}
	return validateHostedOperationArguments(request.Operation, request.Arguments)
}

func validateHostedOperationRequestFresh(request hostedOperationRequest, stateHead, defaultHead string, now time.Time) error {
	if request.ExpectedStateHeadSHA != strings.ToLower(strings.TrimSpace(stateHead)) {
		return errors.New("hosted operation request state head is stale")
	}
	if request.DefaultHeadSHA != strings.ToLower(strings.TrimSpace(defaultHead)) {
		return errors.New("hosted operation request default head is stale")
	}
	if now.IsZero() || now.Before(request.IssuedAt.Add(-time.Minute)) || now.After(request.ExpiresAt) {
		return errors.New("hosted operation request is expired or from the future")
	}
	return nil
}

func validateHostedOperationArguments(operation string, data json.RawMessage) error {
	switch operation {
	case hostedApproveBaselinePlan, hostedApproveBaselineApply:
		var value hostedBaselineArguments
		if err := decodeHostedArguments(data, &value); err != nil {
			return err
		}
		if operation == hostedApproveBaselinePlan {
			if value != (hostedBaselineArguments{}) {
				return errors.New("hosted baseline plan accepts no apply arguments")
			}
			return nil
		}
		if value.RepositoryID == "" || value.RunID <= 0 || value.ArtifactID <= 0 || value.PRNumber <= 0 || value.ActorID <= 0 || strings.TrimSpace(value.Reason) == "" {
			return errors.New("hosted baseline apply arguments are incomplete")
		}
	case hostedApproveMergePlan, hostedApproveMergeApply:
		var value hostedMergeArguments
		if err := decodeHostedArguments(data, &value); err != nil {
			return err
		}
		if value.PRNumber <= 0 || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(strings.ToLower(value.HeadSHA)) {
			return errors.New("hosted merge operation requires an exact pull request and head")
		}
		if operation == hostedApproveMergeApply && (value.BaseSHA == "" || value.DiffDigest == "" || strings.TrimSpace(value.Reason) == "") {
			return errors.New("hosted merge apply arguments are incomplete")
		}
	case hostedRetryRepair:
		var value hostedRetryArguments
		if err := decodeHostedArguments(data, &value); err != nil {
			return err
		}
		if value.Finding == "" || value.Recurrence < 0 || value.Attempt <= 0 || value.FailureID == "" || value.Reason == "" {
			return errors.New("hosted retry arguments are incomplete")
		}
	case hostedRecoverDispatchPlan, hostedRecoverDispatchApply:
		var value hostedRecoveryArguments
		if err := decodeHostedArguments(data, &value); err != nil {
			return err
		}
		if (value.Action != "retry" && value.Action != "revoke") || value.Correlation == "" {
			return errors.New("hosted dispatch recovery action or correlation is invalid")
		}
		if operation == hostedRecoverDispatchApply && (value.RequestDigest == "" || value.PlanDigest == "" || value.PlannedAt.IsZero() || value.Reason == "") {
			return errors.New("hosted dispatch recovery apply arguments are incomplete")
		}
	default:
		return fmt.Errorf("operation %q is not an operator-state operation", operation)
	}
	return nil
}

func isHostedOperatorOperation(operation string) bool {
	switch operation {
	case hostedApproveBaselinePlan, hostedApproveBaselineApply, hostedApproveMergePlan, hostedApproveMergeApply,
		hostedRetryRepair, hostedRecoverDispatchPlan, hostedRecoverDispatchApply:
		return true
	default:
		return false
	}
}

func executeHostedOperator(ctx context.Context, stateDir string, request hostedOperationRequest, authority integrated.HostedOperatorAuthority, client *hivegithub.Client) (any, error) {
	if authority == nil || client == nil {
		return nil, errors.New("hosted operator execution lacks verified actor or GitHub authority")
	}
	switch request.Operation {
	case hostedApproveBaselinePlan, hostedApproveBaselineApply:
		var arguments hostedBaselineArguments
		if err := decodeHostedArguments(request.Arguments, &arguments); err != nil {
			return nil, err
		}
		result, err := integrated.ApproveSetupBaseline(ctx, integrated.ApproveSetupBaselineOptions{
			StateDir: stateDir, PlanOnly: request.Operation == hostedApproveBaselinePlan,
			ExpectedRepositoryID: arguments.RepositoryID, ExpectedCaptureRunID: arguments.RunID, ExpectedArtifactID: arguments.ArtifactID,
			ExpectedPRNumber: arguments.PRNumber, ExpectedHeadSHA: arguments.HeadSHA, ExpectedBaseSHA: arguments.BaseSHA,
			ExpectedDiffDigest: arguments.DiffDigest, ExpectedCandidateDigest: arguments.CandidateDigest, ExpectedActorID: arguments.ActorID,
			ExpectedPlanDigest: arguments.PlanDigest, Reason: arguments.Reason, GitHub: client, GitTransportToken: os.Getenv("HIVE_GITHUB_TOKEN"), HostedAuthority: authority,
		})
		if result.Planned {
			result.ApplyCommand = fmt.Sprintf("hive approve-baseline --repo-id %s --run-id %d --artifact-id %d --pr %d --head %s --base %s --diff-digest %s --candidate-digest %s --actor-id %d --plan-digest %s --reason %s --json",
				result.RepositoryID, result.CaptureRunID, result.ArtifactID, result.PRNumber, result.HeadSHA, result.BaseSHA, result.DiffDigest,
				result.CandidateDigest, result.ActorID, result.PlanDigest, strconv.Quote("REVIEWED_REASON"))
		} else {
			result.NextCommand = "hive run --json"
		}
		return result, err
	case hostedApproveMergePlan, hostedApproveMergeApply:
		var arguments hostedMergeArguments
		if err := decodeHostedArguments(request.Arguments, &arguments); err != nil {
			return nil, err
		}
		result, err := integrated.ApproveMerge(ctx, integrated.ApproveMergeOptions{
			StateDir: stateDir, PRNumber: arguments.PRNumber, ExpectedHeadSHA: arguments.HeadSHA, ExpectedBaseSHA: arguments.BaseSHA,
			ExpectedDiff: arguments.DiffDigest, Reason: arguments.Reason, PlanOnly: request.Operation == hostedApproveMergePlan,
			GitHub: client, HostedAuthority: authority,
		})
		if result.Planned {
			result.ApplyCommand = fmt.Sprintf("hive approve-merge --pr %d --head %s --base %s --diff-digest %s --reason %s --json",
				result.Gate.Number, strings.ToLower(result.Gate.HeadSHA), strings.ToLower(result.Gate.BaseSHA), result.DiffDigest, strconv.Quote("REVIEWED_REASON"))
		} else {
			result.NextCommand = "hive run --json"
		}
		return result, err
	case hostedRetryRepair:
		var arguments hostedRetryArguments
		if err := decodeHostedArguments(request.Arguments, &arguments); err != nil {
			return nil, err
		}
		result, err := integrated.RetryRepair(ctx, integrated.RetryRepairOptions{
			StateDir: stateDir, RepositoryFingerprint: arguments.Finding, ExpectedRecurrence: arguments.Recurrence, ExpectedAttempt: arguments.Attempt,
			ExpectedFailureClass: repair.FailureClass(arguments.FailureClass), ExpectedFailureID: arguments.FailureID, Reason: arguments.Reason,
			GitHub: client, HostedAuthority: authority,
		})
		result.NextCommand = "hive run --json"
		return result, err
	case hostedRecoverDispatchPlan, hostedRecoverDispatchApply:
		var arguments hostedRecoveryArguments
		if err := decodeHostedArguments(request.Arguments, &arguments); err != nil {
			return nil, err
		}
		result, err := integrated.RecoverWorkflowDispatch(ctx, integrated.RecoverWorkflowDispatchOptions{
			StateDir: stateDir, Action: integrated.WorkflowDispatchRecoveryAction(arguments.Action), CorrelationID: arguments.Correlation,
			ExpectedRequestDigest: arguments.RequestDigest, ExpectedPlanDigest: arguments.PlanDigest, PlannedAt: arguments.PlannedAt,
			Reason: arguments.Reason, PlanOnly: request.Operation == hostedRecoverDispatchPlan, GitHub: client, HostedAuthority: authority,
		})
		if result.Planned {
			result.ApplyCommand = fmt.Sprintf("hive recover-dispatch --action %s --correlation %s --request-digest %s --plan-digest %s --planned-at %s --reason %s --json",
				result.Action, result.CorrelationID, result.RequestDigest, result.PlanDigest, result.PlannedAt.Format(time.RFC3339Nano), strconv.Quote("ACCOUNTABLE_REASON"))
		} else {
			result.NextCommand = "hive run --json"
		}
		return result, err
	default:
		return nil, fmt.Errorf("unsupported hosted operator operation %q", request.Operation)
	}
}

func decodeHostedArguments(data json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode hosted operation arguments: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("hosted operation arguments have trailing content")
	}
	return nil
}

func verifyHostedWorkflowOperator(ctx context.Context, client *hivegithub.Client, config integrated.Config, request hostedOperationRequest, requestSHA256 string) (integrated.HostedOperatorAuthority, integrated.HostedOperatorIdentity, error) {
	runID, err := strconv.ParseInt(osEnv("GITHUB_RUN_ID"), 10, 64)
	if err != nil || runID <= 0 {
		return nil, integrated.HostedOperatorIdentity{}, errors.New("hosted operator workflow run identity is invalid")
	}
	eventActorID, err := strconv.ParseInt(osEnv("HIVE_EVENT_ACTOR_ID"), 10, 64)
	if err != nil || eventActorID <= 0 {
		return nil, integrated.HostedOperatorIdentity{}, errors.New("hosted operator event actor identity is invalid")
	}
	authority, err := integrated.VerifyHostedOperatorAuthority(ctx, client, integrated.HostedOperatorProof{
		Config: config, Operation: request.Operation, RequestID: request.RequestID, RequestSHA256: requestSHA256,
		DefaultHeadSHA: request.DefaultHeadSHA, RunHeadSHA: strings.ToLower(osEnv("GITHUB_SHA")), RunID: runID, EventActorID: eventActorID,
		EventActorLogin: osEnv("HIVE_EVENT_ACTOR_LOGIN"), RequestedActor: request.Actor,
	})
	if err != nil {
		return nil, integrated.HostedOperatorIdentity{}, err
	}
	identity, err := integrated.DescribeHostedOperatorAuthority(authority)
	if err != nil {
		return nil, integrated.HostedOperatorIdentity{}, err
	}
	return authority, identity, nil
}

func osEnv(name string) string { return strings.TrimSpace(os.Getenv(name)) }

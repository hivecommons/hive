package integrated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

type WorkflowDispatchRecoveryAction string

const (
	WorkflowDispatchRecoveryRetry  WorkflowDispatchRecoveryAction = "retry"
	WorkflowDispatchRecoveryRevoke WorkflowDispatchRecoveryAction = "revoke"
	dispatchRecoveryPlanLifetime                                  = 15 * time.Minute
)

type RecoverWorkflowDispatchOptions struct {
	StateDir              string
	Action                WorkflowDispatchRecoveryAction
	CorrelationID         string
	ExpectedRequestDigest string
	ExpectedPlanDigest    string
	PlannedAt             time.Time
	Reason                string
	PlanOnly              bool
	GitHub                *hivegithub.Client
	HostedAuthority       HostedOperatorAuthority
}

type WorkflowDispatchRecoveryDiscovery struct {
	PagesScanned int       `json:"pages_scanned"`
	RunsScanned  int       `json:"runs_scanned"`
	CompletedAt  time.Time `json:"completed_at"`
	MatchFound   bool      `json:"match_found"`
}

type RecoverWorkflowDispatchResult struct {
	SchemaVersion string                            `json:"schema_version"`
	Repository    string                            `json:"repository"`
	RepositoryID  string                            `json:"repository_id"`
	Action        WorkflowDispatchRecoveryAction    `json:"action"`
	CorrelationID string                            `json:"correlation_id"`
	WorkflowFile  string                            `json:"workflow_file"`
	Ref           string                            `json:"ref"`
	Operation     string                            `json:"operation"`
	DisplayTitle  string                            `json:"display_title"`
	RequestDigest string                            `json:"request_digest"`
	RequestState  string                            `json:"request_state"`
	Actor         string                            `json:"actor"`
	PlannedAt     time.Time                         `json:"planned_at"`
	ExpiresAt     time.Time                         `json:"expires_at"`
	PlanDigest    string                            `json:"plan_digest"`
	Discovery     WorkflowDispatchRecoveryDiscovery `json:"discovery"`
	Planned       bool                              `json:"planned"`
	Applied       bool                              `json:"applied"`
	Revoked       bool                              `json:"revoked"`
	ApplyCommand  string                            `json:"apply_command,omitempty"`
	NextCommand   string                            `json:"next_command,omitempty"`
}

func RecoverWorkflowDispatch(ctx context.Context, options RecoverWorkflowDispatchOptions) (RecoverWorkflowDispatchResult, error) {
	result := RecoverWorkflowDispatchResult{SchemaVersion: "hive.recover-workflow-dispatch.v1"}
	options.StateDir = strings.TrimSpace(options.StateDir)
	options.CorrelationID = strings.ToLower(strings.TrimSpace(options.CorrelationID))
	options.ExpectedRequestDigest = strings.ToLower(strings.TrimSpace(options.ExpectedRequestDigest))
	options.ExpectedPlanDigest = strings.ToLower(strings.TrimSpace(options.ExpectedPlanDigest))
	options.Reason = strings.TrimSpace(options.Reason)
	if options.GitHub == nil || options.StateDir == "" || !workflowDispatchCorrelationPattern.MatchString(options.CorrelationID) ||
		(options.Action != WorkflowDispatchRecoveryRetry && options.Action != WorkflowDispatchRecoveryRevoke) {
		return result, fmt.Errorf("GitHub client, persistent state, exact dispatch correlation, and recovery action retry or revoke are required")
	}
	if !options.PlanOnly {
		if len(options.ExpectedRequestDigest) != sha256.Size*2 || len(options.ExpectedPlanDigest) != sha256.Size*2 || options.PlannedAt.IsZero() || options.Reason == "" || len(options.Reason) > 2048 {
			return result, fmt.Errorf("apply requires exact request and plan digests, planned time, and a bounded accountable reason")
		}
		if _, err := hex.DecodeString(options.ExpectedRequestDigest); err != nil {
			return result, fmt.Errorf("expected workflow dispatch request digest is invalid")
		}
		if _, err := hex.DecodeString(options.ExpectedPlanDigest); err != nil {
			return result, fmt.Errorf("expected workflow dispatch recovery plan digest is invalid")
		}
	}
	release, err := acquireProductionRunLease(options.StateDir, 2*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize workflow dispatch recovery with production runs: %w", err)
	}
	defer release()
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	if err := rejectPendingAuthorizerTransfer(store, options.StateDir, "workflow dispatch recovery"); err != nil {
		return result, err
	}
	result.Repository, result.RepositoryID, result.Action = config.Repository, config.RepositoryID, options.Action
	fail := func(cause error) (RecoverWorkflowDispatchResult, error) {
		if options.PlanOnly {
			return result, cause
		}
		if auditErr := store.AuditStrict(AuditEntry{Action: "recover_workflow_dispatch", Allowed: false, Repository: config.Repository, Detail: cause.Error()}); auditErr != nil {
			return result, fmt.Errorf("%v; persist denied dispatch recovery audit: %w", cause, auditErr)
		}
		return result, cause
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return fail(err)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return fail(err)
	}
	if !exists {
		return fail(fmt.Errorf("there is no durable workflow dispatch to recover"))
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.CorrelationID != options.CorrelationID {
		return fail(fmt.Errorf("workflow dispatch recovery does not match the exact repository identity and correlation"))
	}
	if !WorkflowDispatchNeedsRecovery(intent) {
		return fail(fmt.Errorf("workflow dispatch is not in the ambiguous transport-failure state"))
	}
	if intent.RequestDigest == "" {
		return fail(fmt.Errorf("ambiguous workflow dispatch has no exact recorded request digest"))
	}
	result.CorrelationID, result.WorkflowFile, result.Ref, result.Operation = intent.CorrelationID, intent.WorkflowFile, intent.Ref, intent.Operation
	result.DisplayTitle, result.RequestDigest, result.RequestState = intent.ExpectedDisplayTitle, intent.RequestDigest, "ambiguous_transport_failure"
	operation := "recover-dispatch-apply"
	if options.PlanOnly {
		operation = "recover-dispatch-plan"
	}
	operator, err := resolveOperatorIdentity(ctx, options.GitHub, options.HostedAuthority, config.Repository, operation)
	if err != nil {
		return fail(err)
	}
	actor := operator.Login
	result.Actor = actor
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return fail(fmt.Errorf("configured repository is invalid"))
	}
	matched, pages, scanned, discoveryErr := discoverCorrelatedWorkflowRun(ctx, options.GitHub, owner, repo, intent.WorkflowFile, intent.Ref, intent)
	result.Discovery = WorkflowDispatchRecoveryDiscovery{PagesScanned: pages, RunsScanned: scanned, CompletedAt: time.Now().UTC(), MatchFound: matched != nil}
	if discoveryErr != nil {
		return fail(fmt.Errorf("workflow dispatch discovery is inconclusive; state remains fail-closed: %w", discoveryErr))
	}
	if matched != nil {
		return fail(fmt.Errorf("exact matching workflow run %d exists at %s; recovery is rejected, run Hive to bind and consume it", matched.GetID(), matched.GetHTMLURL()))
	}
	if options.PlanOnly {
		result.Planned, result.PlannedAt = true, result.Discovery.CompletedAt
		result.ExpiresAt = result.PlannedAt.Add(dispatchRecoveryPlanLifetime)
		result.PlanDigest, err = workflowDispatchRecoveryPlanDigest(intent, options.Action, actor, result.PlannedAt)
		if err != nil {
			return result, err
		}
		result.ApplyCommand = fmt.Sprintf("hive recover-dispatch --state-dir %q --action %s --correlation %s --request-digest %s --plan-digest %s --planned-at %s --reason %q --json", options.StateDir, options.Action, intent.CorrelationID, intent.RequestDigest, result.PlanDigest, result.PlannedAt.Format(time.RFC3339Nano), "ACCOUNTABLE_REASON")
		return result, nil
	}
	now := time.Now().UTC()
	if options.PlannedAt.After(now.Add(time.Minute)) || now.After(options.PlannedAt.Add(dispatchRecoveryPlanLifetime)) {
		return fail(fmt.Errorf("workflow dispatch recovery plan is expired or from the future; rerun --plan"))
	}
	if options.ExpectedRequestDigest != intent.RequestDigest {
		return fail(fmt.Errorf("workflow dispatch request digest changed after planning"))
	}
	expectedPlanDigest, err := workflowDispatchRecoveryPlanDigest(intent, options.Action, actor, options.PlannedAt)
	if err != nil {
		return fail(err)
	}
	if options.ExpectedPlanDigest != expectedPlanDigest {
		return fail(fmt.Errorf("workflow dispatch recovery plan does not match repository, request, action, actor, or planned time"))
	}
	detail := fmt.Sprintf("actor=%s action=%s repository_id=%s correlation=%s workflow=%s ref=%s title=%q request_digest=%s plan_digest=%s planned_at=%s reason=%s discovery_pages=%d discovery_runs=%d", actor, options.Action, config.RepositoryID, intent.CorrelationID, intent.WorkflowFile, intent.Ref, intent.ExpectedDisplayTitle, intent.RequestDigest, expectedPlanDigest, options.PlannedAt.Format(time.RFC3339Nano), options.Reason, pages, scanned)
	if err := store.AuditStrict(AuditEntry{Action: "recover_workflow_dispatch", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, fmt.Errorf("persist dispatch recovery authorization: %w", err)
	}
	result.PlannedAt, result.ExpiresAt, result.PlanDigest = options.PlannedAt, options.PlannedAt.Add(dispatchRecoveryPlanLifetime), expectedPlanDigest
	result.Applied = true
	if options.Action == WorkflowDispatchRecoveryRevoke {
		if intent.Operation == setupBaselineWorkflowOperation {
			if err := revokeSetupBaselineDispatch(store, config, intent); err != nil {
				return result, fmt.Errorf("retire exact setup baseline dispatch before revoke: %w", err)
			}
		}
		if err := store.DeleteWorkflowDispatchIntent(); err != nil {
			return result, fmt.Errorf("revoke exact ambiguous workflow dispatch: %w", err)
		}
		result.Revoked = true
		result.NextCommand = fmt.Sprintf("hive run --state-dir %q --json", options.StateDir)
		return result, nil
	}
	intent.RecoveryAction = string(WorkflowDispatchRecoveryRetry)
	intent.RecoveryRequestDigest = intent.RequestDigest
	intent.RecoveryPlanDigest = expectedPlanDigest
	intent.RecoveryActor = actor
	intent.RecoveryReason = options.Reason
	intent.RecoveryAuthorizedAt = now
	intent.RecoveryOriginalAttemptedAt = intent.DispatchAttemptedAt
	intent.RecoveryCount++
	intent.DispatchAttemptedAt = time.Time{}
	intent.DispatchAcknowledgedAt = time.Time{}
	intent.RequestDigest = ""
	intent.RunID, intent.RunURL, intent.MatchedAt = 0, "", time.Time{}
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		return result, fmt.Errorf("activate audited same-correlation dispatch retry: %w", err)
	}
	result.NextCommand = fmt.Sprintf("hive run --state-dir %q --json", options.StateDir)
	return result, nil
}

func workflowDispatchRecoveryPlanDigest(intent WorkflowDispatchIntent, action WorkflowDispatchRecoveryAction, actor string, plannedAt time.Time) (string, error) {
	record := struct {
		SchemaVersion string                         `json:"schema_version"`
		Repository    string                         `json:"repository"`
		RepositoryID  string                         `json:"repository_id"`
		Action        WorkflowDispatchRecoveryAction `json:"action"`
		CorrelationID string                         `json:"correlation_id"`
		WorkflowFile  string                         `json:"workflow_file"`
		Ref           string                         `json:"ref"`
		Operation     string                         `json:"operation"`
		DisplayTitle  string                         `json:"display_title"`
		RequestDigest string                         `json:"request_digest"`
		Actor         string                         `json:"actor"`
		PlannedAt     time.Time                      `json:"planned_at"`
	}{
		SchemaVersion: "hive.recover-workflow-dispatch-plan.v1", Repository: intent.Repository, RepositoryID: intent.RepositoryID,
		Action: action, CorrelationID: intent.CorrelationID, WorkflowFile: intent.WorkflowFile, Ref: intent.Ref, Operation: intent.Operation,
		DisplayTitle: intent.ExpectedDisplayTitle, RequestDigest: intent.RequestDigest, Actor: actor, PlannedAt: plannedAt.UTC(),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

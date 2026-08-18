package integrated

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/repair"
	"github.com/kubestellar/hive/pkg/visualhive"
)

type RetryRepairOptions struct {
	StateDir              string
	RepositoryFingerprint string
	ExpectedRecurrence    int
	ExpectedAttempt       int
	ExpectedFailureClass  repair.FailureClass
	ExpectedFailureID     string
	Reason                string
	GitHub                *hivegithub.Client
	HostedAuthority       HostedOperatorAuthority
}

type RetryRepairResult struct {
	SchemaVersion string         `json:"schema_version"`
	Actor         string         `json:"actor"`
	Attempt       repair.Attempt `json:"attempt"`
	NextCommand   string         `json:"next_command"`
}

// RetryRepair authorizes the exact persisted infrastructure or patch-engine
// checkpoint without spending a new model attempt. It shares the production
// run lease, binds the actor to GitHub authentication, and delegates the
// compare-and-resume mutation to repair.Store's fsynced audit transaction.
func RetryRepair(ctx context.Context, options RetryRepairOptions) (RetryRepairResult, error) {
	result := RetryRepairResult{SchemaVersion: "hive.retry-repair.v1"}
	options.StateDir = strings.TrimSpace(options.StateDir)
	options.RepositoryFingerprint = strings.TrimSpace(options.RepositoryFingerprint)
	options.ExpectedFailureID = strings.TrimSpace(options.ExpectedFailureID)
	options.Reason = strings.TrimSpace(options.Reason)
	if options.GitHub == nil || options.StateDir == "" || options.RepositoryFingerprint == "" || options.ExpectedRecurrence < 0 || options.ExpectedAttempt <= 0 || options.ExpectedFailureClass == "" || options.ExpectedFailureID == "" || options.Reason == "" {
		return result, fmt.Errorf("state directory, finding fingerprint, expected recurrence, expected attempt, expected failure class, expected failure ID, reason, and GitHub authentication are required")
	}
	release, err := acquireProductionRunLease(options.StateDir, 2*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize repair retry with production runs: %w", err)
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
	if err := rejectPendingAuthorizerTransfer(store, options.StateDir, "repair retry"); err != nil {
		return result, err
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	operator, err := resolveOperatorIdentity(ctx, options.GitHub, options.HostedAuthority, config.Repository, "retry-repair")
	if err != nil {
		return result, err
	}
	actor := operator.Login
	repairStore, err := repair.NewStore(filepath.Join(options.StateDir, "repair"))
	if err != nil {
		return result, err
	}
	checkpoint, exists := repairStore.Get(options.RepositoryFingerprint)
	if !exists {
		return result, fmt.Errorf("repair checkpoint %q does not exist", options.RepositoryFingerprint)
	}
	if checkpoint.Stage == repair.StageFailed && checkpoint.ResumeStage == repair.StagePROpen {
		lifecycle, lifecycleErr := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive"))
		if lifecycleErr != nil {
			return result, lifecycleErr
		}
		finding, found := lifecycle.Finding(options.RepositoryFingerprint)
		if !found || finding.Repository != config.Repository || finding.RepositoryID != config.RepositoryID || finding.Recurrences != checkpoint.Recurrence ||
			finding.RepairAttempts != checkpoint.Attempt || finding.PRNumber != checkpoint.PRNumber || finding.Branch != checkpoint.Branch ||
			!strings.EqualFold(finding.RepairCommitSHA, checkpoint.CommitSHA) || finding.Status != visualhive.StatusNeedsRevision {
			return result, fmt.Errorf("repair base-conflict retry does not match the exact durable lifecycle checkpoint")
		}
		if finding.HumanReviewRequired {
			if finding.ManualReviewKind != "repair_base_conflict" {
				return result, fmt.Errorf("repair is held for unrelated manual review %q", finding.ManualReviewKind)
			}
			// Clearing the lifecycle hold before resuming the failed repair state
			// is crash-safe: StageFailed still blocks all worker mutation if the
			// second durable transition does not complete.
			if err := lifecycle.MarkManualReviewComplete(options.RepositoryFingerprint, "repair_base_conflict"); err != nil {
				return result, err
			}
		}
	}
	attempt, err := repairStore.ResumeRetry(repair.RetryRequest{
		RepositoryFingerprint: options.RepositoryFingerprint, ExpectedRecurrence: options.ExpectedRecurrence, ExpectedAttempt: options.ExpectedAttempt,
		ExpectedFailureClass: options.ExpectedFailureClass, ExpectedFailureID: options.ExpectedFailureID, Actor: actor, Reason: options.Reason,
	})
	if err != nil {
		return result, err
	}
	result.Actor = actor
	result.Attempt = attempt
	result.NextCommand = fmt.Sprintf("hive run --state-dir %q --json", options.StateDir)
	return result, nil
}

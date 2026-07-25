package integrated

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

type ManagementOperation string

const (
	OperationUpgrade   ManagementOperation = "upgrade"
	OperationRollback  ManagementOperation = "rollback"
	OperationUninstall ManagementOperation = "uninstall"
)

type ManagementOptions struct {
	Operation         ManagementOperation
	StateDir          string
	VisualHiveRef     string
	VisualHiveCommand string
	VisualHiveArgs    []string
	DeleteState       bool
	Cancel            bool
	GitHub            *hivegithub.Client
	GitTransportToken string
}

type ManagementResult struct {
	SchemaVersion               string              `json:"schema_version"`
	Operation                   ManagementOperation `json:"operation"`
	Repository                  string              `json:"repository"`
	Branch                      string              `json:"branch"`
	CommitSHA                   string              `json:"commit_sha"`
	PRNumber                    int                 `json:"pr_number"`
	PRURL                       string              `json:"pr_url"`
	PreviousRef                 string              `json:"previous_ref,omitempty"`
	RequestedRef                string              `json:"requested_ref,omitempty"`
	Idempotent                  bool                `json:"idempotent"`
	StateDeleted                bool                `json:"state_deleted"`
	FinalizationPending         bool                `json:"finalization_pending"`
	CleanupVerified             bool                `json:"cleanup_verified"`
	ProtectionReconciled        bool                `json:"protection_reconciled"`
	NextCommand                 string              `json:"next_command,omitempty"`
	SetupAuthorizationContext   string              `json:"setup_authorization_context,omitempty"`
	SetupAuthorizationStatusID  int64               `json:"setup_authorization_status_id,omitempty"`
	SetupAuthorizationCreatorID int64               `json:"setup_authorization_creator_id,omitempty"`
	SetupAuthorizationReused    bool                `json:"setup_authorization_reused,omitempty"`
	Cancelled                   bool                `json:"cancelled,omitempty"`
	CancelCommand               string              `json:"cancel_command,omitempty"`
}

func RunManagement(ctx context.Context, options ManagementOptions) (ManagementResult, error) {
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	result := ManagementResult{SchemaVersion: "hive.management.v1", Operation: options.Operation}
	if options.GitHub == nil || strings.TrimSpace(options.StateDir) == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	if options.Operation != OperationUpgrade && options.Operation != OperationRollback && options.Operation != OperationUninstall {
		return result, fmt.Errorf("unsupported management operation %q", options.Operation)
	}
	if options.Cancel && options.Operation != OperationUninstall {
		return result, fmt.Errorf("--cancel is supported only for an uninstall with a durable pending cleanup intent")
	}
	if options.Cancel && options.DeleteState {
		return result, fmt.Errorf("uninstall cancellation cannot be combined with permanent state deletion")
	}
	release, leaseErr := acquireProductionRunLease(options.StateDir, 25*time.Minute)
	if leaseErr != nil {
		return result, fmt.Errorf("serialize %s with production runs: %w", options.Operation, leaseErr)
	}
	defer release()
	if options.Operation == OperationUninstall {
		releaseDeletionGuard, guardErr := acquireUninstallDeletionGuard(options.StateDir)
		if guardErr != nil {
			return result, fmt.Errorf("serialize uninstall state deletion: %w", guardErr)
		}
		defer releaseDeletionGuard()
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	if options.Operation == OperationUninstall {
		if err := restoreInterruptedStateDeletion(store); err != nil {
			return result, err
		}
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	if transition, exists, transitionErr := store.LoadHostedReleaseTransitionIntent(); transitionErr != nil {
		return result, fmt.Errorf("read hosted release transition before %s: %w", options.Operation, transitionErr)
	} else if exists {
		return result, fmt.Errorf("hosted %s transition phase %s is pending; resume or explicitly cancel that exact transition before %s", transition.Operation, transition.Phase, options.Operation)
	}
	if transfer, exists, transferErr := store.LoadAuthorizerTransferIntent(); transferErr != nil {
		return result, transferErr
	} else if exists {
		return result, fmt.Errorf("setup authorizer transfer to %s (numeric ID %d) is pending; finish its exact next command before another managed operation", transfer.NewAuthorizerLogin, transfer.NewAuthorizerID)
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	result.Repository, result.PreviousRef = config.Repository, config.VisualHiveRef
	if options.Operation == OperationUninstall {
		if err := verifyManagedOperationAuthorizer(ctx, options.GitHub, config, options.Operation); err != nil {
			return result, err
		}
		if err := drainSetupBaselineForUninstall(ctx, store, config, options.GitHub); err != nil {
			return result, fmt.Errorf("drain setup baseline lifecycle before uninstall: %w", err)
		}
	} else if err := blockPendingSetupBaselineManagement(store, options.Operation); err != nil {
		return result, err
	}
	if options.Operation == OperationUninstall {
		intent, exists, intentErr := store.LoadUninstallIntent()
		if intentErr != nil {
			return result, intentErr
		}
		if exists {
			if options.Cancel {
				return cancelUninstall(ctx, options, store, config, intent, result)
			}
			return resumeUninstall(ctx, options, store, config, intent, result, release)
		}
		if options.Cancel {
			return result, fmt.Errorf("no durable uninstall cleanup intent is pending to cancel")
		}
		if recoveryErr := verifyUninstallRecoveryState(store); recoveryErr != nil {
			_ = store.AuditStrict(AuditEntry{Action: "uninstall_recovery_state", Allowed: false, Repository: config.Repository, Detail: recoveryErr.Error()})
			return result, fmt.Errorf("uninstall requires exact recovery of pending integrated mutation state before cancellation: %w", recoveryErr)
		}
		config.Paused = true
		if err := store.Save(config); err != nil {
			return result, err
		}
		result.FinalizationPending, result.NextCommand, result.CancelCommand = true, UninstallNextCommand(options.StateDir), UninstallCancelCommand(options.StateDir)
		if err := store.AuditStrict(AuditEntry{Action: "uninstall_drain_started", Allowed: true, Repository: config.Repository, Detail: "automation paused before exact issue/PR/branch cancellation"}); err != nil {
			return result, err
		}
		if drainErr := drainUninstallLifecycle(ctx, options.StateDir, store, config, options.GitHub); drainErr != nil {
			_ = store.AuditStrict(AuditEntry{Action: "uninstall_drain", Allowed: false, Repository: config.Repository, Detail: drainErr.Error()})
			return result, fmt.Errorf("uninstall drain stopped with automation paused and state preserved: %w; correct the exact ownership mismatch and retry %s", drainErr, result.NextCommand)
		}
		if quiescenceErr := verifyUninstallQuiescence(options.StateDir, store); quiescenceErr != nil {
			_ = store.AuditStrict(AuditEntry{Action: "uninstall_quiescence", Allowed: false, Repository: config.Repository, Detail: quiescenceErr.Error()})
			return result, fmt.Errorf("uninstall drain did not reach terminal state: %w; retry %s", quiescenceErr, result.NextCommand)
		}
		if err := store.AuditStrict(AuditEntry{Action: "uninstall_quiescence", Allowed: true, Repository: config.Repository, Detail: "lifecycle, outbox, repair, and recovery state are terminal"}); err != nil {
			return result, err
		}
	}
	policy := automation.Policy{ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), AllowedRepositories: []string{config.Repository}}
	if options.Operation == OperationUpgrade || options.Operation == OperationRollback {
		requested := strings.ToLower(strings.TrimSpace(options.VisualHiveRef))
		if requested == "" && options.Operation == OperationRollback {
			requested = strings.ToLower(strings.TrimSpace(config.PreviousVersion))
		}
		if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(requested) {
			return result, fmt.Errorf("%s requires an immutable 40-character Visual Hive commit SHA", options.Operation)
		}
		result.RequestedRef, options.VisualHiveRef = requested, requested
		if err := VerifyVisualHiveWorkflowCommits(ctx, options.GitHub, config.VisualHiveRepo, requested); err != nil {
			return result, err
		}
		if requested == config.VisualHiveRef {
			// Local state is not proof that a prior upgrade PR reached the
			// default branch. Only report a no-op when every managed production
			// file at the target branch still matches this exact pin and policy.
			if installedErr := VerifyInstalledSetup(ctx, options.GitHub, config); installedErr == nil {
				if strings.TrimSpace(options.VisualHiveCommand) != "" && (config.VisualHiveCommand != options.VisualHiveCommand || !reflect.DeepEqual(config.VisualHiveArgs, options.VisualHiveArgs)) {
					config.VisualHiveCommand = options.VisualHiveCommand
					config.VisualHiveArgs = append([]string(nil), options.VisualHiveArgs...)
					if err := store.Save(config); err != nil {
						return result, err
					}
				}
				result.Idempotent = true
				return result, nil
			}
		}
		if err := verifyManagedOperationAuthorizer(ctx, options.GitHub, config, options.Operation); err != nil {
			return result, err
		}
	}
	branch := managedOperationBranch(string(options.Operation), config.RepositoryID)
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupBranch); err != nil {
		return result, err
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return result, err
	}
	baseSHA := ""
	if options.Operation == OperationUninstall {
		baseSHA, err = git(ctx, config.CheckoutDir, "rev-parse", "origin/"+defaultBranch)
		if err != nil {
			return result, fmt.Errorf("read exact default-branch base before uninstall: %w", err)
		}
		baseSHA = strings.ToLower(strings.TrimSpace(baseSHA))
	}
	if _, err := git(ctx, config.CheckoutDir, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return result, err
	}
	managed := managedSetupFilesForConfig(config)
	title := ""
	marker := fmt.Sprintf("<!-- hive-%s: %s -->", options.Operation, strings.ToLower(config.Repository))
	if options.Operation == OperationUninstall {
		marker, err = newUninstallMarker(config.Repository)
		if err != nil {
			return result, err
		}
	}
	candidate := config
	switch options.Operation {
	case OperationUpgrade, OperationRollback:
		if config.SetupAuthorizationActorID <= 0 {
			return result, fmt.Errorf("managed %s requires the numeric setup authorizer recorded by a current Hive installation; rerun setup first", options.Operation)
		}
		candidate.SetupBranch = branch
		requested := options.VisualHiveRef
		if !strings.EqualFold(requested, config.VisualHiveRef) {
			candidate.PreviousVersion = config.VisualHiveRef
		}
		candidate.VisualHiveRef, candidate.UpdatedAt = requested, time.Now().UTC()
		if strings.TrimSpace(options.VisualHiveCommand) != "" {
			candidate.VisualHiveCommand = options.VisualHiveCommand
			candidate.VisualHiveArgs = append([]string(nil), options.VisualHiveArgs...)
		}
		inspection, inspectErr := InspectCheckout(config.CheckoutDir, defaultBranch)
		if inspectErr != nil {
			return result, inspectErr
		}
		if err := writeManagedFiles(config.CheckoutDir, candidate, inspection); err != nil {
			return result, err
		}
		title = fmt.Sprintf("%s Visual Hive to %s", titleCaseOperation(options.Operation), requested[:12])
	case OperationUninstall:
		if config.SetupAuthorizationActorID <= 0 {
			return result, fmt.Errorf("managed uninstall requires the numeric setup authorizer recorded by a current Hive installation; rerun setup first")
		}
		candidate.Paused = true
		if managedPathPreimagesConfigured(config) && !hasValidManagedPathPreimages(config) {
			return result, fmt.Errorf("managed uninstall refuses an invalid repository preimage ledger")
		}
		if hasValidManagedPathPreimages(config) {
			if err := restoreManagedPathPreimages(config.CheckoutDir, config); err != nil {
				return result, fmt.Errorf("restore repository-owned files during uninstall: %w", err)
			}
		} else {
			// Legacy installations predate the preimage ledger. Preserve their
			// historical behavior; every newly created installation records an
			// exact restoration policy before the first managed write.
			for _, relative := range managed {
				if err := os.Remove(filepath.Join(config.CheckoutDir, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
					return result, err
				}
			}
		}
		title = uninstallTitle()
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	if err := stageManagedPaths(ctx, config.CheckoutDir, managed); err != nil {
		return result, err
	}
	if options.Operation == OperationUninstall {
		if err := applyManagedPathPreimageModes(ctx, config.CheckoutDir, config); err != nil {
			return result, err
		}
	}
	changed, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
	}
	result.Idempotent = strings.TrimSpace(changed) == ""
	if !result.Idempotent {
		message := "chore: " + string(options.Operation) + " Hive integration"
		if _, err := git(ctx, config.CheckoutDir, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", message, "-m", managedCommitTrailers(config.RepositoryID, string(options.Operation))); err != nil {
			return result, err
		}
	}
	sha, err := git(ctx, config.CheckoutDir, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.CommitSHA, result.Branch = strings.TrimSpace(sha), branch
	if options.Operation == OperationUninstall {
		candidate.Paused = true
		if err := store.Save(candidate); err != nil {
			return result, err
		}
		intent, err := newUninstallIntent(config, defaultBranch, branch, result.CommitSHA, baseSHA, marker, strings.Fields(changed))
		if err != nil {
			return result, err
		}
		if err := store.SaveUninstallIntent(intent); err != nil {
			return result, err
		}
		if intent.Phase == uninstallAlreadyClean {
			if err := VerifyUninstalledSetupAtCommit(ctx, options.GitHub, config, intent.BaseSHA); err != nil {
				return result, fmt.Errorf("cleanup produced no changes but target is not exactly uninstalled: %w", err)
			}
			if err := store.AuditStrict(AuditEntry{Action: "uninstall_already_clean", Allowed: true, Repository: config.Repository, Detail: "head=" + intent.BaseSHA}); err != nil {
				return result, err
			}
			result.FinalizationPending, result.NextCommand = true, UninstallNextCommand(options.StateDir)
			return result, nil
		}
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPush); err != nil {
		return result, err
	}
	if err := pushManagedBranch(ctx, config.CheckoutDir, config.Repository, branch, config.RepositoryID, string(options.Operation), result.CommitSHA); err != nil {
		return result, err
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPR); err != nil {
		return result, err
	}
	body := fmt.Sprintf("%s\n\nHive-managed `%s` operation.\n\n- Previous Visual Hive ref: `%s`\n- Requested Visual Hive ref: `%s`\n\nThis PR is intentionally reviewable and idempotent. Hive remains paused immediately for uninstall; upgrade and rollback take effect after this PR is merged.", marker, options.Operation, config.VisualHiveRef, result.RequestedRef)
	if options.Operation == OperationUninstall {
		body = uninstallBody(marker, config)
	}
	pull, err := options.GitHub.UpsertRepairPullRequest(ctx, config.Repository, branch, result.CommitSHA, defaultBranch, title, body, marker)
	if err != nil {
		return result, err
	}
	if err := verifyManagedPullHead(string(options.Operation), pull, result.CommitSHA); err != nil {
		return result, err
	}
	result.PRNumber, result.PRURL = pull.Number, pull.URL
	candidate.SetupBranch, candidate.SetupPRNumber, candidate.SetupPRURL = branch, pull.Number, pull.URL
	if options.Operation == OperationUpgrade || options.Operation == OperationRollback || options.Operation == OperationUninstall {
		authorization, authorizationErr := authorizeManagedSetupPullRequest(ctx, store, options.GitHub, policy, candidate, string(options.Operation), config.CheckoutDir, branch, result.CommitSHA, pull.URL, pull.Number)
		if authorizationErr != nil {
			return result, authorizationErr
		}
		result.SetupAuthorizationContext, result.SetupAuthorizationStatusID = authorization.Status.Context, authorization.Status.StatusID
		result.SetupAuthorizationCreatorID, result.SetupAuthorizationReused = authorization.Status.CreatorID, authorization.Status.Reused
	}
	if options.Operation == OperationUninstall {
		intent, exists, err := store.LoadUninstallIntent()
		if err != nil || !exists {
			if err == nil {
				err = fmt.Errorf("durable uninstall intent disappeared before pull request binding")
			}
			return result, err
		}
		snapshot, err := options.GitHub.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, pull.Number, marker, branch, result.CommitSHA, defaultBranch)
		if err != nil {
			return result, err
		}
		intent, err = bindUninstallPullSnapshot(intent, snapshot)
		if err != nil {
			return result, err
		}
		if err := store.SaveUninstallIntent(intent); err != nil {
			return result, err
		}
		candidate.Paused = true
		if err := store.Save(candidate); err != nil {
			return result, err
		}
		if err := store.AuditStrict(AuditEntry{Action: "uninstall_prepared", Allowed: true, Repository: config.Repository, Detail: result.PRURL}); err != nil {
			return result, err
		}
		result.FinalizationPending, result.NextCommand = true, UninstallNextCommand(options.StateDir)
	} else {
		if err := store.Save(candidate); err != nil {
			return result, err
		}
		if err := store.AuditStrict(AuditEntry{Action: string(options.Operation), Allowed: true, Repository: config.Repository, Detail: result.PRURL}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func blockPendingSetupBaselineManagement(store *Store, operation ManagementOperation) error {
	rebind, rebindExists, rebindErr := store.LoadSetupBaselineRebindIntent()
	if rebindErr != nil {
		return fmt.Errorf("read setup baseline rebind before %s: %w", operation, rebindErr)
	}
	if rebindExists {
		return fmt.Errorf("setup baseline rebind phase %s or its durable audit receipt is pending; complete %s before %s", rebind.Phase, SetupBaselineRebindNextCommand(rebind), operation)
	}
	baseline, exists, baselineErr := store.LoadSetupBaselineIntent()
	if baselineErr != nil {
		return baselineErr
	}
	if exists && (baseline.Phase != SetupBaselineProductionVerified || baseline.PendingAudit != nil) {
		return fmt.Errorf("setup baseline phase %s or its durable audit receipt is pending; complete it before %s", baseline.Phase, operation)
	}
	return nil
}

func verifyManagedOperationAuthorizer(ctx context.Context, client *hivegithub.Client, config Config, operation ManagementOperation) error {
	if config.SetupAuthorizationActorID <= 0 {
		return fmt.Errorf("managed %s requires the numeric setup authorizer recorded by a current Hive installation; rerun setup first", operation)
	}
	identity, err := client.AuthenticatedNumericUser(ctx)
	if err != nil {
		return err
	}
	if identity.ID != config.SetupAuthorizationActorID {
		return fmt.Errorf("managed %s is bound to GitHub user ID %d, but the current authenticated user is %d; rerun with the recorded setup authorizer", operation, config.SetupAuthorizationActorID, identity.ID)
	}
	return nil
}

func titleCaseOperation(operation ManagementOperation) string {
	value := string(operation)
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func deleteManagedState(stateDir string) error {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute) + string(os.PathSeparator)
	home, _ := os.UserHomeDir()
	if absolute == volume || absolute == filepath.Clean(home) || len(filepath.SplitList(absolute)) == 0 {
		return fmt.Errorf("refusing to delete unsafe state path %s", absolute)
	}
	rootInfo, rootErr := os.Lstat(absolute)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(absolute) {
		return fmt.Errorf("refusing to delete non-ordinary state root %s", absolute)
	}
	if resolved, evalErr := filepath.EvalSymlinks(absolute); evalErr != nil || filepath.Clean(resolved) != absolute {
		return fmt.Errorf("refusing to delete state root that resolves through filesystem indirection")
	}
	marker := filepath.Join(absolute, "integrated", "config.json")
	finalizingMarker := filepath.Join(absolute, "integrated", uninstallFinalizingConfigFile)
	info, statErr := os.Lstat(marker)
	if statErr != nil {
		marker = finalizingMarker
		info, statErr = os.Lstat(finalizingMarker)
	}
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(marker) {
		return fmt.Errorf("refusing to delete %s without a managed Hive config marker", absolute)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	var config Config
	if json.Unmarshal(data, &config) != nil || !supportedDurableConfigSchema(config.SchemaVersion) || strings.TrimSpace(config.Repository) == "" || strings.TrimSpace(config.RepositoryID) == "" {
		return fmt.Errorf("refusing to delete %s because its managed config identity is invalid", absolute)
	}
	allowedDirectories := map[string]bool{"integrated": true, "visual-hive": true, "repair": true, "beads": true, "runtime": true}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		info, infoErr := os.Lstat(filepath.Join(absolute, name))
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(filepath.Join(absolute, name)) {
			return fmt.Errorf("refusing to delete state root with linked or unreadable top-level entry %s", name)
		}
		if entry.IsDir() && allowedDirectories[name] {
			continue
		}
		if !entry.IsDir() && info.Mode().IsRegular() && (name == stateOwnershipMarkerFile || regexp.MustCompile(`^upgrade-integrated-[0-9]+\.json$`).MatchString(name)) {
			continue
		}
		return fmt.Errorf("refusing to delete Hive state root %s because unrelated top-level entry %s would be removed", absolute, name)
	}
	if err := validateManagedIntegratedStateInventory(filepath.Join(absolute, "integrated"), absolute, config); err != nil {
		return err
	}
	ownerPath := filepath.Join(absolute, stateOwnershipMarkerFile)
	ownerInfo, ownerStatErr := os.Lstat(ownerPath)
	if ownerStatErr != nil || !ownerInfo.Mode().IsRegular() || ownerInfo.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(ownerPath) {
		return fmt.Errorf("refusing to delete Hive state root without an exact ordinary ownership marker; rerun setup or upgrade to migrate legacy state")
	}
	ownerData, ownerErr := os.ReadFile(ownerPath)
	if ownerErr != nil {
		return ownerErr
	}
	var owner stateOwnershipMarker
	if json.Unmarshal(ownerData, &owner) != nil || owner.SchemaVersion != "hive.state-owner.v1" || owner.Repository != config.Repository || owner.RepositoryID != config.RepositoryID {
		return fmt.Errorf("refusing to delete Hive state root with mismatched ownership marker")
	}
	if err := validateManagedStateDeletionTree(absolute, config); err != nil {
		return err
	}
	return os.RemoveAll(absolute)
}

const (
	managedDaemonStatusMaxBytes = int64(64 << 10)
	managedDaemonLeaseMaxBytes  = int64(16 << 10)
	managedDaemonLogMaxBytes    = int64(128 << 20)
)

type managedDaemonDeletionStatus struct {
	SchemaVersion    string          `json:"schema_version"`
	PID              int             `json:"pid"`
	Running          bool            `json:"running"`
	RuntimeRunning   bool            `json:"runtime_running"`
	Repository       string          `json:"repository,omitempty"`
	StateDir         string          `json:"state_dir"`
	Executable       string          `json:"executable,omitempty"`
	HiveCommit       string          `json:"hive_commit,omitempty"`
	ExecutableSHA256 string          `json:"executable_sha256,omitempty"`
	StartedAt        time.Time       `json:"started_at,omitempty"`
	StoppedAt        time.Time       `json:"stopped_at,omitempty"`
	IntervalSeconds  int64           `json:"interval_seconds"`
	LastAttemptAt    time.Time       `json:"last_attempt_at,omitempty"`
	LastSuccessAt    time.Time       `json:"last_success_at,omitempty"`
	LastRunURL       string          `json:"last_run_url,omitempty"`
	LastError        string          `json:"last_error,omitempty"`
	NextRunAt        time.Time       `json:"next_run_at,omitempty"`
	Service          json.RawMessage `json:"service,omitempty"`
}

type managedDaemonDeletionLease struct {
	SchemaVersion    string    `json:"schema_version"`
	PID              int       `json:"pid"`
	StateDir         string    `json:"state_dir,omitempty"`
	Executable       string    `json:"executable"`
	HiveCommit       string    `json:"hive_commit,omitempty"`
	ExecutableSHA256 string    `json:"executable_sha256,omitempty"`
	AcquiredAt       time.Time `json:"acquired_at"`
}

func validateManagedIntegratedStateInventory(integratedDir, stateDir string, config Config) error {
	allowedFiles := map[string]bool{
		"audit.jsonl": true, "config.json": true, uninstallFinalizingConfigFile: true,
		"merge-approval.json": true, "merge-intent.json": true, pauseRequestFile: true,
		productionRunLeaseFile: true, legacyProductionRunLeaseFile: true,
		protectionActivationFile: true, authorizerTransferIntentFile: true,
		repairRefreshFile: true, repairRetirementFile: true, setupBaselineFile: true,
		setupBaselineRebindFile: true, uninstallIntentFile: true, workflowDispatchFile: true,
		schedulerStartIntentFile: true, hostedReleaseTransitionFile: true,
	}
	managedDaemonFiles := map[string]bool{"daemon.json": true, "daemon.lease": true, "daemon.log": true}
	entries, err := os.ReadDir(integratedDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(integratedDir, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to delete linked or unreadable integrated state entry %s", entry.Name())
		}
		if entry.IsDir() && entry.Name() == "checkouts" {
			continue
		}
		if entry.IsDir() && entry.Name() == "checkout" {
			if strings.TrimSpace(config.CheckoutDir) == "" || !sameFilesystemPath(path, config.CheckoutDir) {
				return fmt.Errorf("refusing to delete current managed checkout outside the exact configured path")
			}
			exists, checkoutErr := validateManagedCheckoutBeforeGit(path, config.Repository)
			if checkoutErr != nil {
				return fmt.Errorf("refusing to delete current managed checkout without exact ownership: %w", checkoutErr)
			}
			if !exists {
				return fmt.Errorf("refusing to delete missing current managed checkout")
			}
			continue
		}
		if !entry.IsDir() && info.Mode().IsRegular() && allowedFiles[entry.Name()] {
			continue
		}
		if !entry.IsDir() && info.Mode().IsRegular() && managedDaemonFiles[entry.Name()] {
			if err := validateManagedDaemonDeletionFile(path, entry.Name(), stateDir, config); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("refusing to delete unrelated integrated state entry %s", entry.Name())
	}
	return nil
}

func validateManagedDaemonDeletionFile(path, name, stateDir string, config Config) error {
	switch name {
	case "daemon.log":
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(path) || info.Size() < 0 || info.Size() > managedDaemonLogMaxBytes {
			return fmt.Errorf("refusing to delete daemon log that is linked, non-regular, or exceeds %d bytes", managedDaemonLogMaxBytes)
		}
		return nil
	case "daemon.json":
		data, err := readBoundedOrdinaryFile(path, managedDaemonStatusMaxBytes)
		if err != nil {
			return fmt.Errorf("refusing to delete invalid daemon status: %w", err)
		}
		var status managedDaemonDeletionStatus
		if err := decodeStrictManagedDaemonJSON(data, &status); err != nil {
			return fmt.Errorf("refusing to delete invalid daemon status: %w", err)
		}
		validSchema := status.SchemaVersion == "hive.integrated-daemon.v1" || (status.SchemaVersion == "hive.integrated-daemon.v2" && immutableCommit.MatchString(status.HiveCommit) && setupAuthorizationDigest.MatchString(status.ExecutableSHA256))
		if !validSchema || status.Running || status.RuntimeRunning || status.PID < 0 ||
			!sameFilesystemPath(status.StateDir, stateDir) || (strings.TrimSpace(status.Repository) != "" && !strings.EqualFold(status.Repository, config.Repository)) {
			return fmt.Errorf("refusing to delete daemon status that is active or does not bind this exact state and repository")
		}
		return nil
	case "daemon.lease":
		data, err := readBoundedOrdinaryFile(path, managedDaemonLeaseMaxBytes)
		if err != nil {
			return fmt.Errorf("refusing to delete invalid daemon lease: %w", err)
		}
		var lease managedDaemonDeletionLease
		if err := decodeStrictManagedDaemonJSON(data, &lease); err != nil {
			return fmt.Errorf("refusing to delete invalid daemon lease: %w", err)
		}
		validIntegratedSchema := lease.SchemaVersion == "hive.integrated-daemon-lease.v1" ||
			(lease.SchemaVersion == "hive.integrated-daemon-lease.v2" && immutableCommit.MatchString(lease.HiveCommit) && setupAuthorizationDigest.MatchString(lease.ExecutableSHA256))
		validNormalVisualSchema := lease.SchemaVersion == "hive.normal-visual-daemon-lease.v1" &&
			sameFilesystemPath(lease.StateDir, stateDir) &&
			immutableCommit.MatchString(lease.HiveCommit) &&
			setupAuthorizationDigest.MatchString(lease.ExecutableSHA256)
		validSchema := validIntegratedSchema || validNormalVisualSchema
		if !validSchema || lease.PID <= 0 || strings.TrimSpace(lease.Executable) == "" || lease.AcquiredAt.IsZero() {
			return fmt.Errorf("refusing to delete daemon lease with an invalid managed binding")
		}
		return nil
	default:
		return fmt.Errorf("refusing to delete unknown daemon state file %s", name)
	}
}

func decodeStrictManagedDaemonJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateManagedStateDeletionTree(root string, config Config) error {
	allowedDirectories := []string{"integrated", "visual-hive", "repair", "beads", "runtime"}
	for _, name := range allowedDirectories {
		path := filepath.Join(root, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := validateOrdinaryDeletionTree(path, 500000); err != nil {
			return fmt.Errorf("refusing to delete unsafe %s state tree: %w", name, err)
		}
	}
	checkouts := filepath.Join(root, "integrated", "checkouts")
	if _, err := os.Lstat(checkouts); err == nil {
		entries, readErr := os.ReadDir(checkouts)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			checkout := filepath.Join(checkouts, entry.Name())
			if !entry.IsDir() {
				return fmt.Errorf("refusing to delete non-directory managed checkout entry %s", entry.Name())
			}
			marker := filepath.Join(checkout, ".git", managedCheckoutOwnerFile)
			info, statErr := os.Lstat(marker)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(marker) {
				return fmt.Errorf("refusing to delete checkout %s without an ordinary Hive ownership marker", entry.Name())
			}
			data, readErr := os.ReadFile(marker)
			var checkoutOwner managedCheckoutOwner
			if readErr != nil || json.Unmarshal(data, &checkoutOwner) != nil || checkoutOwner.SchemaVersion != "hive.checkout-owner.v1" || !strings.EqualFold(checkoutOwner.Repository, config.Repository) {
				return fmt.Errorf("refusing to delete checkout %s with mismatched ownership", entry.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateOrdinaryDeletionTree(root string, maxEntries int) error {
	root = filepath.Clean(root)
	count := 0
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > maxEntries {
			return fmt.Errorf("entry count exceeds %d", maxEntries)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(path) || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("%s is a link, reparse point, or non-regular entry", path)
		}
		if info.IsDir() {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || filepath.Clean(resolved) != filepath.Clean(path) {
				return fmt.Errorf("directory %s resolves through filesystem indirection", path)
			}
		}
		return nil
	})
}

package integrated

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

const (
	uninstallIntentSchema         = "hive.uninstall-intent.v1"
	uninstallIntentFile           = "uninstall-intent.json"
	uninstallFinalizingConfigFile = "config.uninstall-finalizing.json"
	uninstallPrepared             = "prepared"
	uninstallPROpen               = "pr_open"
	uninstallAlreadyClean         = "already_clean"
	maxUninstallDrainFindings     = 100
	maxUninstallDrainPullsPerItem = 20
)

// UninstallIntent is the durable authority boundary between preparing a
// reviewable cleanup and permanently deleting local state. A delete-state flag
// on the preparation call is never carried forward as deletion authority.
type UninstallIntent struct {
	SchemaVersion         string    `json:"schema_version"`
	Phase                 string    `json:"phase"`
	Repository            string    `json:"repository"`
	RepositoryID          string    `json:"repository_id"`
	DefaultBranch         string    `json:"default_branch"`
	Branch                string    `json:"branch"`
	CleanupCommitSHA      string    `json:"cleanup_commit_sha"`
	BaseSHA               string    `json:"base_sha"`
	Marker                string    `json:"marker"`
	ManagedPathsDigest    string    `json:"managed_paths_digest"`
	ChangedFiles          []string  `json:"changed_files"`
	PRNumber              int       `json:"pr_number,omitempty"`
	PRURL                 string    `json:"pr_url,omitempty"`
	DiffDigest            string    `json:"diff_digest,omitempty"`
	PreviousSetupBranch   string    `json:"previous_setup_branch,omitempty"`
	PreviousSetupPRNumber int       `json:"previous_setup_pr_number,omitempty"`
	PreviousSetupPRURL    string    `json:"previous_setup_pr_url,omitempty"`
	PreparedAt            time.Time `json:"prepared_at"`
	PRRecordedAt          time.Time `json:"pr_recorded_at,omitempty"`
}

func (s *Store) SaveUninstallIntent(intent UninstallIntent) error {
	intent.SchemaVersion = uninstallIntentSchema
	intent.ChangedFiles = sortedUniquePaths(intent.ChangedFiles)
	if err := validateUninstallIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, uninstallIntentFile), append(data, '\n'))
}

func (s *Store) LoadUninstallIntent() (UninstallIntent, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, uninstallIntentFile))
	if os.IsNotExist(err) {
		return UninstallIntent{}, false, nil
	}
	if err != nil {
		return UninstallIntent{}, false, err
	}
	var intent UninstallIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return UninstallIntent{}, false, fmt.Errorf("decode uninstall intent: %w", err)
	}
	if err := validateUninstallIntent(intent); err != nil {
		return UninstallIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteUninstallIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, uninstallIntentFile))
}

func validateUninstallIntent(intent UninstallIntent) error {
	if intent.SchemaVersion != uninstallIntentSchema ||
		(intent.Phase != uninstallPrepared && intent.Phase != uninstallPROpen && intent.Phase != uninstallAlreadyClean) ||
		strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" || strings.TrimSpace(intent.DefaultBranch) == "" ||
		!strings.HasPrefix(intent.Branch, "hive/uninstall-") || !immutableCommit.MatchString(strings.ToLower(intent.CleanupCommitSHA)) ||
		!immutableCommit.MatchString(strings.ToLower(intent.BaseSHA)) || strings.TrimSpace(intent.Marker) == "" ||
		len(intent.ManagedPathsDigest) != sha256.Size*2 || intent.PreparedAt.IsZero() {
		return fmt.Errorf("uninstall intent is incomplete or invalid")
	}
	if _, err := hex.DecodeString(intent.ManagedPathsDigest); err != nil {
		return fmt.Errorf("uninstall intent managed-path digest is invalid")
	}
	if intent.PreviousSetupPRNumber < 0 || (intent.PreviousSetupPRNumber == 0) != (strings.TrimSpace(intent.PreviousSetupPRURL) == "") {
		return fmt.Errorf("uninstall intent previous setup pull identity is invalid")
	}
	if !exactPathOrder(intent.ChangedFiles, sortedUniquePaths(intent.ChangedFiles)) {
		return fmt.Errorf("uninstall intent changed paths are not canonical")
	}
	switch intent.Phase {
	case uninstallPROpen:
		if intent.PRNumber <= 0 || strings.TrimSpace(intent.PRURL) == "" || len(intent.DiffDigest) != sha256.Size*2 || intent.PRRecordedAt.IsZero() || len(intent.ChangedFiles) == 0 {
			return fmt.Errorf("recorded uninstall pull request evidence is incomplete")
		}
		if _, err := hex.DecodeString(intent.DiffDigest); err != nil {
			return fmt.Errorf("uninstall pull request diff digest is invalid")
		}
	case uninstallPrepared:
		if intent.PRNumber != 0 || intent.PRURL != "" || intent.DiffDigest != "" || len(intent.ChangedFiles) == 0 {
			return fmt.Errorf("prepared uninstall intent has invalid pull request state")
		}
	case uninstallAlreadyClean:
		if intent.PRNumber != 0 || intent.PRURL != "" || intent.DiffDigest != "" || len(intent.ChangedFiles) != 0 || !strings.EqualFold(intent.CleanupCommitSHA, intent.BaseSHA) {
			return fmt.Errorf("already-clean uninstall intent has invalid state")
		}
	}
	return nil
}

func newUninstallIntent(config Config, defaultBranch, branch, commitSHA, baseSHA, marker string, changedFiles []string) (UninstallIntent, error) {
	pathsDigest, err := managedPathPolicyDigest(config)
	if err != nil {
		return UninstallIntent{}, err
	}
	phase := uninstallPrepared
	if len(changedFiles) == 0 {
		phase = uninstallAlreadyClean
	}
	intent := UninstallIntent{
		SchemaVersion: uninstallIntentSchema, Phase: phase, Repository: config.Repository, RepositoryID: config.RepositoryID,
		DefaultBranch: defaultBranch, Branch: branch, CleanupCommitSHA: strings.ToLower(strings.TrimSpace(commitSHA)),
		BaseSHA: strings.ToLower(strings.TrimSpace(baseSHA)), Marker: marker, ManagedPathsDigest: pathsDigest,
		ChangedFiles: sortedUniquePaths(changedFiles), PreparedAt: time.Now().UTC(),
		PreviousSetupBranch: config.SetupBranch, PreviousSetupPRNumber: config.SetupPRNumber, PreviousSetupPRURL: config.SetupPRURL,
	}
	return intent, validateUninstallIntent(intent)
}

func resumeUninstall(ctx context.Context, options ManagementOptions, store *Store, config Config, intent UninstallIntent, result ManagementResult, release func()) (ManagementResult, error) {
	result.Repository, result.Branch, result.CommitSHA = config.Repository, intent.Branch, intent.CleanupCommitSHA
	result.PRNumber, result.PRURL = intent.PRNumber, intent.PRURL
	result.FinalizationPending = true
	result.NextCommand = UninstallNextCommand(options.StateDir)
	result.CancelCommand = UninstallCancelCommand(options.StateDir)
	if err := validateUninstallIntentBinding(intent, config); err != nil {
		return result, err
	}
	if intent.Phase == uninstallPrepared {
		completed, err := completePreparedUninstall(ctx, options, store, config, intent, result)
		if err != nil {
			return result, fmt.Errorf("prepared uninstall cleanup could not resume with its exact immutable binding: %w; cancel and restart through %s", err, result.CancelCommand)
		}
		return completed, nil
	}
	if !options.DeleteState {
		return result, nil
	}
	if err := finalizeUninstall(ctx, options, store, config, intent, &result); err != nil {
		return result, fmt.Errorf("uninstall finalization stopped with state preserved: %w; retry after reconciliation with %s, or cancel the exact unmerged cleanup through %s", err, result.NextCommand, result.CancelCommand)
	}
	if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_state_deletion", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("phase=%s pr=%d head=%s", intent.Phase, intent.PRNumber, intent.CleanupCommitSHA)}); err != nil {
		return result, err
	}
	if err := markStateDeletionFinalizing(store); err != nil {
		return result, err
	}
	// The lease file lives below the managed root. Every remote mutation and
	// every state proof is complete before releasing it; durable pause plus the
	// missing config marker makes a concurrent command fail closed during this
	// intentionally tiny cross-platform deletion window.
	release()
	if err := deleteManagedState(options.StateDir); err != nil {
		return result, err
	}
	result.FinalizationPending = false
	result.StateDeleted = true
	return result, nil
}

func cancelUninstall(ctx context.Context, options ManagementOptions, store *Store, config Config, intent UninstallIntent, result ManagementResult) (ManagementResult, error) {
	result.Repository, result.Branch, result.CommitSHA = config.Repository, intent.Branch, intent.CleanupCommitSHA
	result.PRNumber, result.PRURL = intent.PRNumber, intent.PRURL
	result.CancelCommand = UninstallCancelCommand(options.StateDir)
	if err := validateUninstallIntentBinding(intent, config); err != nil {
		return result, err
	}
	detail := fmt.Sprintf("phase=%s pr=%d url=%s branch=%s owned_head=%s cancel=true", intent.Phase, intent.PRNumber, intent.PRURL, intent.Branch, intent.CleanupCommitSHA)
	if err := store.AuditStrict(AuditEntry{Action: "uninstall_cancel_authorized", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if intent.Phase != uninstallAlreadyClean {
		snapshot, found, err := options.GitHub.CloseManagedPullRequestForCancellation(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.PRURL, intent.Marker, intent.Branch, intent.DefaultBranch)
		if err != nil {
			return result, fmt.Errorf("close exact unmerged uninstall cleanup PR for cancellation: %w", err)
		}
		if intent.Phase == uninstallPROpen && !found {
			return result, fmt.Errorf("recorded uninstall cleanup PR #%d disappeared; cancellation cannot prove its exact identity", intent.PRNumber)
		}
		if found {
			detail += fmt.Sprintf(" observed_pr=%d observed_head=%s observed_base=%s", snapshot.Number, snapshot.HeadSHA, snapshot.BaseSHA)
			if err := store.AuditStrict(AuditEntry{Action: "uninstall_cancel_pr_closed", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return result, err
			}
		}
	}
	deleted, retiredHead, err := options.GitHub.DeleteUninstallBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CleanupCommitSHA)
	if err != nil {
		return result, fmt.Errorf("retire exact uninstall cleanup branch for cancellation: %w", err)
	}
	if err := store.AuditStrict(AuditEntry{Action: "uninstall_cancel_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("%s retired_head=%s deleted=%t", detail, retiredHead, deleted)}); err != nil {
		return result, err
	}
	// Cancellation intentionally remains paused. The CLI stopped persistent
	// scheduling before entering management, and silently restarting authority
	// would be surprising. Restore the prior setup PR identity so status and a
	// later resume/restart are consistent with the still-installed default tree.
	config.Paused = true
	config.SetupBranch, config.SetupPRNumber, config.SetupPRURL = intent.PreviousSetupBranch, intent.PreviousSetupPRNumber, intent.PreviousSetupPRURL
	if err := store.Save(config); err != nil {
		return result, err
	}
	if err := store.AuditStrict(AuditEntry{Action: "uninstall_cancelled", Allowed: true, Repository: config.Repository, Detail: detail + " automation_remains_paused=true"}); err != nil {
		return result, err
	}
	if err := store.DeleteUninstallIntent(); err != nil {
		return result, err
	}
	result.Cancelled, result.FinalizationPending, result.Idempotent = true, false, false
	result.NextCommand, result.CancelCommand = "", ""
	return result, nil
}

func completePreparedUninstall(ctx context.Context, options ManagementOptions, store *Store, config Config, intent UninstallIntent, result ManagementResult) (ManagementResult, error) {
	policy := automation.Policy{ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), AllowedRepositories: []string{config.Repository}}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return result, err
	}
	if defaultBranch != intent.DefaultBranch {
		return result, fmt.Errorf("default branch changed from %s to %s", intent.DefaultBranch, defaultBranch)
	}
	if _, err := git(ctx, config.CheckoutDir, "cat-file", "-e", intent.CleanupCommitSHA+"^{commit}"); err != nil {
		return result, fmt.Errorf("prepared cleanup commit %s is no longer available locally", intent.CleanupCommitSHA)
	}
	snapshot, found, err := options.GitHub.FindManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.Marker, intent.Branch, intent.CleanupCommitSHA, intent.DefaultBranch)
	if err != nil {
		return result, err
	}
	if !found {
		if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPush); err != nil {
			return result, err
		}
		if err := pushManagedBranch(ctx, config.CheckoutDir, config.Repository, intent.Branch, config.RepositoryID, string(OperationUninstall), intent.CleanupCommitSHA); err != nil {
			return result, err
		}
		if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPR); err != nil {
			return result, err
		}
		pull, err := options.GitHub.UpsertRepairPullRequest(ctx, config.Repository, intent.Branch, intent.CleanupCommitSHA, intent.DefaultBranch, uninstallTitle(), uninstallBody(intent.Marker, config), intent.Marker)
		if err != nil {
			return result, err
		}
		snapshot, err = options.GitHub.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, pull.Number, intent.Marker, intent.Branch, intent.CleanupCommitSHA, intent.DefaultBranch)
		if err != nil {
			return result, err
		}
	}
	authorization, err := authorizeManagedSetupPullRequest(ctx, store, options.GitHub, policy, config, string(OperationUninstall), config.CheckoutDir, intent.Branch, intent.CleanupCommitSHA, snapshot.URL, snapshot.Number)
	if err != nil {
		return result, err
	}
	result.SetupAuthorizationContext, result.SetupAuthorizationStatusID = authorization.Status.Context, authorization.Status.StatusID
	result.SetupAuthorizationCreatorID, result.SetupAuthorizationReused = authorization.Status.CreatorID, authorization.Status.Reused
	updated, err := bindUninstallPullSnapshot(intent, snapshot)
	if err != nil {
		return result, err
	}
	if err := store.SaveUninstallIntent(updated); err != nil {
		return result, err
	}
	config.Paused, config.SetupBranch, config.SetupPRNumber, config.SetupPRURL = true, updated.Branch, updated.PRNumber, updated.PRURL
	if err := store.Save(config); err != nil {
		return result, err
	}
	if err := store.AuditStrict(AuditEntry{Action: "uninstall_prepared", Allowed: true, Repository: config.Repository, Detail: updated.PRURL}); err != nil {
		return result, err
	}
	result.PRNumber, result.PRURL = updated.PRNumber, updated.PRURL
	result.FinalizationPending, result.NextCommand = true, UninstallNextCommand(options.StateDir)
	return result, nil
}

func bindUninstallPullSnapshot(intent UninstallIntent, snapshot hivegithub.ManagedPullRequestSnapshot) (UninstallIntent, error) {
	if snapshot.Number <= 0 || !strings.EqualFold(snapshot.HeadSHA, intent.CleanupCommitSHA) || snapshot.HeadBranch != intent.Branch ||
		snapshot.BaseBranch != intent.DefaultBranch || !equalPaths(snapshot.ChangedFiles, intent.ChangedFiles) || len(snapshot.DiffDigest) != sha256.Size*2 {
		return intent, fmt.Errorf("cleanup pull request does not match the exact durable uninstall intent")
	}
	intent.Phase, intent.PRNumber, intent.PRURL, intent.BaseSHA, intent.DiffDigest, intent.PRRecordedAt =
		uninstallPROpen, snapshot.Number, snapshot.URL, strings.ToLower(snapshot.BaseSHA), strings.ToLower(snapshot.DiffDigest), time.Now().UTC()
	if err := validateUninstallIntent(intent); err != nil {
		return intent, err
	}
	return intent, nil
}

func finalizeUninstall(ctx context.Context, options ManagementOptions, store *Store, config Config, intent UninstallIntent, result *ManagementResult) error {
	if intent.Phase == uninstallPROpen {
		snapshot, err := options.GitHub.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.Marker, intent.Branch, intent.CleanupCommitSHA, intent.DefaultBranch)
		if err != nil {
			return err
		}
		if !snapshot.Merged || snapshot.State != "closed" || strings.TrimSpace(snapshot.MergeSHA) == "" {
			return fmt.Errorf("exact cleanup pull request #%d has not merged", intent.PRNumber)
		}
		if !strings.EqualFold(snapshot.BaseSHA, intent.BaseSHA) || !strings.EqualFold(snapshot.DiffDigest, intent.DiffDigest) || !equalPaths(snapshot.ChangedFiles, intent.ChangedFiles) {
			return fmt.Errorf("merged cleanup pull request base, diff, or changed paths no longer match the durable intent")
		}
	}
	currentHead, err := currentDefaultBranchHead(ctx, options.GitHub, config)
	if err != nil {
		return err
	}
	if err := VerifyUninstalledSetupAtCommit(ctx, options.GitHub, config, currentHead); err != nil {
		return err
	}
	if err := verifyUninstallQuiescence(options.StateDir, store); err != nil {
		return err
	}
	if err := reconcileUninstallProtection(ctx, options.GitHub, store, config); err != nil {
		return err
	}
	result.CleanupVerified = true
	result.ProtectionReconciled = true
	return nil
}

func validateUninstallIntentBinding(intent UninstallIntent, config Config) error {
	digest, err := managedPathPolicyDigest(config)
	if err != nil {
		return err
	}
	expectedLegacyMarker := fmt.Sprintf("<!-- hive-uninstall: %s -->", strings.ToLower(config.Repository))
	if !strings.EqualFold(intent.Repository, config.Repository) || intent.RepositoryID != strings.TrimSpace(config.RepositoryID) ||
		intent.DefaultBranch != config.DefaultBranch || (intent.Marker != expectedLegacyMarker && !validUninstallMarker(intent.Marker, config.Repository)) || intent.ManagedPathsDigest != digest ||
		intent.Branch != managedOperationBranch(string(OperationUninstall), config.RepositoryID) {
		return fmt.Errorf("durable uninstall intent no longer matches repository identity or managed path policy")
	}
	return nil
}

func verifyUninstallQuiescence(stateDir string, store *Store) error {
	if err := verifyUninstallRecoveryState(store); err != nil {
		return err
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(stateDir, "visual-hive"))
	if err != nil {
		return err
	}
	if pending := lifecycle.PendingOutbox(); len(pending) != 0 {
		return fmt.Errorf("Visual Hive issue outbox still has %d pending mutation(s)", len(pending))
	}
	if pending, err := pendingRepairRetirementCount(store); err != nil {
		return fmt.Errorf("read repair retirement outbox: %w", err)
	} else if pending != 0 {
		return fmt.Errorf("repair retirement outbox still has %d pending exact PR/branch mutation(s)", pending)
	}
	snapshot := lifecycle.Snapshot()
	for key, finding := range snapshot.Findings {
		if finding == nil || !terminalUninstallFinding(*finding) {
			return fmt.Errorf("finding %s has not reached a reconciled terminal lifecycle state", key)
		}
	}
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return err
	}
	for key, attempt := range repairStore.Snapshot().Attempts {
		finding := snapshot.Findings[key]
		if attempt == nil || finding == nil || !terminalUninstallFinding(*finding) || repairAttemptPending(*attempt) {
			return fmt.Errorf("repair attempt %s has unreconciled durable state", key)
		}
		switch attempt.Stage {
		case repair.StageCancelled:
		case repair.StageNoChange:
			if attempt.PRNumber != 0 {
				return fmt.Errorf("no-change repair attempt %s retains a pull request binding", key)
			}
		case repair.StagePROpen:
			if finding.MergeSHA == "" || finding.PRNumber != attempt.PRNumber || finding.Branch != attempt.Branch || !strings.EqualFold(finding.RepairCommitSHA, attempt.CommitSHA) {
				return fmt.Errorf("repair attempt %s is not reconciled to its terminal merged finding", key)
			}
		default:
			return fmt.Errorf("repair attempt %s remains in nonterminal stage %s", key, attempt.Stage)
		}
	}
	return nil
}

func verifyUninstallRecoveryState(store *Store) error {
	for name, state := range rangeIntegratedRecoveryState(store) {
		if state.err != nil {
			return state.err
		}
		if state.exists {
			return fmt.Errorf("integrated %s is still pending", name)
		}
	}
	return nil
}

func drainUninstallLifecycle(ctx context.Context, stateDir string, store *Store, config Config, client *hivegithub.Client) error {
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(stateDir, "visual-hive"))
	if err != nil {
		return err
	}
	if err := prepareRepairRetirements(store, config, lifecycle); err != nil {
		return fmt.Errorf("prepare repair retirement outbox for uninstall: %w", err)
	}
	if err := reconcileRepairRetirements(ctx, store, config, lifecycle, client, integratedPolicy(config), true); err != nil {
		return fmt.Errorf("drain repair retirement outbox while paused: %w", err)
	}
	repairStore, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return err
	}
	beadStore, err := beads.NewStore(filepath.Join(stateDir, "beads", "quality"))
	if err != nil {
		return err
	}
	snapshot := lifecycle.Snapshot()
	keys := make([]string, 0, len(snapshot.Findings))
	for key := range snapshot.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxUninstallDrainFindings {
		return fmt.Errorf("uninstall drain is bounded to %d findings; found %d", maxUninstallDrainFindings, len(keys))
	}
	pendingByFinding := map[string]bool{}
	for _, entry := range snapshot.Outbox {
		if entry != nil && entry.CompletedAt == nil {
			pendingByFinding[entry.RepositoryFingerprint] = true
		}
	}
	for _, key := range keys {
		finding := snapshot.Findings[key]
		if finding == nil {
			return fmt.Errorf("finding %s is missing durable state", key)
		}
		if err := drainFindingRepairResources(ctx, store, lifecycle, config, client, *finding); err != nil {
			return err
		}
		issueClosed := finding.IssueNumber > 0
		if !issueClosed && pendingByFinding[finding.RepositoryFingerprint] {
			marker, err := uninstallLifecycleMarker(*finding)
			if err != nil {
				return err
			}
			detail := fmt.Sprintf("finding=%s marker=%s ambiguous_issue_number=true", finding.RepositoryFingerprint, marker)
			if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_recover_close_issue", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return err
			}
			writerLogin, writerID := finding.IssueWriterLogin, finding.IssueWriterID
			if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
				writerLogin, writerID, err = client.ResolveLifecycleIssueWriter(ctx, config.Repository, marker, 0)
				if err != nil {
					return err
				}
				if err := lifecycle.BindIssueWriter(finding.RepositoryFingerprint, writerID, writerLogin); err != nil {
					return err
				}
				finding.IssueWriterID, finding.IssueWriterLogin = writerID, writerLogin
			}
			number, url, found, err := client.CloseLifecycleIssueByMarkerExactOwned(ctx, config.Repository, marker, writerLogin, writerID)
			if err != nil {
				return err
			}
			if found {
				if err := lifecycle.MarkIssueOpened(finding.RepositoryFingerprint, number, url); err != nil {
					return err
				}
				finding.IssueNumber, finding.IssueURL, issueClosed = number, url, true
			}
		}
		if issueClosed {
			marker, err := uninstallLifecycleMarker(*finding)
			if err != nil {
				return err
			}
			detail := fmt.Sprintf("finding=%s issue=%d marker=%s", finding.RepositoryFingerprint, finding.IssueNumber, marker)
			if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_close_issue", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return err
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "uninstall_close_issue", true, detail)
			writerLogin, writerID := finding.IssueWriterLogin, finding.IssueWriterID
			if writerID <= 0 || strings.TrimSpace(writerLogin) == "" {
				writerLogin, writerID, err = client.ResolveLifecycleIssueWriter(ctx, config.Repository, marker, finding.IssueNumber)
				if err != nil {
					return err
				}
				if err := lifecycle.BindIssueWriter(finding.RepositoryFingerprint, writerID, writerLogin); err != nil {
					return err
				}
				finding.IssueWriterID, finding.IssueWriterLogin = writerID, writerLogin
			}
			if err := client.CloseLifecycleIssueExactOwned(ctx, config.Repository, finding.IssueNumber, marker, writerLogin, writerID); err != nil {
				return err
			}
		}
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_cancel_finding", Allowed: true, Repository: config.Repository, Detail: "finding=" + finding.RepositoryFingerprint}); err != nil {
			return err
		}
		if err := lifecycle.CancelForUninstall(finding.RepositoryFingerprint, issueClosed, beadStore); err != nil {
			return err
		}
	}
	attempts := repairStore.Snapshot().Attempts
	attemptKeys := make([]string, 0, len(attempts))
	for key := range attempts {
		attemptKeys = append(attemptKeys, key)
	}
	sort.Strings(attemptKeys)
	if len(attemptKeys) > maxUninstallDrainFindings {
		return fmt.Errorf("uninstall drain is bounded to %d repair attempts; found %d", maxUninstallDrainFindings, len(attemptKeys))
	}
	for _, key := range attemptKeys {
		attempt := attempts[key]
		if attempt == nil {
			return fmt.Errorf("repair attempt %s is missing durable state", key)
		}
		if err := drainRepairAttemptResources(ctx, store, config, client, *attempt); err != nil {
			return err
		}
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_cancel_repair", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s stage=%s", key, attempt.Stage)}); err != nil {
			return err
		}
		if err := repairStore.CancelForUninstall(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func drainFindingRepairResources(ctx context.Context, store *Store, lifecycle *visualhive.LifecycleStore, config Config, client *hivegithub.Client, finding visualhive.FindingLifecycle) error {
	marker := fmt.Sprintf("<!-- hive-repair: %s -->", finding.RepositoryFingerprint)
	pulls, err := client.ListOpenRepairPullRequests(ctx, config.Repository, config.DefaultBranch, marker)
	if err != nil {
		return err
	}
	if len(pulls) > maxUninstallDrainPullsPerItem {
		return fmt.Errorf("uninstall drain is bounded to %d repair pull requests per finding; found %d for %s", maxUninstallDrainPullsPerItem, len(pulls), finding.RepositoryFingerprint)
	}
	seen := map[int]bool{}
	for _, pull := range pulls {
		seen[pull.Number] = true
		if err := closeAndDeleteUninstallRepair(ctx, store, lifecycle, config, client, finding.RepositoryFingerprint, marker, pull.Number, pull.Branch, pull.HeadSHA); err != nil {
			return err
		}
	}
	if finding.PRNumber > 0 {
		if finding.Branch == "" || finding.RepairCommitSHA == "" {
			return fmt.Errorf("finding %s has an incomplete exact repair PR binding", finding.RepositoryFingerprint)
		}
		if !seen[finding.PRNumber] {
			if err := closeAndDeleteUninstallRepair(ctx, store, lifecycle, config, client, finding.RepositoryFingerprint, marker, finding.PRNumber, finding.Branch, finding.RepairCommitSHA); err != nil {
				return err
			}
		}
	} else if finding.Branch != "" || finding.RepairCommitSHA != "" {
		if finding.Branch == "" || finding.RepairCommitSHA == "" {
			return fmt.Errorf("finding %s has an incomplete repair branch binding", finding.RepositoryFingerprint)
		}
		if err := deleteUninstallRepairBranch(ctx, store, lifecycle, config, client, finding.RepositoryFingerprint, finding.Branch, finding.RepairCommitSHA); err != nil {
			return err
		}
	}
	return nil
}

func closeAndDeleteUninstallRepair(ctx context.Context, store *Store, lifecycle *visualhive.LifecycleStore, config Config, client *hivegithub.Client, fingerprint, marker string, number int, branch, headSHA string) error {
	detail := fmt.Sprintf("finding=%s pr=%d branch=%s head=%s", fingerprint, number, branch, headSHA)
	if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_close_repair_pr", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	lifecycle.RecordAuthorization(fingerprint, "uninstall_close_repair_pr", true, detail)
	if err := client.CloseRepairPullRequestExact(ctx, config.Repository, number, marker, branch, headSHA); err != nil {
		return err
	}
	return deleteUninstallRepairBranch(ctx, store, lifecycle, config, client, fingerprint, branch, headSHA)
}

func deleteUninstallRepairBranch(ctx context.Context, store *Store, lifecycle *visualhive.LifecycleStore, config Config, client *hivegithub.Client, fingerprint, branch, headSHA string) error {
	detail := fmt.Sprintf("finding=%s branch=%s head=%s", fingerprint, branch, headSHA)
	if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_delete_repair_branch", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	lifecycle.RecordAuthorization(fingerprint, "uninstall_delete_repair_branch", true, detail)
	return client.DeleteRepairBranchExact(ctx, config.Repository, branch, headSHA)
}

func drainRepairAttemptResources(ctx context.Context, store *Store, config Config, client *hivegithub.Client, attempt repair.Attempt) error {
	marker := fmt.Sprintf("<!-- hive-repair: %s -->", attempt.RepositoryFingerprint)
	if attempt.PRNumber > 0 {
		if attempt.Branch == "" || attempt.CommitSHA == "" {
			return fmt.Errorf("repair attempt %s has an incomplete exact PR binding", attempt.RepositoryFingerprint)
		}
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_close_repair_pr", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s pr=%d branch=%s head=%s", attempt.RepositoryFingerprint, attempt.PRNumber, attempt.Branch, attempt.CommitSHA)}); err != nil {
			return err
		}
		if err := client.CloseRepairPullRequestExact(ctx, config.Repository, attempt.PRNumber, marker, attempt.Branch, attempt.CommitSHA); err != nil {
			return err
		}
	}
	if attempt.Branch != "" && attempt.CommitSHA != "" {
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_delete_repair_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s branch=%s head=%s", attempt.RepositoryFingerprint, attempt.Branch, attempt.CommitSHA)}); err != nil {
			return err
		}
		if err := client.DeleteRepairBranchExact(ctx, config.Repository, attempt.Branch, attempt.CommitSHA); err != nil {
			return err
		}
	}
	if review := attempt.BaselineReview; review != nil && review.ProposalPRNumber > 0 {
		if review.ProposalBranch == "" || review.ProposalCommitSHA == "" || review.RepairHeadSHA == "" {
			return fmt.Errorf("repair attempt %s has an incomplete baseline proposal binding", attempt.RepositoryFingerprint)
		}
		baselineMarker := fmt.Sprintf("<!-- hive-baseline-review: %s:%s -->", attempt.RepositoryFingerprint, review.RepairHeadSHA)
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_close_baseline_pr", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s pr=%d branch=%s head=%s", attempt.RepositoryFingerprint, review.ProposalPRNumber, review.ProposalBranch, review.ProposalCommitSHA)}); err != nil {
			return err
		}
		if err := client.CloseRepairPullRequestExact(ctx, config.Repository, review.ProposalPRNumber, baselineMarker, review.ProposalBranch, review.ProposalCommitSHA); err != nil {
			return err
		}
		if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_delete_baseline_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s branch=%s head=%s", attempt.RepositoryFingerprint, review.ProposalBranch, review.ProposalCommitSHA)}); err != nil {
			return err
		}
		if _, err := client.DeleteBaselineBranchExact(ctx, config.Repository, review.ProposalBranch, review.ProposalCommitSHA); err != nil {
			return err
		}
	}
	return nil
}

func uninstallLifecycleMarker(finding visualhive.FindingLifecycle) (string, error) {
	if finding.PendingMarkerMigrationFrom != "" {
		return finding.PendingMarkerMigrationFrom, nil
	}
	fingerprint := finding.RepositoryFingerprint
	if finding.RootCauseKey != "" {
		fingerprint = finding.PublicationFingerprint
	}
	if len(fingerprint) != sha256.Size*2 {
		return "", fmt.Errorf("finding %s has no exact lifecycle issue marker binding", finding.RepositoryFingerprint)
	}
	return fmt.Sprintf("<!-- hive-visual-fingerprint: %s -->", fingerprint), nil
}

func rangeIntegratedRecoveryState(store *Store) map[string]struct {
	exists bool
	err    error
} {
	_, workflow, workflowErr := store.LoadWorkflowDispatchIntent()
	_, merge, mergeErr := store.LoadMergeIntent()
	_, approval, approvalErr := store.LoadMergeApproval()
	_, refresh, refreshErr := store.LoadRepairRefreshIntent()
	_, transfer, transferErr := store.LoadAuthorizerTransferIntent()
	_, setupBaseline, setupBaselineErr := store.LoadSetupBaselineIntent()
	_, setupBaselineRebind, setupBaselineRebindErr := store.LoadSetupBaselineRebindIntent()
	return map[string]struct {
		exists bool
		err    error
	}{
		"workflow dispatch intent":  {workflow, workflowErr},
		"merge intent":              {merge, mergeErr},
		"merge approval":            {approval, approvalErr},
		"repair refresh intent":     {refresh, refreshErr},
		"setup authorizer transfer": {transfer, transferErr},
		"setup baseline intent":     {setupBaseline, setupBaselineErr},
		"setup baseline rebind":     {setupBaselineRebind, setupBaselineRebindErr},
	}
}

func terminalUninstallFinding(finding visualhive.FindingLifecycle) bool {
	if finding.Status == visualhive.StatusCancelled {
		return finding.ResolvedAt != nil && (finding.IssueNumber == 0 || finding.ClosedAt != nil)
	}
	if finding.Status == visualhive.StatusIssueClosed {
		return finding.IssueNumber > 0 && finding.ClosedAt != nil
	}
	return finding.Status == visualhive.StatusResolved && finding.IssueNumber == 0 && finding.ResolvedAt != nil
}

func repairAttemptPending(attempt repair.Attempt) bool {
	if attempt.Stage == repair.StageCancelled {
		return !repair.UninstallRefsRetired(attempt)
	}
	if attempt.ResumeStage != "" || attempt.RetryTransactionID != "" || attempt.PreparationCleanupPending || attempt.PreparationCleanupRef != "" ||
		attempt.ToolSnapshotRef != "" || attempt.ToolSnapshotCommit != "" || attempt.ToolSnapshotHead != "" || attempt.ToolSnapshotPhase != "" ||
		attempt.ModelBaseGuardRef != "" || attempt.ModelBaseGuardCommit != "" || attempt.CandidateGuardRef != "" || attempt.CandidateGuardCommit != "" ||
		attempt.DiscardDirtyBranch != "" || attempt.LegacyOwnershipAdoption || attempt.LegacyUnsealedCheckpoint {
		return true
	}
	if attempt.BaselineReview != nil && attempt.BaselineReview.Status != repair.BaselineReviewApproved && attempt.BaselineReview.Status != repair.BaselineReviewRejected {
		return true
	}
	return false
}

func reconcileUninstallProtection(ctx context.Context, client *hivegithub.Client, store *Store, config Config) error {
	activation, exists, err := store.LoadProtectionActivation()
	if err != nil {
		return err
	}
	present, err := client.RequiredCheckContextPresent(ctx, config.Repository, config.DefaultBranch, visualHivePRCheckContext)
	if err != nil {
		return fmt.Errorf("verify Visual Hive branch-protection cleanup: %w", err)
	}
	if !present {
		return store.AuditStrict(AuditEntry{Action: "uninstall_protection_reconciled", Allowed: true, Repository: config.Repository, Detail: "branch=" + config.DefaultBranch + " required_context_already_absent=true"})
	}
	if exists && !ProtectionActivationMatchesConfig(activation, config) {
		return fmt.Errorf("protection activation record no longer matches the installed repository binding")
	}
	if present {
		return fmt.Errorf("branch protection or a ruleset still requires %q; GitHub provides no conditional exact-context mutation, so remove only the exact required context while preserving every unrelated check and control, then retry", visualHivePRCheckContext)
	}
	return store.AuditStrict(AuditEntry{Action: "uninstall_protection_reconciled", Allowed: true, Repository: config.Repository, Detail: "branch=" + config.DefaultBranch})
}

func currentDefaultBranchHead(ctx context.Context, client *hivegithub.Client, config Config) (string, error) {
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return "", fmt.Errorf("repository identity is invalid")
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, repo, config.DefaultBranch, 0)
	if err != nil {
		return "", fmt.Errorf("read current default branch: %w", err)
	}
	sha := strings.ToLower(strings.TrimSpace(branch.GetCommit().GetSHA()))
	if !immutableCommit.MatchString(sha) {
		return "", fmt.Errorf("current default branch returned no immutable commit")
	}
	return sha, nil
}

// VerifyUninstalledSetupAtCommit is the inverse of installed setup
// verification. Every managed read is bound to one immutable target commit.
// Hive-owned paths must return an exact 404 while repository-owned preimages
// must be restored byte-for-byte; an API error or directory response proves
// neither condition.
func VerifyUninstalledSetupAtCommit(ctx context.Context, client *hivegithub.Client, config Config, commitSHA string) error {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if client == nil || client.GoGitHub() == nil || !immutableCommit.MatchString(commitSHA) {
		return fmt.Errorf("GitHub client and exact target commit are required to verify uninstall")
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("repository identity is invalid")
	}
	if managedPathPreimagesConfigured(config) && !hasValidManagedPathPreimages(config) {
		return fmt.Errorf("uninstall verification refuses an invalid managed-path preimage ledger")
	}
	preimagesValid := hasValidManagedPathPreimages(config)
	for _, relative := range managedSetupFilesForConfig(config) {
		content, directory, response, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, relative, &gh.RepositoryContentGetOptions{Ref: commitSHA})
		preimage := config.ManagedPathPreimages[relative]
		if preimagesValid && preimage.Existed {
			if err != nil || content == nil || len(directory) != 0 {
				return fmt.Errorf("repository-owned setup path %s was not restored at target commit %s", relative, commitSHA)
			}
			value, decodeErr := content.GetContent()
			if decodeErr != nil {
				return fmt.Errorf("decode restored repository-owned setup path %s: %w", relative, decodeErr)
			}
			digest := sha256.Sum256([]byte(value))
			if !bytes.Equal([]byte(value), preimage.Content) || !strings.EqualFold(preimage.ContentSHA256, hex.EncodeToString(digest[:])) {
				return fmt.Errorf("repository-owned setup path %s was not restored byte-for-byte at target commit %s", relative, commitSHA)
			}
			continue
		}
		if err == nil || content != nil || len(directory) != 0 {
			return fmt.Errorf("Hive-owned managed setup path %s still exists at target commit %s", relative, commitSHA)
		}
		if response == nil || response.StatusCode != http.StatusNotFound {
			return fmt.Errorf("verify absence of managed setup path %s: %w", relative, err)
		}
	}
	return nil
}

func uninstallTitle() string { return "Uninstall Hive production automation" }

func newUninstallMarker(repository string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := cryptorand.Read(nonce); err != nil {
		return "", fmt.Errorf("create uninstall cleanup identity: %w", err)
	}
	return fmt.Sprintf("<!-- hive-uninstall: %s:%s -->", strings.ToLower(strings.TrimSpace(repository)), hex.EncodeToString(nonce)), nil
}

func validUninstallMarker(marker, repository string) bool {
	prefix := fmt.Sprintf("<!-- hive-uninstall: %s:", strings.ToLower(strings.TrimSpace(repository)))
	if !strings.HasPrefix(marker, prefix) || !strings.HasSuffix(marker, " -->") {
		return false
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(marker, prefix), " -->")
	if len(nonce) != 32 {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}

func uninstallBody(marker string, config Config) string {
	return fmt.Sprintf("%s\n\nHive-managed `uninstall` operation.\n\n- Previous Visual Hive ref: `%s`\n- Requested Visual Hive ref: ``\n\nThis PR is intentionally reviewable and idempotent. Hive remains paused immediately for uninstall; upgrade and rollback take effect after this PR is merged.", marker, config.VisualHiveRef)
}

// UninstallNextCommand returns the exact post-merge finalization command.
func UninstallNextCommand(stateDir string) string {
	return "hive uninstall --state-dir " + strconv.Quote(strings.TrimSpace(stateDir)) + " --delete-state --json"
}

// UninstallCancelCommand returns the exact audited pre-merge cancellation path.
func UninstallCancelCommand(stateDir string) string {
	return "hive uninstall --state-dir " + strconv.Quote(strings.TrimSpace(stateDir)) + " --cancel --json"
}

func acquireUninstallDeletionGuard(stateDir string) (func(), error) {
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	path := filepath.Join(filepath.Dir(absolute), fmt.Sprintf(".hive-uninstall-%x.lock", digest[:8]))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := tryLockProductionRunLease(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !locked {
		_ = file.Close()
		return nil, ErrRunInProgress
	}
	return func() {
		_ = unlockProductionRunLease(file)
		_ = file.Close()
	}, nil
}

func markStateDeletionFinalizing(store *Store) error {
	if store == nil {
		return fmt.Errorf("integrated state store is required")
	}
	marker := filepath.Join(store.dir, uninstallFinalizingConfigFile)
	if _, err := os.Stat(marker); err == nil {
		return fmt.Errorf("uninstall state deletion marker already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := durableReplaceStateFile(store.configPath, marker); err != nil {
		return fmt.Errorf("mark uninstall state deletion finalizing: %w", err)
	}
	return nil
}

func restoreInterruptedStateDeletion(store *Store) error {
	if store == nil {
		return fmt.Errorf("integrated state store is required")
	}
	if _, err := os.Stat(store.configPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	marker := filepath.Join(store.dir, uninstallFinalizingConfigFile)
	if _, err := os.Stat(marker); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := durableReplaceStateFile(marker, store.configPath); err != nil {
		return fmt.Errorf("restore interrupted uninstall finalization: %w", err)
	}
	return nil
}

func digestPaths(paths []string) (string, error) {
	data, err := json.Marshal(sortedUniquePaths(paths))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func sortedUniquePaths(paths []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimPrefix(strings.TrimSpace(filepath.ToSlash(path)), "./")
		if path != "" && !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	sort.Strings(result)
	return result
}

func equalPaths(left, right []string) bool {
	left, right = sortedUniquePaths(left), sortedUniquePaths(right)
	return exactPathOrder(left, right)
}

func exactPathOrder(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

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
	maxUninstallLifecycleStores   = 64
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
	if err := synchronizeUninstallVerificationHead(ctx, config, currentHead); err != nil {
		return err
	}
	if err := verifyUninstalledSetupOrPendingProposalAtCommit(ctx, options.GitHub, config, currentHead); err != nil {
		return err
	}
	if err := verifyUninstallQuiescence(options.StateDir, store); err != nil {
		return err
	}
	if err := reconcileUninstallProtection(ctx, options.GitHub, store, config); err != nil {
		return err
	}
	if err := retirePreviousSetupReviewForUninstall(ctx, options.GitHub, store, config, intent); err != nil {
		return err
	}
	if err := retireUninstallCleanupBranch(ctx, options.GitHub, store, config, intent); err != nil {
		return err
	}
	result.CleanupVerified = true
	result.ProtectionReconciled = true
	return nil
}

func retireUninstallCleanupBranch(ctx context.Context, client *hivegithub.Client, store *Store, config Config, intent UninstallIntent) error {
	if client == nil || store == nil {
		return fmt.Errorf("uninstall cleanup branch retirement requires GitHub and audit stores")
	}
	detail := fmt.Sprintf("branch=%s owned_head=%s", intent.Branch, intent.CleanupCommitSHA)
	if err := store.AuditStrict(AuditEntry{
		Action: "authorize_uninstall_delete_cleanup_branch", Allowed: true, Repository: config.Repository, Detail: detail,
	}); err != nil {
		return err
	}
	deleted, retiredHead, err := client.DeleteUninstallBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.CleanupCommitSHA)
	if err != nil {
		return fmt.Errorf("retire exact uninstall cleanup branch after verified finalization: %w", err)
	}
	return store.AuditStrict(AuditEntry{
		Action: "uninstall_cleanup_branch_retired", Allowed: true, Repository: config.Repository,
		Detail: fmt.Sprintf("%s retired_head=%s deleted=%t", detail, retiredHead, deleted),
	})
}

func retirePreviousSetupReviewForUninstall(ctx context.Context, client *hivegithub.Client, store *Store, config Config, intent UninstallIntent) error {
	if intent.PreviousSetupPRNumber == 0 {
		return nil
	}
	expectedBranch := managedOperationBranch("setup", config.RepositoryID)
	if client == nil || store == nil ||
		intent.PreviousSetupBranch != expectedBranch ||
		strings.TrimSpace(intent.PreviousSetupPRURL) == "" ||
		!immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(config.SetupHeadSHA))) {
		return fmt.Errorf("previous setup pull request lacks an exact managed branch, URL, or head binding")
	}
	marker := "<!-- hive-setup: " + strings.ToLower(strings.TrimSpace(config.Repository)) + " -->"
	snapshot, err := client.InspectManagedPullRequestExact(
		ctx, config.Repository, config.RepositoryID, intent.PreviousSetupPRNumber,
		marker, intent.PreviousSetupBranch, config.SetupHeadSHA, config.DefaultBranch,
	)
	if err != nil {
		return fmt.Errorf("inspect previous setup pull request before uninstall retirement: %w", err)
	}
	if snapshot.URL != intent.PreviousSetupPRURL {
		return fmt.Errorf("previous setup pull request URL no longer matches its durable uninstall binding")
	}
	closed := snapshot.State == "closed"
	if !snapshot.Merged {
		if snapshot.State != "open" && snapshot.State != "closed" {
			return fmt.Errorf("previous setup pull request has unsupported state %q", snapshot.State)
		}
		if err := store.AuditStrict(AuditEntry{
			Action: "authorize_uninstall_close_setup_pr", Allowed: true, Repository: config.Repository,
			Detail: fmt.Sprintf("pr=%d head=%s branch=%s", snapshot.Number, snapshot.HeadSHA, snapshot.HeadBranch),
		}); err != nil {
			return err
		}
		closedSnapshot, found, err := client.CloseManagedPullRequestForCancellation(
			ctx, config.Repository, config.RepositoryID, intent.PreviousSetupPRNumber,
			intent.PreviousSetupPRURL, marker, intent.PreviousSetupBranch, config.DefaultBranch,
		)
		if err != nil {
			return fmt.Errorf("close previous unmerged setup pull request during uninstall: %w", err)
		}
		if !found || closedSnapshot.Number != snapshot.Number || !strings.EqualFold(closedSnapshot.HeadSHA, snapshot.HeadSHA) {
			return fmt.Errorf("previous setup pull request did not close with its exact managed identity")
		}
		closed = true
	}
	if err := store.AuditStrict(AuditEntry{
		Action: "authorize_uninstall_delete_setup_branch", Allowed: true, Repository: config.Repository,
		Detail: fmt.Sprintf("branch=%s owned_head=%s", intent.PreviousSetupBranch, config.SetupHeadSHA),
	}); err != nil {
		return err
	}
	deleted, retiredHead, err := client.DeleteSetupBranchDescendantExact(ctx, config.Repository, intent.PreviousSetupBranch, config.SetupHeadSHA)
	if err != nil {
		return fmt.Errorf("retire previous setup branch during uninstall: %w", err)
	}
	return store.AuditStrict(AuditEntry{
		Action: "uninstall_setup_review_retired", Allowed: true, Repository: config.Repository,
		Detail: fmt.Sprintf("pr=%d merged=%t closed=%t branch_deleted=%t retired_head=%s", snapshot.Number, snapshot.Merged, closed, deleted, retiredHead),
	})
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

// restoreSetupBaselinePreimages returns the exact baseline paths that uninstall
// must stage. New baselines are removed; repository-owned baselines are
// restored from the immutable base of the exact managed setup PR. The durable
// initial inventory is verified before any worktree mutation.
func restoreSetupBaselinePreimages(ctx context.Context, client *hivegithub.Client, checkout string, config Config) ([]string, error) {
	initialDigest := strings.ToLower(strings.TrimSpace(config.SetupBaselineInitialDigest))
	if initialDigest == "" {
		// Legacy installations did not record a baseline inventory. Preserve
		// their historical behavior rather than guessing ownership.
		return nil, nil
	}
	recomputed, err := setupBaselineCandidateDigest(config.SetupBaselineInitialCandidates)
	if err != nil || !strings.EqualFold(recomputed, initialDigest) {
		return nil, fmt.Errorf("durable initial baseline inventory is invalid")
	}
	currentSHA, err := git(ctx, checkout, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve current baseline inventory commit: %w", err)
	}
	currentCandidates, _, err := setupBaselineInventoryAtCommit(ctx, checkout, strings.ToLower(strings.TrimSpace(currentSHA)))
	if err != nil {
		return nil, fmt.Errorf("inspect current baseline inventory: %w", err)
	}
	initialByPath := make(map[string]SetupBaselineCandidate, len(config.SetupBaselineInitialCandidates))
	paths := make([]string, 0, len(currentCandidates)+len(config.SetupBaselineInitialCandidates))
	for _, candidate := range config.SetupBaselineInitialCandidates {
		initialByPath[candidate.Path] = candidate
		paths = append(paths, candidate.Path)
	}
	for _, candidate := range currentCandidates {
		paths = append(paths, candidate.Path)
	}
	paths = sortedUniquePaths(paths)
	if len(paths) == 0 {
		return nil, nil
	}

	preinstallSHA := ""
	if len(config.SetupBaselineInitialCandidates) > 0 {
		if client == nil || config.SetupPRNumber <= 0 || strings.TrimSpace(config.SetupPRURL) == "" ||
			!immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(config.SetupHeadSHA))) {
			return nil, fmt.Errorf("repository-owned baseline restoration requires the exact managed setup PR binding")
		}
		marker := "<!-- hive-setup: " + strings.ToLower(strings.TrimSpace(config.Repository)) + " -->"
		snapshot, inspectErr := client.InspectManagedPullRequestExact(
			ctx, config.Repository, config.RepositoryID, config.SetupPRNumber,
			marker, config.SetupBranch, config.SetupHeadSHA, config.DefaultBranch,
		)
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect exact setup PR baseline preimage: %w", inspectErr)
		}
		if snapshot.URL != config.SetupPRURL || !immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(snapshot.BaseSHA))) {
			return nil, fmt.Errorf("managed setup PR baseline preimage binding is invalid")
		}
		preinstallSHA = strings.ToLower(snapshot.BaseSHA)
		preinstallCandidates, preinstallDigest, inventoryErr := setupBaselineInventoryAtCommit(ctx, checkout, preinstallSHA)
		if inventoryErr != nil {
			return nil, fmt.Errorf("inspect setup PR base baseline inventory: %w", inventoryErr)
		}
		if !strings.EqualFold(preinstallDigest, initialDigest) || !equalSetupBaselineCandidates(preinstallCandidates, config.SetupBaselineInitialCandidates) {
			return nil, fmt.Errorf("setup PR base no longer matches the durable initial baseline inventory")
		}
	}

	for _, relative := range paths {
		if _, existed := initialByPath[relative]; existed {
			if _, err := git(ctx, checkout, "restore", "--source="+preinstallSHA, "--staged", "--worktree", "--", relative); err != nil {
				return nil, fmt.Errorf("restore repository-owned baseline %s: %w", relative, err)
			}
			continue
		}
		if err := os.Remove(filepath.Join(checkout, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove Hive-created baseline %s: %w", relative, err)
		}
	}
	return paths, nil
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

func drainWorkflowDispatchForUninstall(ctx context.Context, store *Store, config Config, client *hivegithub.Client) error {
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil || !exists {
		return err
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID {
		return fmt.Errorf("workflow dispatch state no longer matches installed repository authority")
	}
	if intent.RunID <= 0 || strings.TrimSpace(intent.RunURL) == "" || intent.MatchedAt.IsZero() {
		return fmt.Errorf("workflow dispatch %s is not bound to one exact run; recover it before uninstall", intent.CorrelationID)
	}
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if client == nil || client.GoGitHub() == nil || !ok || owner == "" || repository == "" {
		return fmt.Errorf("GitHub authentication and an exact repository are required to retire a workflow dispatch")
	}
	run, _, err := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repository, intent.RunID)
	if err != nil {
		return fmt.Errorf("read exact workflow dispatch before uninstall: %w", err)
	}
	if err := validateExactWorkflowRun(run, intent); err != nil {
		return fmt.Errorf("verify exact workflow dispatch before uninstall: %w", err)
	}
	if run.GetHTMLURL() != intent.RunURL || run.GetStatus() != "completed" || strings.TrimSpace(run.GetConclusion()) == "" {
		return fmt.Errorf("workflow dispatch %d is not the exact completed run recorded by Hive", intent.RunID)
	}
	detail := fmt.Sprintf(
		"operation=%s correlation=%s run=%d url=%s conclusion=%s head=%s matched_at=%s",
		intent.Operation, intent.CorrelationID, intent.RunID, intent.RunURL, run.GetConclusion(), run.GetHeadSHA(), intent.MatchedAt.Format(time.RFC3339Nano),
	)
	if err := store.AuditStrict(AuditEntry{
		Action: "authorize_uninstall_workflow_dispatch_retirement", Allowed: true, Repository: config.Repository, Detail: detail,
	}); err != nil {
		return err
	}
	if err := store.DeleteWorkflowDispatchIntent(); err != nil {
		return fmt.Errorf("retire exact workflow dispatch before uninstall: %w", err)
	}
	return nil
}

func drainUninstallLifecycle(ctx context.Context, stateDir string, lifecycleBeadsDirs []string, store *Store, config Config, client *hivegithub.Client) error {
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
	attempts := repairStore.Snapshot().Attempts
	beadStores, err := openUninstallLifecycleBeadStores(stateDir, lifecycleBeadsDirs)
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
		if err := drainFindingRepairResources(ctx, store, lifecycle, config, client, *finding, attempts[finding.RepositoryFingerprint]); err != nil {
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
		var beadStore *beads.Store
		if strings.TrimSpace(finding.BeadID) != "" {
			beadStore, err = selectUninstallLifecycleBeadStore(beadStores, finding.BeadID)
			if err != nil {
				return err
			}
		}
		if err := lifecycle.CancelForUninstall(finding.RepositoryFingerprint, issueClosed, beadStore); err != nil {
			return err
		}
	}
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

func openUninstallLifecycleBeadStores(stateDir string, configuredDirs []string) ([]*beads.Store, error) {
	if len(configuredDirs) == 0 {
		store, err := beads.NewStore(filepath.Join(stateDir, "beads", "quality"))
		if err != nil {
			return nil, err
		}
		return []*beads.Store{store}, nil
	}
	if len(configuredDirs) > maxUninstallLifecycleStores {
		return nil, fmt.Errorf("ordinary Hive lifecycle beads directories are bounded to %d", maxUninstallLifecycleStores)
	}
	seen := make(map[string]bool, len(configuredDirs))
	stores := make([]*beads.Store, 0, len(configuredDirs))
	for _, configuredDir := range configuredDirs {
		configuredDir = strings.TrimSpace(configuredDir)
		if configuredDir == "" || !filepath.IsAbs(configuredDir) {
			return nil, fmt.Errorf("ordinary Hive lifecycle beads directory must be absolute")
		}
		configuredDir = filepath.Clean(configuredDir)
		if seen[configuredDir] {
			continue
		}
		seen[configuredDir] = true
		store, err := beads.NewSharedStore(configuredDir)
		if err != nil {
			return nil, err
		}
		stores = append(stores, store)
	}
	if len(stores) == 0 {
		return nil, fmt.Errorf("ordinary Hive lifecycle beads directories are required")
	}
	return stores, nil
}

func selectUninstallLifecycleBeadStore(stores []*beads.Store, beadID string) (*beads.Store, error) {
	var match *beads.Store
	for _, store := range stores {
		if store == nil {
			continue
		}
		if _, err := store.Get(beadID); err != nil {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("bead %s exists in multiple ordinary Hive role stores", beadID)
		}
		match = store
	}
	if match == nil {
		return nil, fmt.Errorf("bead %s not found in ordinary Hive role stores", beadID)
	}
	return match, nil
}

func drainFindingRepairResources(ctx context.Context, store *Store, lifecycle *visualhive.LifecycleStore, config Config, client *hivegithub.Client, finding visualhive.FindingLifecycle, attempt *repair.Attempt) error {
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
		if finding.Branch == "" {
			return fmt.Errorf("finding %s has an incomplete repair branch binding", finding.RepositoryFingerprint)
		}
		if finding.RepairCommitSHA == "" {
			if attempt == nil || attempt.RepositoryFingerprint != finding.RepositoryFingerprint || attempt.Stage != repair.StageNoChange ||
				attempt.Branch != finding.Branch || attempt.CommitSHA != "" || attempt.PRNumber != 0 {
				return fmt.Errorf("finding %s has an incomplete repair branch binding", finding.RepositoryFingerprint)
			}
			absent, err := client.RepairBranchAbsentExact(ctx, config.Repository, finding.Branch)
			if err != nil {
				return err
			}
			if !absent {
				return fmt.Errorf("finding %s has an incomplete repair branch binding", finding.RepositoryFingerprint)
			}
			detail := fmt.Sprintf("finding=%s branch=%s stage=%s remote_absent=true", finding.RepositoryFingerprint, finding.Branch, attempt.Stage)
			if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_absent_repair_branch", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return err
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "uninstall_absent_repair_branch", true, detail)
		} else if err := deleteUninstallRepairBranch(ctx, store, lifecycle, config, client, finding.RepositoryFingerprint, finding.Branch, finding.RepairCommitSHA); err != nil {
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
	if attempt.Branch != "" || attempt.CommitSHA != "" {
		if attempt.Branch == "" {
			return fmt.Errorf("repair attempt %s has an incomplete repair branch binding", attempt.RepositoryFingerprint)
		}
		if attempt.CommitSHA == "" {
			absentUnpublishedFailure := attempt.Stage == repair.StageFailed &&
				attempt.ResumeStage == repair.StagePrepared &&
				!attempt.AttemptCounted
			// Management holds the serialized lifecycle lock and automation is
			// already paused here. An uncounted model invocation with no published
			// ref therefore has no external repair resource left to retire.
			absentInterruptedModel := attempt.Stage == repair.StageModelRunning &&
				!attempt.AttemptCounted
			absentCancelled := attempt.Stage == repair.StageCancelled &&
				!attempt.AttemptCounted && repair.UninstallRefsRetired(attempt)
			if (attempt.Stage != repair.StageNoChange && !absentUnpublishedFailure && !absentInterruptedModel && !absentCancelled) || attempt.PRNumber != 0 {
				return fmt.Errorf("repair attempt %s has an incomplete repair branch binding", attempt.RepositoryFingerprint)
			}
			absent, err := client.RepairBranchAbsentExact(ctx, config.Repository, attempt.Branch)
			if err != nil {
				return err
			}
			if !absent {
				return fmt.Errorf("repair attempt %s has an incomplete repair branch binding", attempt.RepositoryFingerprint)
			}
			if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_absent_repair_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s branch=%s stage=%s remote_absent=true", attempt.RepositoryFingerprint, attempt.Branch, attempt.Stage)}); err != nil {
				return err
			}
			if absentInterruptedModel || absentCancelled {
				removed, err := repair.RetireBrokenUnpublishedBranchForUninstall(ctx, config.CheckoutDir, attempt, true)
				if err != nil {
					return err
				}
				if removed {
					if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_remove_broken_local_repair_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s branch=%s stage=%s remote_absent=true", attempt.RepositoryFingerprint, attempt.Branch, attempt.Stage)}); err != nil {
						return err
					}
				}
			}
		} else {
			if err := store.AuditStrict(AuditEntry{Action: "authorize_uninstall_delete_repair_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("finding=%s branch=%s head=%s", attempt.RepositoryFingerprint, attempt.Branch, attempt.CommitSHA)}); err != nil {
				return err
			}
			if err := client.DeleteRepairBranchExact(ctx, config.Repository, attempt.Branch, attempt.CommitSHA); err != nil {
				return err
			}
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

// synchronizeUninstallVerificationHead makes the exact current default-branch
// commit available to the Hive-owned checkout without moving or cleaning it.
// GitHub normally creates a distinct merge commit for the uninstall PR, so the
// locally prepared cleanup head alone is insufficient for baseline-inventory
// verification.
func synchronizeUninstallVerificationHead(ctx context.Context, config Config, expectedHead string) error {
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if !immutableCommit.MatchString(expectedHead) {
		return fmt.Errorf("uninstall verification requires an immutable default-branch head")
	}
	branch := strings.TrimSpace(config.DefaultBranch)
	if !validLegacyBranchName(branch) {
		return fmt.Errorf("uninstall verification requires a safe installed default branch")
	}
	exists, err := validateManagedCheckoutBeforeGit(config.CheckoutDir, config.Repository)
	if err != nil {
		return fmt.Errorf("validate managed checkout before uninstall verification: %w", err)
	}
	if !exists {
		return fmt.Errorf("uninstall verification managed checkout is unavailable")
	}
	remoteURL := RepositoryCloneURL(config.Repository)
	remoteRef := "refs/remotes/origin/" + branch
	refspec := "+refs/heads/" + branch + ":" + remoteRef
	if _, err := gitTransport(ctx, config.CheckoutDir, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return fmt.Errorf("fetch uninstall verification head: %w", err)
	}
	fetchedHead, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve fetched uninstall verification head: %w", err)
	}
	fetchedHead = strings.ToLower(strings.TrimSpace(fetchedHead))
	if fetchedHead != expectedHead {
		return fmt.Errorf("fetched default-branch head %s does not match verified uninstall head %s", fetchedHead, expectedHead)
	}
	if _, err := git(ctx, config.CheckoutDir, "cat-file", "-e", expectedHead+"^{commit}"); err != nil {
		return fmt.Errorf("verify fetched uninstall commit %s: %w", expectedHead, err)
	}
	return nil
}

// verifyUninstalledSetupOrPendingProposalAtCommit preserves the normal
// byte-for-byte preimage proof while allowing one narrower recovery case: the
// exact Hive-owned setup PR never merged and the target commit still has no
// installed Hive markers. This avoids restoring stale preimages captured by an
// interrupted pre-publication setup transaction.
func verifyUninstalledSetupOrPendingProposalAtCommit(ctx context.Context, client *hivegithub.Client, config Config, commitSHA string) error {
	if err := VerifyUninstalledSetupAtCommit(ctx, client, config, commitSHA); err == nil {
		return nil
	} else {
		recoverable, recoveryErr := unmergedSetupProposalLeavesTargetUninstalled(ctx, client, config, commitSHA)
		if recoveryErr != nil {
			return fmt.Errorf("verify managed-path restoration: %w; verify exact unmerged setup recovery: %v", err, recoveryErr)
		}
		if !recoverable {
			return err
		}
	}
	return nil
}

// unmergedSetupProposalLeavesTargetUninstalled proves that Hive's exact setup
// proposal is still unmerged and that its target commit never acquired the
// authoritative installation markers. It never infers repository cleanliness
// from local state or from the potentially stale preimage ledger.
func unmergedSetupProposalLeavesTargetUninstalled(ctx context.Context, client *hivegithub.Client, config Config, commitSHA string) (bool, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if client == nil || client.GoGitHub() == nil || !immutableCommit.MatchString(commitSHA) {
		return false, fmt.Errorf("GitHub client and exact target commit are required")
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return false, fmt.Errorf("repository identity is invalid")
	}
	markers := []string{".hive/integrated.json"}
	if config.VisualHive {
		markers = append(markers, visualHiveProductionWorkflowPath)
	}
	for _, relative := range markers {
		content, directory, response, err := client.GoGitHub().Repositories.GetContents(
			ctx, owner, repo, relative, &gh.RepositoryContentGetOptions{Ref: commitSHA},
		)
		if err == nil || content != nil || len(directory) != 0 {
			return false, nil
		}
		if response == nil || response.StatusCode != http.StatusNotFound {
			return false, fmt.Errorf("verify absence of installation marker %s: %w", relative, err)
		}
	}
	expectedBranch := managedOperationBranch("setup", config.RepositoryID)
	if config.SetupPRNumber <= 0 || strings.TrimSpace(config.SetupPRURL) == "" ||
		config.SetupBranch != expectedBranch || !immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(config.SetupHeadSHA))) {
		return false, fmt.Errorf("target lacks installation markers but setup proposal identity is incomplete")
	}
	marker := "<!-- hive-setup: " + strings.ToLower(strings.TrimSpace(config.Repository)) + " -->"
	snapshot, err := client.InspectManagedPullRequestExact(
		ctx, config.Repository, config.RepositoryID, config.SetupPRNumber,
		marker, config.SetupBranch, config.SetupHeadSHA, config.DefaultBranch,
	)
	if err != nil {
		return false, err
	}
	if snapshot.URL != config.SetupPRURL || snapshot.Merged ||
		(snapshot.State != "open" && snapshot.State != "closed") {
		return false, fmt.Errorf("setup proposal is not the exact unmerged Hive-owned pull request")
	}
	if !strings.EqualFold(snapshot.BaseSHA, commitSHA) {
		return false, fmt.Errorf("setup proposal base %s does not match target commit %s", snapshot.BaseSHA, commitSHA)
	}
	if strings.EqualFold(snapshot.HeadSHA, snapshot.BaseSHA) {
		return false, fmt.Errorf("setup proposal has no distinct managed head")
	}
	return true, nil
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
	if initialDigest := strings.ToLower(strings.TrimSpace(config.SetupBaselineInitialDigest)); initialDigest != "" {
		expectedDigest, digestErr := setupBaselineCandidateDigest(config.SetupBaselineInitialCandidates)
		if digestErr != nil || !strings.EqualFold(expectedDigest, initialDigest) {
			return fmt.Errorf("uninstall verification refuses an invalid initial baseline inventory")
		}
		liveCandidates, liveDigest, inventoryErr := setupBaselineInventoryAtCommit(ctx, config.CheckoutDir, commitSHA)
		if inventoryErr != nil {
			return fmt.Errorf("verify restored baseline inventory at target commit %s: %w", commitSHA, inventoryErr)
		}
		if !strings.EqualFold(liveDigest, initialDigest) || !equalSetupBaselineCandidates(liveCandidates, config.SetupBaselineInitialCandidates) {
			return fmt.Errorf("baseline inventory was not restored byte-for-byte at target commit %s", commitSHA)
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

// RestoreInterruptedUninstallFinalization recovers the exact durable config
// marker left when a final state-deletion attempt stopped after the config was
// sealed but before the managed state root could be removed. Dashboard
// callers use this only for an owner-requested uninstall-finalize operation so
// a new exact plan can be produced without selecting repository state from
// client input.
func RestoreInterruptedUninstallFinalization(stateDir, repository string) error {
	stateDir = strings.TrimSpace(stateDir)
	repository = strings.TrimSpace(repository)
	if stateDir == "" || repository == "" {
		return fmt.Errorf("state directory and repository are required to recover uninstall finalization")
	}
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	rootInfo, err := os.Lstat(absolute)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(absolute) {
		return fmt.Errorf("interrupted uninstall state root is unavailable or non-ordinary")
	}
	integratedDir := filepath.Join(absolute, "integrated")
	integratedInfo, err := os.Lstat(integratedDir)
	if err != nil || !integratedInfo.IsDir() || integratedInfo.Mode()&os.ModeSymlink != 0 || deletionPathIsReparsePoint(integratedDir) {
		return fmt.Errorf("interrupted uninstall integrated state is unavailable or non-ordinary")
	}
	store := &Store{
		dir:        integratedDir,
		configPath: filepath.Join(integratedDir, "config.json"),
		auditPath:  filepath.Join(integratedDir, "audit.jsonl"),
	}
	if _, statErr := os.Lstat(store.configPath); statErr == nil {
		config, loadErr := store.Load()
		if loadErr != nil {
			return loadErr
		}
		if !strings.EqualFold(config.Repository, repository) || !sameFilesystemPath(config.StateDir, absolute) {
			return fmt.Errorf("installed integrated lifecycle does not match repository %s and its exact state directory", repository)
		}
		return nil
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	marker := filepath.Join(store.dir, uninstallFinalizingConfigFile)
	data, err := readOrdinaryStateFile(marker)
	if err != nil {
		return fmt.Errorf("read interrupted uninstall finalization marker: %w", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil || !supportedDurableConfigSchema(config.SchemaVersion) {
		return fmt.Errorf("interrupted uninstall finalization marker has an invalid managed config identity")
	}
	if !strings.EqualFold(config.Repository, repository) || !sameFilesystemPath(config.StateDir, absolute) {
		return fmt.Errorf("interrupted uninstall finalization does not match repository %s and its exact state directory", repository)
	}
	if !config.Paused {
		return fmt.Errorf("interrupted uninstall finalization is not durably paused")
	}
	ownerData, err := readOrdinaryStateFile(filepath.Join(absolute, stateOwnershipMarkerFile))
	if err != nil {
		return fmt.Errorf("read interrupted uninstall state ownership: %w", err)
	}
	var owner stateOwnershipMarker
	if json.Unmarshal(ownerData, &owner) != nil ||
		owner.SchemaVersion != "hive.state-owner.v1" ||
		!strings.EqualFold(owner.Repository, config.Repository) ||
		owner.RepositoryID != config.RepositoryID {
		return fmt.Errorf("interrupted uninstall state ownership does not match the managed config")
	}
	intent, exists, err := store.LoadUninstallIntent()
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("interrupted uninstall finalization has no durable uninstall intent")
	}
	if err := validateUninstallIntentBinding(intent, config); err != nil {
		return fmt.Errorf("interrupted uninstall finalization intent is invalid: %w", err)
	}
	if err := restoreInterruptedStateDeletion(store); err != nil {
		return err
	}
	return store.AuditStrict(AuditEntry{
		Action:     "uninstall_finalization_recovered",
		Allowed:    true,
		Repository: config.Repository,
		Detail:     fmt.Sprintf("phase=%s request=owner_dashboard_finalize", intent.Phase),
	})
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

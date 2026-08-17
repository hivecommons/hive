package integrated

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	OperationAuthorizerTransfer    ManagementOperation = "authorizer-transfer"
	authorizerTransferIntentSchema                     = "hive.setup-authorizer-transfer.v1"
	authorizerTransferIntentFile                       = "setup-authorizer-transfer.json"
	authorizerTransferPrepared                         = "prepared"
	authorizerTransferPullRecorded                     = "pull_recorded"
	authorizerTransferAuthorized                       = "authorized"
)

// AuthorizerTransferIntent is the durable boundary for rotating the human
// principal allowed to authorize managed setup PRs. The old actor remains in
// Config until the exact old-actor-authorized PR is merged and its complete
// managed tree is verified at the current default-branch head.
type AuthorizerTransferIntent struct {
	SchemaVersion        string        `json:"schema_version"`
	Phase                string        `json:"phase"`
	Repository           string        `json:"repository"`
	RepositoryID         string        `json:"repository_id"`
	DefaultBranch        string        `json:"default_branch"`
	VisualHive           bool          `json:"visual_hive"`
	ExecutionMode        ExecutionMode `json:"execution_mode,omitempty"`
	SourceConfigDigest   string        `json:"source_config_digest"`
	OldAuthorizerID      int64         `json:"old_authorizer_id"`
	OldAuthorizerLogin   string        `json:"old_authorizer_login"`
	NewAuthorizerID      int64         `json:"new_authorizer_id"`
	NewAuthorizerLogin   string        `json:"new_authorizer_login"`
	Reason               string        `json:"reason"`
	Branch               string        `json:"branch"`
	BaseSHA              string        `json:"base_sha"`
	HeadSHA              string        `json:"head_sha"`
	Marker               string        `json:"marker"`
	ChangedFiles         []string      `json:"changed_files"`
	PRNumber             int           `json:"pr_number,omitempty"`
	PRURL                string        `json:"pr_url,omitempty"`
	PullDiffDigest       string        `json:"pull_diff_digest,omitempty"`
	SetupDiffDigest      string        `json:"setup_diff_digest,omitempty"`
	AuthorizationContext string        `json:"authorization_context,omitempty"`
	AuthorizationStatus  int64         `json:"authorization_status_id,omitempty"`
	AuthorizationCreator int64         `json:"authorization_creator_id,omitempty"`
	PreparedAt           time.Time     `json:"prepared_at"`
	PullRecordedAt       time.Time     `json:"pull_recorded_at,omitempty"`
	AuthorizedAt         time.Time     `json:"authorized_at,omitempty"`
}

type AuthorizerTransferOptions struct {
	StateDir          string
	NewAuthorizer     string
	Reason            string
	Cancel            bool
	GitHub            *hivegithub.Client
	GitTransportToken string
}

type AuthorizerTransferResult struct {
	SchemaVersion               string `json:"schema_version"`
	Repository                  string `json:"repository"`
	Phase                       string `json:"phase"`
	OldAuthorizerID             int64  `json:"old_authorizer_id"`
	OldAuthorizerLogin          string `json:"old_authorizer_login"`
	NewAuthorizerID             int64  `json:"new_authorizer_id"`
	NewAuthorizerLogin          string `json:"new_authorizer_login"`
	Branch                      string `json:"branch"`
	CommitSHA                   string `json:"commit_sha"`
	PRNumber                    int    `json:"pr_number,omitempty"`
	PRURL                       string `json:"pr_url,omitempty"`
	SetupAuthorizationContext   string `json:"setup_authorization_context,omitempty"`
	SetupAuthorizationStatusID  int64  `json:"setup_authorization_status_id,omitempty"`
	SetupAuthorizationCreatorID int64  `json:"setup_authorization_creator_id,omitempty"`
	FinalizationPending         bool   `json:"finalization_pending"`
	Completed                   bool   `json:"completed"`
	Cancelled                   bool   `json:"cancelled"`
	CurrentDefaultHead          string `json:"current_default_head,omitempty"`
	NextCommand                 string `json:"next_command,omitempty"`
	CancelCommand               string `json:"cancel_command,omitempty"`
}

func (s *Store) SaveAuthorizerTransferIntent(intent AuthorizerTransferIntent) error {
	intent.SchemaVersion = authorizerTransferIntentSchema
	intent.ChangedFiles = sortedUniquePaths(intent.ChangedFiles)
	if err := validateAuthorizerTransferIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(s.dir, authorizerTransferIntentFile), append(data, '\n'))
}

func (s *Store) LoadAuthorizerTransferIntent() (AuthorizerTransferIntent, bool, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, authorizerTransferIntentFile))
	if os.IsNotExist(err) {
		return AuthorizerTransferIntent{}, false, nil
	}
	if err != nil {
		return AuthorizerTransferIntent{}, false, err
	}
	var intent AuthorizerTransferIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return AuthorizerTransferIntent{}, false, fmt.Errorf("decode setup authorizer transfer intent: %w", err)
	}
	if err := validateAuthorizerTransferIntent(intent); err != nil {
		return AuthorizerTransferIntent{}, false, err
	}
	return intent, true, nil
}

func (s *Store) DeleteAuthorizerTransferIntent() error {
	return durableRemoveStateFile(filepath.Join(s.dir, authorizerTransferIntentFile))
}

func validateAuthorizerTransferIntent(intent AuthorizerTransferIntent) error {
	if intent.SchemaVersion != authorizerTransferIntentSchema ||
		(intent.Phase != authorizerTransferPrepared && intent.Phase != authorizerTransferPullRecorded && intent.Phase != authorizerTransferAuthorized) ||
		strings.TrimSpace(intent.Repository) == "" || strings.TrimSpace(intent.RepositoryID) == "" || strings.TrimSpace(intent.DefaultBranch) == "" ||
		len(intent.SourceConfigDigest) != sha256.Size*2 || intent.OldAuthorizerID <= 0 || intent.NewAuthorizerID <= 0 || intent.OldAuthorizerID == intent.NewAuthorizerID ||
		strings.TrimSpace(intent.OldAuthorizerLogin) == "" || strings.TrimSpace(intent.NewAuthorizerLogin) == "" || strings.TrimSpace(intent.Reason) == "" || len(intent.Reason) > 2048 ||
		intent.Branch != managedOperationBranch(string(OperationAuthorizerTransfer), intent.RepositoryID) || !immutableCommit.MatchString(strings.ToLower(intent.BaseSHA)) ||
		!immutableCommit.MatchString(strings.ToLower(intent.HeadSHA)) || !validAuthorizerTransferMarker(intent.Marker, intent.Repository, intent.OldAuthorizerID, intent.NewAuthorizerID) ||
		len(intent.ChangedFiles) == 0 || !exactPathOrder(intent.ChangedFiles, sortedUniquePaths(intent.ChangedFiles)) || intent.PreparedAt.IsZero() {
		return fmt.Errorf("setup authorizer transfer intent is incomplete or invalid")
	}
	if _, err := hex.DecodeString(intent.SourceConfigDigest); err != nil {
		return fmt.Errorf("setup authorizer transfer source config digest is invalid")
	}
	allowed := map[string]bool{}
	for _, path := range managedSetupFilesForMode(intent.VisualHive, normalizedExecutionMode(intent.ExecutionMode)) {
		allowed[path] = true
	}
	for _, path := range intent.ChangedFiles {
		if !allowed[path] {
			return fmt.Errorf("setup authorizer transfer intent contains unmanaged path %s", path)
		}
	}
	if intent.Phase == authorizerTransferPrepared {
		if intent.PRNumber != 0 || intent.PRURL != "" || intent.PullDiffDigest != "" || intent.SetupDiffDigest != "" || intent.AuthorizationContext != "" || intent.AuthorizationStatus != 0 || intent.AuthorizationCreator != 0 || !intent.PullRecordedAt.IsZero() || !intent.AuthorizedAt.IsZero() {
			return fmt.Errorf("prepared setup authorizer transfer contains premature pull or status authority")
		}
		return nil
	}
	if intent.PRNumber <= 0 || strings.TrimSpace(intent.PRURL) == "" || len(intent.PullDiffDigest) != sha256.Size*2 || len(intent.SetupDiffDigest) != sha256.Size*2 ||
		!validSetupAuthorizationContext(intent.AuthorizationContext) || intent.PullRecordedAt.IsZero() {
		return fmt.Errorf("recorded setup authorizer transfer pull evidence is incomplete")
	}
	for _, digest := range []string{intent.PullDiffDigest, intent.SetupDiffDigest} {
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("setup authorizer transfer diff digest is invalid")
		}
	}
	if intent.Phase == authorizerTransferPullRecorded {
		if intent.AuthorizationStatus != 0 || intent.AuthorizationCreator != 0 || !intent.AuthorizedAt.IsZero() {
			return fmt.Errorf("pull-recorded setup authorizer transfer contains premature status authority")
		}
		return nil
	}
	if intent.AuthorizationStatus <= 0 || intent.AuthorizationCreator != intent.OldAuthorizerID || intent.AuthorizedAt.IsZero() {
		return fmt.Errorf("authorized setup authorizer transfer lacks exact old-actor status evidence")
	}
	return nil
}

func validSetupAuthorizationContext(value string) bool {
	if !strings.HasPrefix(value, SetupAuthorizationContextPrefix) || len(value) != len(SetupAuthorizationContextPrefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, SetupAuthorizationContextPrefix))
	return err == nil
}

func RunAuthorizerTransfer(ctx context.Context, options AuthorizerTransferOptions) (AuthorizerTransferResult, error) {
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	result := AuthorizerTransferResult{SchemaVersion: "hive.setup-authorizer-transfer.v1"}
	options.StateDir = strings.TrimSpace(options.StateDir)
	options.NewAuthorizer = strings.TrimSpace(options.NewAuthorizer)
	options.Reason = strings.TrimSpace(options.Reason)
	if options.GitHub == nil || options.StateDir == "" || (!options.Cancel && (options.NewAuthorizer == "" || options.Reason == "")) || len(options.Reason) > 2048 {
		return result, fmt.Errorf("GitHub client, persistent state directory, target authorizer login, and a bounded accountable reason are required")
	}
	release, err := waitForProductionRunLease(ctx, options.StateDir, 10*time.Minute)
	if err != nil {
		return result, fmt.Errorf("serialize setup authorizer transfer with production runs: %w", err)
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
	result.Repository = config.Repository
	fail := func(cause error) (AuthorizerTransferResult, error) {
		if auditErr := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer", Allowed: false, Repository: config.Repository, Detail: cause.Error()}); auditErr != nil {
			return result, fmt.Errorf("%v; persist denied setup authorizer transfer audit: %w", cause, auditErr)
		}
		return result, cause
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return fail(err)
	}
	if intent, exists, loadErr := store.LoadAuthorizerTransferIntent(); loadErr != nil {
		return fail(loadErr)
	} else if exists {
		if options.Cancel {
			return cancelAuthorizerTransfer(ctx, options, store, config, intent, result, fail)
		}
		if !strings.EqualFold(intent.NewAuthorizerLogin, options.NewAuthorizer) || intent.Reason != options.Reason {
			return fail(fmt.Errorf("an exact setup authorizer transfer to %s is already pending; repeat its exact target and reason", intent.NewAuthorizerLogin))
		}
		return resumeAuthorizerTransfer(ctx, options, store, config, intent, result, fail)
	}
	if options.Cancel {
		return fail(fmt.Errorf("no setup authorizer transfer is pending to cancel"))
	}
	if config.SetupAuthorizationActorID <= 0 {
		return fail(fmt.Errorf("the installed setup has no numeric authorizer to transfer"))
	}
	current, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil {
		return fail(err)
	}
	if current.ID != config.SetupAuthorizationActorID {
		return fail(fmt.Errorf("setup authorizer transfer is bound to current GitHub user ID %d, but the authenticated user is %d", config.SetupAuthorizationActorID, current.ID))
	}
	target, err := options.GitHub.ResolveHumanNumericUser(ctx, options.NewAuthorizer)
	if err != nil {
		return fail(err)
	}
	if target.ID == current.ID {
		return fail(fmt.Errorf("target %s is already the recorded setup authorizer", target.Login))
	}
	if _, exists, err := store.LoadUninstallIntent(); err != nil {
		return fail(err)
	} else if exists {
		return fail(fmt.Errorf("setup authorizer transfer cannot start while uninstall is pending"))
	}
	if err := verifyUninstallRecoveryState(store); err != nil {
		return fail(fmt.Errorf("setup authorizer transfer requires reconciled mutation state: %w", err))
	}
	if err := VerifyInstalledSetup(ctx, options.GitHub, config); err != nil {
		return fail(fmt.Errorf("setup authorizer transfer requires the current durable policy to be exactly installed: %w", err))
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return fail(err)
	}
	if defaultBranch != config.DefaultBranch {
		return fail(fmt.Errorf("default branch changed from %s to %s", config.DefaultBranch, defaultBranch))
	}
	baseSHA, err := git(ctx, config.CheckoutDir, "rev-parse", "origin/"+defaultBranch)
	if err != nil {
		return fail(fmt.Errorf("read exact authorizer transfer base: %w", err))
	}
	baseSHA = strings.ToLower(strings.TrimSpace(baseSHA))
	branch := managedOperationBranch(string(OperationAuthorizerTransfer), config.RepositoryID)
	if err := authorizeSetup(store, integratedPolicy(config), config.Repository, automation.ActionSetupBranch); err != nil {
		return fail(err)
	}
	if _, err := git(ctx, config.CheckoutDir, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return fail(err)
	}
	candidate := config
	candidate.SetupAuthorizationActorID = target.ID
	candidate.SetupAuthorizationPreviousActorID = current.ID
	candidate.SetupBranch = branch
	candidate.SetupPRNumber, candidate.SetupPRURL = 0, ""
	inspection, err := InspectCheckout(config.CheckoutDir, defaultBranch)
	if err != nil {
		return fail(err)
	}
	if err := writeManagedFiles(config.CheckoutDir, candidate, inspection); err != nil {
		return fail(err)
	}
	managed := managedSetupFilesForConfig(config)
	if err := authorizeSetup(store, integratedPolicy(config), config.Repository, automation.ActionSetupCommit); err != nil {
		return fail(err)
	}
	if err := stageManagedPaths(ctx, config.CheckoutDir, managed); err != nil {
		return fail(err)
	}
	changed, err := git(ctx, config.CheckoutDir, "diff", "--cached", "--name-only")
	if err != nil {
		return fail(err)
	}
	changedFiles := sortedUniquePaths(strings.Fields(changed))
	if len(changedFiles) == 0 {
		return fail(fmt.Errorf("setup authorizer transfer produced no managed policy change"))
	}
	if _, err := git(ctx, config.CheckoutDir, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", "chore: transfer Hive setup authorizer", "-m", managedCommitTrailers(config.RepositoryID, string(OperationAuthorizerTransfer))); err != nil {
		return fail(err)
	}
	headSHA, err := git(ctx, config.CheckoutDir, "rev-parse", "HEAD")
	if err != nil {
		return fail(err)
	}
	sourceDigest, err := authorizerTransferSourceConfigDigest(config)
	if err != nil {
		return fail(err)
	}
	marker, err := newAuthorizerTransferMarker(config.Repository, current.ID, target.ID)
	if err != nil {
		return fail(err)
	}
	intent := AuthorizerTransferIntent{
		SchemaVersion: authorizerTransferIntentSchema, Phase: authorizerTransferPrepared,
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: defaultBranch, VisualHive: config.VisualHive, ExecutionMode: config.ExecutionMode, SourceConfigDigest: sourceDigest,
		OldAuthorizerID: current.ID, OldAuthorizerLogin: current.Login, NewAuthorizerID: target.ID, NewAuthorizerLogin: target.Login,
		Reason: options.Reason, Branch: branch, BaseSHA: baseSHA, HeadSHA: strings.ToLower(strings.TrimSpace(headSHA)),
		Marker: marker, ChangedFiles: changedFiles, PreparedAt: time.Now().UTC(),
	}
	if err := store.SaveAuthorizerTransferIntent(intent); err != nil {
		return fail(err)
	}
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_prepared", Allowed: true, Repository: config.Repository, Detail: authorizerTransferAuditDetail(intent, current.Login, "")}); err != nil {
		return result, err
	}
	return resumeAuthorizerTransfer(ctx, options, store, config, intent, result, fail)
}

func resumeAuthorizerTransfer(ctx context.Context, options AuthorizerTransferOptions, store *Store, config Config, intent AuthorizerTransferIntent, result AuthorizerTransferResult, fail func(error) (AuthorizerTransferResult, error)) (AuthorizerTransferResult, error) {
	result.Repository, result.Phase = intent.Repository, intent.Phase
	result.OldAuthorizerID, result.OldAuthorizerLogin = intent.OldAuthorizerID, intent.OldAuthorizerLogin
	result.NewAuthorizerID, result.NewAuthorizerLogin = intent.NewAuthorizerID, intent.NewAuthorizerLogin
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = intent.Branch, intent.HeadSHA, intent.PRNumber, intent.PRURL
	result.SetupAuthorizationContext, result.SetupAuthorizationStatusID, result.SetupAuthorizationCreatorID = intent.AuthorizationContext, intent.AuthorizationStatus, intent.AuthorizationCreator
	result.FinalizationPending, result.NextCommand = true, AuthorizerTransferNextCommand(options.StateDir, intent)
	result.CancelCommand = AuthorizerTransferCancelCommand(options.StateDir)
	if err := validateAuthorizerTransferBinding(intent, config); err != nil {
		return fail(err)
	}
	current, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil {
		return fail(err)
	}
	defaultBranch, err := ensureCheckout(ctx, config.Repository, config.CheckoutDir)
	if err != nil {
		return fail(err)
	}
	if defaultBranch != intent.DefaultBranch {
		return fail(fmt.Errorf("default branch changed from %s to %s", intent.DefaultBranch, defaultBranch))
	}
	if _, err := git(ctx, config.CheckoutDir, "cat-file", "-e", intent.HeadSHA+"^{commit}"); err != nil {
		return fail(fmt.Errorf("prepared authorizer transfer commit %s is unavailable locally", intent.HeadSHA))
	}
	snapshot, found, err := options.GitHub.FindManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, intent.Marker, intent.Branch, intent.HeadSHA, intent.DefaultBranch)
	if err != nil {
		return fail(fmt.Errorf("the pending setup-authorizer transfer PR no longer has its exact immutable base/head binding; cancel and restart it with %s: %w", AuthorizerTransferCancelCommand(options.StateDir), err))
	}
	if !found {
		if current.ID != intent.OldAuthorizerID || config.SetupAuthorizationActorID != intent.OldAuthorizerID {
			return fail(fmt.Errorf("only the still-recorded old authorizer ID %d may publish the transfer pull request", intent.OldAuthorizerID))
		}
		policy := integratedPolicy(config)
		if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPush); err != nil {
			return fail(err)
		}
		if err := pushManagedBranch(ctx, config.CheckoutDir, config.Repository, intent.Branch, config.RepositoryID, string(OperationAuthorizerTransfer), intent.HeadSHA); err != nil {
			return fail(err)
		}
		if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupPR); err != nil {
			return fail(err)
		}
		pull, err := options.GitHub.UpsertRepairPullRequest(ctx, config.Repository, intent.Branch, intent.HeadSHA, intent.DefaultBranch,
			fmt.Sprintf("Transfer Hive setup authorizer to %s", intent.NewAuthorizerLogin), authorizerTransferBody(intent), intent.Marker)
		if err != nil {
			return fail(err)
		}
		if err := verifyManagedPullHead(string(OperationAuthorizerTransfer), pull, intent.HeadSHA); err != nil {
			return fail(err)
		}
		snapshot, err = options.GitHub.InspectManagedPullRequestExact(ctx, config.Repository, config.RepositoryID, pull.Number, intent.Marker, intent.Branch, intent.HeadSHA, intent.DefaultBranch)
		if err != nil {
			return fail(err)
		}
	}
	if err := validateAuthorizerTransferSnapshot(intent, snapshot); err != nil {
		return fail(err)
	}
	if intent.Phase == authorizerTransferPrepared {
		updated, err := bindAuthorizerTransferPull(ctx, config, intent, snapshot)
		if err != nil {
			return fail(err)
		}
		if err := store.SaveAuthorizerTransferIntent(updated); err != nil {
			return fail(err)
		}
		intent = updated
		result.Phase, result.PRNumber, result.PRURL, result.SetupAuthorizationContext = intent.Phase, intent.PRNumber, intent.PRURL, intent.AuthorizationContext
	}
	if !snapshot.Merged && (snapshot.State != "open" || current.ID != intent.OldAuthorizerID || config.SetupAuthorizationActorID != intent.OldAuthorizerID) {
		return fail(fmt.Errorf("only the still-recorded old authorizer ID %d may authorize the open transfer pull request", intent.OldAuthorizerID))
	}
	if snapshot.Merged && current.ID != intent.OldAuthorizerID && current.ID != intent.NewAuthorizerID {
		return fail(fmt.Errorf("merged transfer finalization requires the exact old or explicitly designated new authorizer identity"))
	}
	statusRequest := hivegithub.SetupAuthorizationStatusRequest{
		Repository: intent.Repository, HeadSHA: intent.HeadSHA, Context: intent.AuthorizationContext,
		TargetURL: intent.PRURL, ExpectedCreatorID: intent.OldAuthorizerID,
	}
	status, statusErr := options.GitHub.VerifySetupAuthorizationStatus(ctx, statusRequest)
	if statusErr != nil {
		if snapshot.Merged {
			return fail(fmt.Errorf("merged authorizer transfer lacks recoverable exact old-actor authorization: %w", statusErr))
		}
		authorizationConfig := config
		authorizationConfig.SetupBranch = intent.Branch
		authorizationConfig.SetupAuthorizationActorID = intent.OldAuthorizerID
		authorization, err := authorizeManagedSetupPullRequest(ctx, store, options.GitHub, integratedPolicy(config), authorizationConfig, string(OperationAuthorizerTransfer), config.CheckoutDir, intent.Branch, intent.HeadSHA, intent.PRURL, intent.PRNumber)
		if err != nil {
			return fail(err)
		}
		if authorization.Status.Context != intent.AuthorizationContext || !strings.EqualFold(authorization.Diff.SHA256, intent.SetupDiffDigest) {
			return fail(fmt.Errorf("created transfer authorization no longer matches the durable context or setup diff"))
		}
		status = authorization.Status
	}
	if status.CreatorID != intent.OldAuthorizerID || status.StatusID <= 0 || status.Context != intent.AuthorizationContext {
		return fail(fmt.Errorf("setup authorizer transfer status is not the exact old-actor authorization"))
	}
	if intent.Phase != authorizerTransferAuthorized || intent.AuthorizationStatus != status.StatusID || intent.AuthorizationCreator != status.CreatorID {
		intent.Phase, intent.AuthorizationStatus, intent.AuthorizationCreator, intent.AuthorizedAt = authorizerTransferAuthorized, status.StatusID, status.CreatorID, time.Now().UTC()
		if err := store.SaveAuthorizerTransferIntent(intent); err != nil {
			return fail(err)
		}
		if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_authorized", Allowed: true, Repository: config.Repository, Detail: authorizerTransferAuditDetail(intent, current.Login, intent.AuthorizationContext)}); err != nil {
			return result, err
		}
	}
	result.Phase, result.SetupAuthorizationContext = intent.Phase, intent.AuthorizationContext
	result.SetupAuthorizationStatusID, result.SetupAuthorizationCreatorID = intent.AuthorizationStatus, intent.AuthorizationCreator
	result.PRNumber, result.PRURL = intent.PRNumber, intent.PRURL
	if !snapshot.Merged {
		return result, nil
	}
	if snapshot.State != "closed" || strings.TrimSpace(snapshot.MergeSHA) == "" {
		return fail(fmt.Errorf("authorizer transfer pull request reports an invalid merged state"))
	}
	currentHead, err := currentDefaultBranchHead(ctx, options.GitHub, config)
	if err != nil {
		return fail(err)
	}
	candidate := authorizerTransferCandidate(config, intent)
	if err := VerifyInstalledSetupAtCommit(ctx, options.GitHub, candidate, currentHead); err != nil {
		return fail(fmt.Errorf("merged authorizer transfer is not exactly installed at current default head %s: %w", currentHead, err))
	}
	detail := authorizerTransferAuditDetail(intent, current.Login, "merge="+snapshot.MergeSHA+" default_head="+currentHead)
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_finalizing", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if err := store.Save(candidate); err != nil {
		return result, err
	}
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_completed", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if err := store.DeleteAuthorizerTransferIntent(); err != nil {
		return result, err
	}
	result.Completed, result.FinalizationPending, result.NextCommand, result.CancelCommand = true, false, "", ""
	result.CurrentDefaultHead = currentHead
	result.Phase = "completed"
	return result, nil
}

func cancelAuthorizerTransfer(ctx context.Context, options AuthorizerTransferOptions, store *Store, config Config, intent AuthorizerTransferIntent, result AuthorizerTransferResult, fail func(error) (AuthorizerTransferResult, error)) (AuthorizerTransferResult, error) {
	result.Repository, result.Phase = intent.Repository, intent.Phase
	result.OldAuthorizerID, result.OldAuthorizerLogin = intent.OldAuthorizerID, intent.OldAuthorizerLogin
	result.NewAuthorizerID, result.NewAuthorizerLogin = intent.NewAuthorizerID, intent.NewAuthorizerLogin
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = intent.Branch, intent.HeadSHA, intent.PRNumber, intent.PRURL
	if err := validateAuthorizerTransferBinding(intent, config); err != nil {
		return fail(err)
	}
	if config.SetupAuthorizationActorID != intent.OldAuthorizerID {
		return fail(fmt.Errorf("setup authorizer transfer has already advanced local authority; cancellation is forbidden and exact finalization is required"))
	}
	current, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil {
		return fail(err)
	}
	if current.ID != intent.OldAuthorizerID {
		return fail(fmt.Errorf("only the recorded old authorizer ID %d may cancel this transfer", intent.OldAuthorizerID))
	}
	detail := authorizerTransferAuditDetail(intent, current.Login, "cancel=true")
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_cancel_authorized", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	snapshot, found, err := options.GitHub.CloseManagedPullRequestForCancellation(ctx, config.Repository, config.RepositoryID, intent.PRNumber, intent.PRURL, intent.Marker, intent.Branch, intent.DefaultBranch)
	if err != nil {
		return fail(err)
	}
	if found {
		detail += fmt.Sprintf(" observed_head=%s observed_base=%s", snapshot.HeadSHA, snapshot.BaseSHA)
	}
	if found {
		if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_cancel_pr_closed", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return result, err
		}
	}
	deleted, retiredHead, err := options.GitHub.DeleteAuthorizerTransferBranchDescendantExact(ctx, config.Repository, intent.Branch, intent.HeadSHA)
	if err != nil {
		return fail(fmt.Errorf("retire exact setup authorizer transfer branch before cancellation: %w", err))
	}
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_cancel_branch", Allowed: true, Repository: config.Repository, Detail: fmt.Sprintf("%s retired_head=%s deleted=%t", detail, retiredHead, deleted)}); err != nil {
		return result, err
	}
	if err := store.AuditStrict(AuditEntry{Action: "setup_authorizer_transfer_cancelled", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, err
	}
	if err := store.DeleteAuthorizerTransferIntent(); err != nil {
		return result, err
	}
	result.Cancelled, result.FinalizationPending, result.Phase = true, false, "cancelled"
	result.NextCommand, result.CancelCommand = "", ""
	return result, nil
}

func bindAuthorizerTransferPull(ctx context.Context, config Config, intent AuthorizerTransferIntent, snapshot hivegithub.ManagedPullRequestSnapshot) (AuthorizerTransferIntent, error) {
	if err := validateAuthorizerTransferSnapshot(intent, snapshot); err != nil {
		return intent, err
	}
	binding, diff, err := BuildSetupAuthorizationBindingWithDiff(ctx, SetupAuthorizationRequest{
		CheckoutDir: config.CheckoutDir, Repository: config.Repository, RepositoryID: config.RepositoryID, PullRequest: snapshot.Number,
		HeadRef: intent.Branch, HeadSHA: intent.HeadSHA, BaseRef: intent.DefaultBranch, BaseSHA: intent.BaseSHA, AuthorizerID: intent.OldAuthorizerID,
		RequiredPresent: authorizerTransferRequiredFiles(config), RequiredAbsent: authorizerTransferAbsentFiles(config),
	})
	if err != nil {
		return intent, err
	}
	allowed := map[string]bool{}
	for _, path := range managedSetupFilesForConfig(config) {
		allowed[path] = true
	}
	for _, path := range diff.ChangedPaths {
		if !allowed[path] && !setupBaselineAuthorizationPath.MatchString(path) {
			return intent, fmt.Errorf("setup authorizer transfer authorization refuses unmanaged path %s", path)
		}
	}
	context, err := binding.StatusContext()
	if err != nil {
		return intent, err
	}
	intent.Phase, intent.PRNumber, intent.PRURL = authorizerTransferPullRecorded, snapshot.Number, snapshot.URL
	intent.PullDiffDigest, intent.SetupDiffDigest, intent.AuthorizationContext = strings.ToLower(snapshot.DiffDigest), strings.ToLower(diff.SHA256), context
	intent.PullRecordedAt = time.Now().UTC()
	return intent, validateAuthorizerTransferIntent(intent)
}

func authorizerTransferRequiredFiles(config Config) []string {
	required := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	if config.VisualHive {
		required = append(required, ".github/workflows/visual-hive-pr.yml", "docs/visual-hive.md", "visual-hive.config.yaml")
	}
	return required
}

func authorizerTransferAbsentFiles(config Config) []string {
	if !config.VisualHive {
		return nil
	}
	return standaloneVisualHiveWriterWorkflowPaths()
}

func validateAuthorizerTransferSnapshot(intent AuthorizerTransferIntent, snapshot hivegithub.ManagedPullRequestSnapshot) error {
	if snapshot.Number <= 0 || snapshot.URL == "" || snapshot.HeadBranch != intent.Branch || !strings.EqualFold(snapshot.HeadSHA, intent.HeadSHA) ||
		snapshot.BaseBranch != intent.DefaultBranch || !strings.EqualFold(snapshot.BaseSHA, intent.BaseSHA) || !equalPaths(snapshot.ChangedFiles, intent.ChangedFiles) || len(snapshot.DiffDigest) != sha256.Size*2 {
		return fmt.Errorf("setup authorizer transfer pull request no longer matches its exact durable base, head, branch, paths, or diff")
	}
	if intent.PRNumber > 0 && (snapshot.Number != intent.PRNumber || snapshot.URL != intent.PRURL || !strings.EqualFold(snapshot.DiffDigest, intent.PullDiffDigest)) {
		return fmt.Errorf("setup authorizer transfer pull request changed after durable binding")
	}
	return nil
}

func validateAuthorizerTransferBinding(intent AuthorizerTransferIntent, config Config) error {
	if !strings.EqualFold(intent.Repository, config.Repository) || intent.RepositoryID != config.RepositoryID || intent.DefaultBranch != config.DefaultBranch ||
		intent.VisualHive != config.VisualHive ||
		intent.Branch != managedOperationBranch(string(OperationAuthorizerTransfer), config.RepositoryID) ||
		!validAuthorizerTransferMarker(intent.Marker, config.Repository, intent.OldAuthorizerID, intent.NewAuthorizerID) {
		return fmt.Errorf("durable setup authorizer transfer no longer matches repository identity")
	}
	if config.SetupAuthorizationActorID == intent.NewAuthorizerID {
		if config.SetupBranch != intent.Branch || config.SetupPRNumber != intent.PRNumber || config.SetupPRURL != intent.PRURL {
			return fmt.Errorf("partially finalized setup authorizer transfer does not match durable pull identity")
		}
		return nil
	}
	if config.SetupAuthorizationActorID != intent.OldAuthorizerID {
		return fmt.Errorf("durable setup authorizer transfer old actor no longer matches local authority")
	}
	digest, err := authorizerTransferSourceConfigDigest(config)
	if err != nil {
		return err
	}
	if !strings.EqualFold(digest, intent.SourceConfigDigest) {
		return fmt.Errorf("durable setup policy changed while authorizer transfer was pending")
	}
	return nil
}

func authorizerTransferSourceConfigDigest(config Config) (string, error) {
	config.UpdatedAt = time.Time{}
	config.Paused = false
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func authorizerTransferCandidate(config Config, intent AuthorizerTransferIntent) Config {
	candidate := config
	candidate.SetupAuthorizationActorID = intent.NewAuthorizerID
	candidate.SetupAuthorizationPreviousActorID = intent.OldAuthorizerID
	candidate.SetupBranch, candidate.SetupPRNumber, candidate.SetupPRURL = intent.Branch, intent.PRNumber, intent.PRURL
	return candidate
}

func newAuthorizerTransferMarker(repository string, oldID, newID int64) (string, error) {
	nonce := make([]byte, 16)
	if _, err := cryptorand.Read(nonce); err != nil {
		return "", fmt.Errorf("create setup authorizer transfer identity: %w", err)
	}
	return fmt.Sprintf("<!-- hive-authorizer-transfer: %s:%d:%d:%s -->", strings.ToLower(strings.TrimSpace(repository)), oldID, newID, hex.EncodeToString(nonce)), nil
}

func validAuthorizerTransferMarker(marker, repository string, oldID, newID int64) bool {
	prefix := fmt.Sprintf("<!-- hive-authorizer-transfer: %s:%d:%d", strings.ToLower(strings.TrimSpace(repository)), oldID, newID)
	// Accept the pre-transaction marker only so an intent created by the prior
	// release can still be cancelled. Every new intent receives a nonce, which
	// prevents its closed PR from colliding with a later restarted transfer.
	if marker == prefix+" -->" {
		return true
	}
	if !strings.HasPrefix(marker, prefix+":") || !strings.HasSuffix(marker, " -->") {
		return false
	}
	nonce := strings.TrimSuffix(strings.TrimPrefix(marker, prefix+":"), " -->")
	if len(nonce) != 32 {
		return false
	}
	_, err := hex.DecodeString(nonce)
	return err == nil
}

func authorizerTransferBody(intent AuthorizerTransferIntent) string {
	return fmt.Sprintf("%s\n\nHive-managed setup authorizer transfer.\n\n- Current authorizer: `%s` (numeric ID `%d`)\n- Proposed authorizer: `%s` (numeric ID `%d`)\n- Accountable reason: %s\n\nThe current authorizer created the exact out-of-band status for this immutable diff. Local authority changes only after this exact PR merges and Hive verifies the complete managed tree at the current default-branch head.",
		intent.Marker, intent.OldAuthorizerLogin, intent.OldAuthorizerID, intent.NewAuthorizerLogin, intent.NewAuthorizerID, intent.Reason)
}

// AuthorizerTransferNextCommand returns the exact resumable finalization
// command surfaced by status and MCP results.
func AuthorizerTransferNextCommand(stateDir string, intent AuthorizerTransferIntent) string {
	return fmt.Sprintf("hive transfer-setup-authorizer --state-dir %q --new-authorizer %q --reason %q --json", strings.TrimSpace(stateDir), intent.NewAuthorizerLogin, intent.Reason)
}

// AuthorizerTransferCancelCommand returns the supported audited abort path.
func AuthorizerTransferCancelCommand(stateDir string) string {
	return fmt.Sprintf("hive transfer-setup-authorizer --state-dir %q --cancel --json", strings.TrimSpace(stateDir))
}

func rejectPendingAuthorizerTransfer(store *Store, stateDir, operation string) error {
	intent, exists, err := store.LoadAuthorizerTransferIntent()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return fmt.Errorf("%s is blocked by the pending setup authorizer transfer to %s/%d; finish %s or, before merge, abort through %s", operation, intent.NewAuthorizerLogin, intent.NewAuthorizerID, AuthorizerTransferNextCommand(stateDir, intent), AuthorizerTransferCancelCommand(stateDir))
}

func authorizerTransferAuditDetail(intent AuthorizerTransferIntent, actor, suffix string) string {
	value := fmt.Sprintf("actor=%s old=%s/%d new=%s/%d branch=%s head=%s pr=%d reason=%q", actor, intent.OldAuthorizerLogin, intent.OldAuthorizerID, intent.NewAuthorizerLogin, intent.NewAuthorizerID, intent.Branch, intent.HeadSHA, intent.PRNumber, intent.Reason)
	if strings.TrimSpace(suffix) != "" {
		value += " " + strings.TrimSpace(suffix)
	}
	return value
}

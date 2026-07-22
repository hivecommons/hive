package integrated

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

// NormalVisualWork is the sealed production artifact returned to the normal
// Hive service. Fetching it performs the existing exact dispatch, job,
// artifact, producer, source-index, and live-head verification, but deliberately
// performs no legacy lifecycle, bead, repair-manager, baseline, or merge work.
type NormalVisualWork struct {
	Config   Config
	Workflow WorkflowRunEvidence
	Artifact hivegithub.VerifiedVisualHiveArtifact
}

// ErrNormalVisualSetupBaselinePending means the final read-only production
// gate found a valid setup-baseline checkpoint that is not production-ready.
// Errors while advancing that checkpoint retain their own identity and are
// reported normally; this sentinel is not authority to bypass or approve it.
var ErrNormalVisualSetupBaselinePending = errors.New("normal Visual Hive setup baseline is pending")

// UsesNormalHiveRuntime reports whether the existing ordinary Hive/dashboard
// process owns local Visual Hive repair cadence. Advisory and issues-only local
// installations retain the legacy scheduler, while hosted installations remain
// owned by their repository controller.
func UsesNormalHiveRuntime(config Config) bool {
	return config.ExecutionMode == ExecutionLocal && config.VisualHive &&
		(config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge)
}

// FetchNormalVisualWork reuses the released integrated setup-baseline state
// machine and production transport/verifier as a narrow source for the normal
// service. The caller must own the shared production-run lease for its complete
// lifetime. Baseline approval remains an exact external operator action; no
// normal production dispatch is permitted before production verification.
func FetchNormalVisualWork(ctx context.Context, stateDir string, timeout time.Duration, client *hivegithub.Client) (NormalVisualWork, error) {
	if client == nil || stateDir == "" {
		return NormalVisualWork{}, errors.New("normal Visual Hive fetch requires GitHub and persistent state")
	}
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return NormalVisualWork{}, err
	}
	config, err := store.Load()
	if err != nil {
		return NormalVisualWork{}, err
	}
	if config.ExecutionMode == ExecutionHosted {
		return NormalVisualWork{}, errors.New("hosted installation remains owned by its GitHub controller")
	}
	if config.Automation != AutomationRepairPR && config.Automation != AutomationAutoMerge {
		return NormalVisualWork{}, errors.New("normal governed repair service requires repair-pr authority")
	}
	pauseRequested, err := PauseRequested(stateDir)
	if err != nil {
		return NormalVisualWork{}, fmt.Errorf("read pause request: %w", err)
	}
	if config.Paused || pauseRequested {
		return NormalVisualWork{}, errors.New("repository automation is paused")
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, client, config); err != nil {
		return NormalVisualWork{}, err
	}
	if err := VerifyInstalledSetup(ctx, client, config); err != nil {
		return NormalVisualWork{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := reconcileNormalVisualSetupBaselineBeforeProduction(runCtx, store, config, client); err != nil {
		return NormalVisualWork{}, err
	}
	// Keep a final read-only gate adjacent to the mutation. The production-run
	// lease excludes setup changes between reconciliation and this dispatch,
	// while this check prevents a future refactor from silently removing the
	// production-verified prerequisite.
	if err := requireNormalVisualSetupBaselineProductionReady(store, config); err != nil {
		return NormalVisualWork{}, err
	}
	workflow, err := dispatchAndWait(runCtx, client, config)
	if err != nil {
		return NormalVisualWork{}, err
	}
	_, verified, err := client.FetchAndVerifyVisualHiveBundle(runCtx, hivegithub.VisualHiveArtifactRequest{
		Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact,
		SourceArtifactID: workflow.EvidenceArtifact, DestinationDir: filepath.Join(stateDir, "visual-hive", "artifacts"),
		FetchSourceArtifact: true, TargetRef: config.DefaultBranch, MaxACMM: config.ACMMLevel,
		ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath,
		ExpectedEvent: visualHiveProductionWorkflowEvent, ExpectedRunName: workflowDispatchDisplayTitle(workflow.CorrelationID),
		ExpectedProducerGitCommit: config.VisualHiveRef, AllowFailedWorkflowRun: workflow.RepositoryTestOverall != 0,
	})
	if err != nil {
		return NormalVisualWork{}, err
	}
	if _, err := requireLiveInstalledWorkflowHead(runCtx, client, config, workflow.HeadSHA); err != nil {
		var stale *staleWorkflowHeadError
		if errors.As(err, &stale) {
			if discardErr := discardStaleWorkflowDispatch(stateDir, config, workflow, stale); discardErr != nil {
				return NormalVisualWork{}, discardErr
			}
		}
		return NormalVisualWork{}, err
	}
	if _, err := synchronizeNormalVisualWorkBase(runCtx, config, workflow.HeadSHA); err != nil {
		return NormalVisualWork{}, err
	}
	return NormalVisualWork{Config: config, Workflow: workflow, Artifact: verified}, nil
}

func reconcileNormalVisualSetupBaselineBeforeProduction(ctx context.Context, store *Store, config Config, client *hivegithub.Client) error {
	intent, blocked, err := reconcileRequiredSetupBaselineBeforeRun(ctx, store, config, client)
	if !blocked {
		return err
	}
	phase := strings.TrimSpace(intent.Phase)
	if phase == "" {
		phase = "unknown"
	}
	if err != nil {
		return fmt.Errorf("advance normal Visual Hive setup baseline phase %s: %w", phase, err)
	}
	return fmt.Errorf("%w: phase %s", ErrNormalVisualSetupBaselinePending, phase)
}

func requireNormalVisualSetupBaselineProductionReady(store *Store, config Config) error {
	if store == nil {
		return errors.New("normal Visual Hive production gate requires persistent state")
	}
	if rebind, exists, err := store.LoadSetupBaselineRebindIntent(); err != nil {
		return fmt.Errorf("read normal Visual Hive setup baseline rebind gate: %w", err)
	} else if exists {
		return fmt.Errorf("%w: reconfiguration phase %s", ErrNormalVisualSetupBaselinePending, rebind.Phase)
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return fmt.Errorf("read normal Visual Hive setup baseline gate: %w", err)
	}
	if !exists {
		if config.SetupBaselineRequired {
			return fmt.Errorf("%w: required checkpoint is missing", ErrNormalVisualSetupBaselinePending)
		}
		return nil
	}
	if intent.Repository != config.Repository || intent.RepositoryID != config.RepositoryID || intent.DefaultBranch != config.DefaultBranch || intent.AuthorizerID != config.SetupAuthorizationActorID ||
		intent.SetupPRNumber != config.SetupPRNumber || intent.SetupPRURL != config.SetupPRURL || !strings.EqualFold(intent.SetupHeadSHA, config.SetupHeadSHA) ||
		!strings.EqualFold(intent.VisualHiveConfigDigest, config.VisualHiveConfigDigest) || !strings.EqualFold(intent.ScreenshotContractDigest, config.SetupBaselineContractDigest) ||
		!strings.EqualFold(intent.InitialBaselineDigest, config.SetupBaselineInitialDigest) || !reflect.DeepEqual(intent.InitialBaselineCandidates, config.SetupBaselineInitialCandidates) {
		return errors.New("normal Visual Hive production gate found a setup baseline bound to different installed configuration")
	}
	if intent.Phase != SetupBaselineProductionVerified || intent.PendingAudit != nil {
		return fmt.Errorf("%w: phase %s", ErrNormalVisualSetupBaselinePending, intent.Phase)
	}
	return nil
}

// NormalVisualWorkRequiresSetupQuiescence reports only setup states that must
// be completed by a command outside the ordinary service cycle. The service
// remains active for every machine-owned baseline phase, but releases the
// production lease while exact human approval is held or a setup rebind owns
// the next command.
func NormalVisualWorkRequiresSetupQuiescence(stateDir string) (bool, error) {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return true, err
	}
	if _, exists, err := store.LoadSetupBaselineRebindIntent(); err != nil {
		return true, err
	} else if exists {
		return true, nil
	}
	intent, exists, err := store.LoadSetupBaselineIntent()
	if err != nil {
		return true, err
	}
	return exists && intent.Phase == SetupBaselinePROpen, nil
}

// synchronizeNormalVisualWorkBase makes the already verified live workflow
// head available to the normal repair controller without moving or cleaning
// the Hive-owned checkout. The explicit refspec prevents a target-controlled
// remote name or ref from widening this authenticated transport boundary.
func synchronizeNormalVisualWorkBase(ctx context.Context, config Config, expectedHead string) (string, error) {
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	if !immutableCommit.MatchString(expectedHead) {
		return "", errors.New("normal Visual Hive work requires an immutable verified workflow head")
	}
	branch := strings.TrimSpace(config.DefaultBranch)
	if !validLegacyBranchName(branch) {
		return "", errors.New("normal Visual Hive work requires a safe installed default branch")
	}
	exists, err := validateManagedCheckoutBeforeGit(config.CheckoutDir, config.Repository)
	if err != nil {
		return "", fmt.Errorf("validate managed checkout before normal Visual Hive synchronization: %w", err)
	}
	if !exists {
		return "", errors.New("normal Visual Hive managed checkout is unavailable")
	}
	remoteURL := RepositoryCloneURL(config.Repository)
	remoteRef := "refs/remotes/origin/" + branch
	refspec := "+refs/heads/" + branch + ":" + remoteRef
	if _, err := gitTransport(ctx, config.CheckoutDir, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return "", fmt.Errorf("fetch verified normal Visual Hive base: %w", err)
	}
	fetchedHead, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched normal Visual Hive head: %w", err)
	}
	fetchedHead = strings.ToLower(strings.TrimSpace(fetchedHead))
	if !immutableCommit.MatchString(fetchedHead) || fetchedHead != expectedHead {
		return "", fmt.Errorf("fetched default-branch head %s does not match verified workflow head %s", fetchedHead, expectedHead)
	}
	tree, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", expectedHead+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("resolve verified normal Visual Hive base tree: %w", err)
	}
	tree = strings.ToLower(strings.TrimSpace(tree))
	if !validNormalVisualGitObjectID(tree) {
		return "", errors.New("verified normal Visual Hive base tree identity is invalid")
	}
	return tree, nil
}

func validNormalVisualGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ConsumeNormalVisualWork retires only the exact durable workflow dispatch.
// allowAlreadyConsumed is used solely for crash recovery after the service
// durably recorded its final exact-head receipt before deleting the intent.
func ConsumeNormalVisualWork(stateDir string, workflow WorkflowRunEvidence, allowAlreadyConsumed bool) error {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	_, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return err
	}
	if !exists && allowAlreadyConsumed {
		return nil
	}
	return consumeWorkflowDispatch(stateDir, workflow)
}

// AcquireNormalVisualWorkLease claims the same OS-backed lifetime ownership as
// legacy RunOnce. Its long metadata horizon is informational; the kernel lock
// and legacy compatibility sentinel remain authoritative until release.
func AcquireNormalVisualWorkLease(stateDir string) (func(), error) {
	return acquireProductionRunLease(stateDir, 10*365*24*time.Hour)
}

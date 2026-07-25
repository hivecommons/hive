package integrated

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/checkpoint"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"gopkg.in/yaml.v3"
)

const (
	checkoutActionSHA           = "9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0" // actions/checkout v7.0.0
	setupNodeActionSHA          = "48b55a011bda9f5d6aeb4c2d9c7362e8dae4041e" // actions/setup-node v6.4.0
	setupPythonActionSHA        = "a309ff8b426b58ec0e2a45f0f869d46889d02405" // actions/setup-python v6.2.0
	uploadArtifactActionSHA     = "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a" // actions/upload-artifact v7.0.1
	managedPreimagesVersion     = "hive.managed-path-preimages.v1"
	maxManagedPreimageBytes     = 4 << 20
	maxManagedPreimageFile      = 1 << 20
	directBootstrapMaxBaselines = 200
	directBootstrapMaxBaseline  = 20 << 20
	directBootstrapMaxTotal     = 500 << 20
	directBootstrapMaxPathBytes = 512
	defaultNormalCodexModel     = "gpt-5.6-sol"
)

var (
	strictPackageRunPattern = regexp.MustCompile(`^(npm|pnpm|yarn)[[:space:]]+(--silent[[:space:]]+)?run[[:space:]]+([A-Za-z0-9_.:-]+)([[:space:]]+--([[:space:]]+[A-Za-z0-9_./:@%+=,-]+)*)?$`)
	safeShellTokenPattern   = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,\\-]+$`)
	environmentTokenPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=[A-Za-z0-9_./:@%+=,\\-]+$`)
	directBootstrapSafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	setupRepositoryCloneURL = func(repository string) string { return "https://github.com/" + repository + ".git" }
)

// RepositoryCloneURL returns the canonical exact Git transport URL used by
// setup and every governed repair path for the same repository identity.
func RepositoryCloneURL(repository string) string {
	return strings.TrimSpace(setupRepositoryCloneURL(strings.TrimSpace(repository)))
}

type packageScriptInvocation struct {
	Name      string
	Forwarded string
}

type directBootstrapProof struct {
	RepositoryID  string
	DefaultBranch string
	BaseSHA       string
	SeedCommits   int
}

type SetupOptions struct {
	Repository                 string
	Coverage                   Coverage
	Automation                 Automation
	Provider                   string
	ProviderCommand            string
	ProviderArgs               []string
	ExecutionMode              ExecutionMode
	RunInterval                time.Duration
	HostedSchedule             string
	HiveReleaseRepository      string
	HiveReleaseVersion         string
	HiveCommit                 string
	DistributionManifestSHA256 string
	HostedControllerProtocol   int
	VisualHive                 bool
	StateDir                   string
	Apply                      bool
	Start                      bool
	// DirectBootstrap is a deliberately narrow no-PR install path for a new,
	// private, disposable repository. It is never inferred from repository
	// shape and is rejected for an existing durable installation.
	DirectBootstrap        bool
	AdoptReviewedBaselines bool
	ExpectedSeedSHA        string
	ReviewedBaselineDigest string
	VisualHiveCommand      string
	VisualHiveArgs         []string
	VisualHiveRepo         string
	VisualHiveRef          string
	MaxActiveIssues        int
	MaxRepairAttempts      int
	AllowedAutoMergePaths  []string
	AllowedAutoMergeRisk   []automation.RiskTier
	AutoMergePathsExplicit bool
	AutoMergeRiskExplicit  bool
	GitHub                 *hivegithub.Client
	GitTransportToken      string
	Policy                 automation.Policy
}

type setupPRClient interface {
	UpsertRepairPullRequest(context.Context, string, string, string, string, string, string, string) (hivegithub.RepairPullRequest, error)
}

func RunSetup(ctx context.Context, options SetupOptions) (SetupResult, error) {
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	if options.ExecutionMode == "" {
		options.ExecutionMode = ExecutionLocal
	}
	var err error
	options, err = NormalizeNormalHiveProvider(options)
	if err != nil {
		return SetupResult{}, err
	}
	if options.RunInterval == 0 {
		options.RunInterval = 15 * time.Minute
	}
	stateDir, err := filepath.Abs(options.StateDir)
	if err != nil {
		return SetupResult{}, fmt.Errorf("resolve persistent state directory: %w", err)
	}
	options.StateDir = filepath.Clean(stateDir)
	if err := validateSetupOptions(options); err != nil {
		return SetupResult{}, err
	}
	requestedStateDir := options.StateDir
	var readOnlyPlanRoot string
	if !options.Apply {
		configPath := filepath.Join(options.StateDir, "integrated", "config.json")
		if _, statErr := os.Lstat(configPath); os.IsNotExist(statErr) {
			readOnlyPlanRoot, err = os.MkdirTemp("", "hive-setup-plan-")
			if err != nil {
				return SetupResult{}, fmt.Errorf("create ephemeral read-only setup workspace: %w", err)
			}
			defer os.RemoveAll(readOnlyPlanRoot)
			options.StateDir = filepath.Join(readOnlyPlanRoot, "state")
		} else if statErr != nil {
			return SetupResult{}, fmt.Errorf("inspect existing setup state before read-only planning: %w", statErr)
		}
	}
	if err := validateSetupStateRoot(options.StateDir, options.Repository); err != nil {
		return SetupResult{}, err
	}
	if options.Apply {
		release, leaseErr := acquireProductionRunLease(options.StateDir, 25*time.Minute)
		if leaseErr != nil {
			return SetupResult{}, fmt.Errorf("serialize setup with production runs: %w", leaseErr)
		}
		defer release()
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return SetupResult{}, err
	}
	prior, priorErr := store.Load()
	hasPrior := priorErr == nil
	if priorErr != nil && !errors.Is(priorErr, os.ErrNotExist) {
		return SetupResult{}, fmt.Errorf("load existing integrated configuration: %w", priorErr)
	}
	if hasPrior && !strings.EqualFold(prior.Repository, options.Repository) {
		return SetupResult{}, fmt.Errorf("state directory %s is already bound to %s; use a different --state-dir for %s", options.StateDir, prior.Repository, options.Repository)
	}
	directIntent, directIntentExists, directIntentErr := store.loadDirectBootstrapIntent()
	if directIntentErr != nil {
		return SetupResult{}, fmt.Errorf("load direct-bootstrap recovery intent: %w", directIntentErr)
	}
	if directIntentExists {
		if !options.DirectBootstrap || !options.Apply {
			return SetupResult{}, fmt.Errorf("an exact direct-bootstrap recovery is pending; rerun apply with the original direct-bootstrap request")
		}
		if options.GitHub == nil {
			return SetupResult{}, fmt.Errorf("GitHub client is required to recover direct bootstrap")
		}
		return recoverDirectBootstrap(ctx, options, store, prior, hasPrior, directIntent)
	}
	if options.DirectBootstrap && hasPrior {
		if options.Apply && options.GitHub != nil {
			if recovered, recognized, replayErr := replayCompletedDirectBootstrap(ctx, options, store, prior); recognized {
				return recovered, replayErr
			}
		}
		return SetupResult{}, fmt.Errorf("direct bootstrap requires a fresh state directory with no prior durable installation or an exact completed-bootstrap audit receipt")
	}
	existingHosted := hasPrior && normalizedExecutionMode(prior.ExecutionMode) == ExecutionHosted
	if existingHosted && options.ExecutionMode != ExecutionHosted {
		return SetupResult{}, fmt.Errorf("setup cannot replace an active hosted installation with local runtime state; use an explicit hosted-to-local migration operation")
	}
	if existingHosted && options.ExecutionMode == ExecutionHosted {
		activeRelease := hostedReleaseIdentity(prior)
		requestedRelease := HostedReleaseIdentity{
			Version: options.HiveReleaseVersion, HiveCommit: strings.ToLower(strings.TrimSpace(options.HiveCommit)),
			VisualHiveCommit:           strings.ToLower(strings.TrimSpace(options.VisualHiveRef)),
			DistributionManifestSHA256: strings.ToLower(strings.TrimSpace(options.DistributionManifestSHA256)),
			HostedControllerProtocol:   options.HostedControllerProtocol,
		}
		if !validHostedReleaseIdentity(activeRelease) {
			return SetupResult{}, fmt.Errorf("existing hosted installation does not have a supported immutable controller identity; use hive upgrade or an explicit migration path before rerunning setup")
		}
		if !equalHostedReleaseIdentity(activeRelease, requestedRelease) {
			return SetupResult{}, fmt.Errorf("setup cannot replace the active hosted release before an exact managed transition; use hive upgrade or hive rollback")
		}
	}
	if options.Apply {
		if transfer, exists, transferErr := store.LoadAuthorizerTransferIntent(); transferErr != nil {
			return SetupResult{}, transferErr
		} else if exists {
			return SetupResult{}, fmt.Errorf("setup authorizer transfer to %s (numeric ID %d) is pending; finish its exact next command before changing managed setup", transfer.NewAuthorizerLogin, transfer.NewAuthorizerID)
		}
	}
	if options.Apply && options.GitHub == nil {
		return SetupResult{}, fmt.Errorf("GitHub client is required to apply setup")
	}
	var legacyCheckout *legacyManagedCheckoutProof
	if hasPrior && options.GitHub != nil {
		liveRepositoryID, identityErr := verifyLiveRepositoryIdentity(ctx, options.GitHub, prior)
		if identityErr != nil {
			return SetupResult{}, fmt.Errorf("verify existing setup repository identity before checkout access: %w", identityErr)
		}
		legacyCheckout = &legacyManagedCheckoutProof{Config: prior, LiveRepositoryID: liveRepositoryID}
	}
	checkout, err := managedSetupCheckoutPath(store.Dir(), options.StateDir, options.Repository, prior, hasPrior)
	if err != nil {
		return SetupResult{}, err
	}
	defaultBranch, err := ensureCheckoutWithLegacy(ctx, options.Repository, checkout, legacyCheckout)
	if err != nil {
		return SetupResult{}, fmt.Errorf("setup checkout in state directory %s: %w", options.StateDir, err)
	}
	if err := validateOrdinarySetupCheckout(checkout); err != nil {
		return SetupResult{}, err
	}
	inspection, err := InspectCheckout(checkout, defaultBranch)
	if err != nil {
		return SetupResult{}, err
	}
	if options.GitHub != nil {
		enrichRemoteInspection(ctx, options.GitHub, options.Repository, &inspection)
	}
	if !options.Apply && strings.TrimSpace(inspection.RepositoryID) != "" {
		if err := ensureStateOwnershipMarker(options.StateDir, Config{Repository: options.Repository, RepositoryID: inspection.RepositoryID}); err != nil {
			return SetupResult{}, fmt.Errorf("bind planned state directory to exact repository identity: %w", err)
		}
	}
	options = effectiveSetupAutoMergePolicy(options, prior, hasPrior)
	planOptions := options
	planOptions.StateDir = requestedStateDir
	plan := buildSetupPlan(planOptions, inspection)
	result := SetupResult{Plan: plan, ProtectionActivationPending: options.Automation == AutomationAutoMerge, ActivationMessage: setupActivationMessage(options.Automation, options.Start, true)}
	if !options.Apply {
		return result, nil
	}
	if strings.TrimSpace(inspection.RepositoryID) == "" {
		return result, fmt.Errorf("GitHub did not return the immutable repository ID for %s; refusing setup mutations", options.Repository)
	}
	authorizer, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil {
		return result, err
	}
	if options.VisualHive {
		if err := VerifyVisualHiveWorkflowCommits(ctx, options.GitHub, options.VisualHiveRepo, options.VisualHiveRef); err != nil {
			return result, err
		}
	}
	directProof := directBootstrapProof{}
	if options.DirectBootstrap {
		directProof, err = inspectDirectBootstrapTarget(ctx, options.GitHub, options.Repository, inspection.RepositoryID, checkout, defaultBranch)
		if err != nil {
			return result, err
		}
		if err := validateDirectBootstrapExpectedSeed(options, directProof); err != nil {
			return result, err
		}
	}
	branch := managedOperationBranch("setup", inspection.RepositoryID)
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupBranch); err != nil {
		return result, err
	}
	remoteURL := RepositoryCloneURL(options.Repository)
	defaultRefspec := "+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
	if _, err := gitTransport(ctx, checkout, remoteURL, "fetch", "--prune", "--no-recurse-submodules", remoteURL, defaultRefspec); err != nil {
		return result, err
	}
	if _, err := git(ctx, checkout, "switch", "-C", branch, "origin/"+defaultBranch); err != nil {
		return result, err
	}
	if err := validateOrdinarySetupCheckout(checkout); err != nil {
		return result, err
	}
	preimagesVersion, managedPreimages, preimagesUpdated, err := resolveManagedPathPreimages(ctx, checkout, prior, hasPrior, options.VisualHive, options.ExecutionMode)
	if err != nil {
		return result, fmt.Errorf("capture repository-owned managed path preimages: %w", err)
	}
	if options.VisualHive {
		if err := runVisualHiveSetup(ctx, options, checkout); err != nil {
			return result, err
		}
		if err := validateOrdinarySetupCheckout(checkout); err != nil {
			return result, fmt.Errorf("Visual Hive static setup produced an unsafe checkout entry: %w", err)
		}
	}
	setupBaselineRequired := false
	setupBaselineContractDigest := ""
	var adoptedBaselineCandidates []SetupBaselineCandidate
	adoptedBaselineDigest := ""
	if options.VisualHive {
		requiresBaselines, contractDigest, baselinePlanErr := visualHiveScreenshotContractInventory(checkout)
		if baselinePlanErr != nil {
			return result, baselinePlanErr
		}
		// A partial baseline inventory is not sufficient: hosted capture must
		// prove every configured screenshot contract. Proposal creation later
		// filters the capture against the exact target head and reviews only the
		// missing or changed PNG delta, preserving identical reviewed bytes.
		setupBaselineRequired = requiresBaselines
		setupBaselineContractDigest = contractDigest
	}
	if options.DirectBootstrap {
		switch {
		case setupBaselineRequired && !options.AdoptReviewedBaselines:
			return result, fmt.Errorf("direct bootstrap found screenshot contracts; separately opt into adoption of exact reviewed baseline bytes already present in the seed commit")
		case !setupBaselineRequired && options.AdoptReviewedBaselines:
			return result, fmt.Errorf("reviewed baseline adoption was requested but the generated Visual Hive configuration has no screenshot contracts")
		case setupBaselineRequired:
			adoptedBaselineCandidates, adoptedBaselineDigest, err = validateDirectBootstrapReviewedBaselines(ctx, checkout, directProof.BaseSHA)
			if err != nil {
				return result, err
			}
			if err := validateDirectBootstrapReviewedDigest(options, adoptedBaselineDigest); err != nil {
				return result, err
			}
			// This one exact seed inventory has already been reviewed by the
			// operator. Do not persist a reusable adoption flag: every later
			// setup/configuration change returns to the normal capture/PR gate.
			setupBaselineRequired = false
		}
	}
	setupBranch := branch
	if options.DirectBootstrap {
		// The default branch is an inert setup-authorization head (a same-repo
		// pull request cannot use the default branch as both head and base). It
		// keeps the generated workflow valid without naming a setup branch that
		// direct bootstrap never creates.
		setupBranch = defaultBranch
	}
	config := Config{
		SchemaVersion: ConfigSchema, Repository: options.Repository, RepositoryID: inspection.RepositoryID, DefaultBranch: defaultBranch,
		Coverage: options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ProviderCommand: options.ProviderCommand, ProviderArgs: append([]string(nil), options.ProviderArgs...),
		ExecutionMode: options.ExecutionMode, RunIntervalSeconds: int64(options.RunInterval / time.Second),
		HostedSchedule:        options.HostedSchedule,
		HostedStateBranch:     hostedStateBranch(inspection.RepositoryID),
		HiveReleaseRepository: options.HiveReleaseRepository, HiveReleaseVersion: options.HiveReleaseVersion,
		HiveCommit: options.HiveCommit, DistributionManifestSHA256: options.DistributionManifestSHA256,
		HostedControllerProtocol: options.HostedControllerProtocol,
		ACMMLevel:                acmmForAutomation(options.Automation), VisualHive: options.VisualHive,
		SetupBaselineRequired:          setupBaselineRequired,
		SetupBaselineContractDigest:    setupBaselineContractDigest,
		SetupBaselineInitialDigest:     adoptedBaselineDigest,
		SetupBaselineInitialCandidates: append([]SetupBaselineCandidate(nil), adoptedBaselineCandidates...),
		MaxActiveIssues:                options.MaxActiveIssues,
		MaxRepairAttempts:              options.MaxRepairAttempts,
		VisualHiveRepo:                 options.VisualHiveRepo, VisualHiveRef: options.VisualHiveRef,
		VisualHiveCommand: options.VisualHiveCommand, VisualHiveArgs: append([]string(nil), options.VisualHiveArgs...),
		ManagedPreimagesVersion: preimagesVersion, ManagedPathPreimages: managedPreimages,
		TestCommands: testCommandsForCoverage(inspection, options.Coverage), AllowedRepairPaths: defaultAllowedRepairPaths(),
		AllowedAutoMergePaths: append([]string(nil), options.AllowedAutoMergePaths...),
		AllowedAutoMergeRisk:  append([]automation.RiskTier(nil), options.AllowedAutoMergeRisk...),
		CheckoutDir:           checkout, StateDir: options.StateDir, SetupBranch: setupBranch,
		Paused:                    options.ExecutionMode == ExecutionHosted && !options.Start,
		SetupAuthorizationActorID: authorizer.ID,
		InstalledAt:               time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if options.VisualHive {
		config.VisualHiveConfigDigest, err = visualHiveConfigDigest(checkout)
		if err != nil {
			return result, err
		}
	}
	if hasPrior {
		config.InstalledAt = prior.InstalledAt
		config.SetupBranch, config.SetupPRNumber, config.SetupPRURL, config.SetupHeadSHA = prior.SetupBranch, prior.SetupPRNumber, prior.SetupPRURL, prior.SetupHeadSHA
		config.PreviousVersion, config.Paused = prior.PreviousVersion, prior.Paused
		if options.ExecutionMode == ExecutionHosted && normalizedExecutionMode(prior.ExecutionMode) == ExecutionHosted && validHostedReleaseIdentity(hostedReleaseIdentity(prior)) {
			current := hostedReleaseIdentity(config)
			priorRelease := hostedReleaseIdentity(prior)
			if !equalHostedReleaseIdentity(current, priorRelease) {
				config.PreviousHostedRelease = &priorRelease
			} else {
				config.PreviousHostedRelease = cloneHostedReleaseIdentity(prior.PreviousHostedRelease)
			}
		}
		if options.ExecutionMode == ExecutionHosted && options.Start {
			config.Paused = false
		}
		config.SetupBaselineInitialDigest = prior.SetupBaselineInitialDigest
		config.SetupBaselineInitialCandidates = append([]SetupBaselineCandidate(nil), prior.SetupBaselineInitialCandidates...)
		config.AllowedRepairPaths = append([]string(nil), prior.AllowedRepairPaths...)
		if prior.SetupAuthorizationActorID > 0 {
			config.SetupAuthorizationActorID = prior.SetupAuthorizationActorID
		}
	}
	if config.ExecutionMode == ExecutionHosted {
		controller, controllerErr := GenerateHostedControllerWorkflow(hostedWorkflowConfig(config))
		if controllerErr != nil {
			return result, fmt.Errorf("generate release-bound hosted controller: %w", controllerErr)
		}
		digest := sha256.Sum256([]byte(controller))
		config.HostedWorkflowSHA256 = hex.EncodeToString(digest[:])
		result.Plan.HostedControllerProtocol = config.HostedControllerProtocol
		result.Plan.HostedWorkflowSHA256 = config.HostedWorkflowSHA256
	}
	// The exact repository preimages must outlive any branch push or setup PR.
	// Persist them before the first remote mutation so an interrupted setup can
	// resume without reclassifying already-managed bytes as repository-owned.
	if preimagesUpdated && !options.DirectBootstrap {
		if existingHosted {
			return result, fmt.Errorf("setup cannot replace the active hosted managed-path ownership ledger; use a dedicated hosted policy transition")
		}
		if err := store.Save(config); err != nil {
			return result, fmt.Errorf("persist managed path preimages before setup publication: %w", err)
		}
	}
	if err := writeManagedFiles(checkout, config, inspection); err != nil {
		return result, err
	}
	managed := append([]string(nil), plan.FilesToManage...)
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupCommit); err != nil {
		return result, err
	}
	if err := stageManagedPaths(ctx, checkout, managed); err != nil {
		return result, err
	}
	diff, err := git(ctx, checkout, "diff", "--cached", "--name-only")
	if err != nil {
		return result, err
	}
	// A different authenticated maintainer rerunning an otherwise identical
	// setup remains a true no-op. A substantive change keeps the human identity
	// already embedded in the default-branch verifier: pull_request workflows
	// execute from the base branch, so silently rotating that identity in the
	// proposed head would make the out-of-band status unverifiable.
	if hasPrior && prior.SetupAuthorizationActorID > 0 && authorizer.ID != prior.SetupAuthorizationActorID && strings.TrimSpace(diff) != "" {
		return result, fmt.Errorf("managed setup changes are bound to GitHub user ID %d, but the current authenticated user is %d; rerun with the recorded setup authorizer (an identical cross-user rerun remains a no-op)", prior.SetupAuthorizationActorID, authorizer.ID)
	}
	if !options.DirectBootstrap && updateSetupAuthorizationBranchForManagedChange(&config, branch, diff) {
		if err := writeManagedFiles(checkout, config, inspection); err != nil {
			return result, err
		}
		if err := stageManagedPaths(ctx, checkout, managed); err != nil {
			return result, err
		}
		diff, err = git(ctx, checkout, "diff", "--cached", "--name-only")
		if err != nil {
			return result, err
		}
	}
	if existingHosted && strings.TrimSpace(diff) != "" {
		return result, fmt.Errorf("setup cannot publish a changed hosted policy while the active configuration still owns the controller; use a dedicated managed policy or release transition command")
	}
	idempotent := strings.TrimSpace(diff) == ""
	if options.DirectBootstrap && idempotent {
		return result, fmt.Errorf("direct bootstrap produced no managed setup change; refusing to advance the default branch")
	}
	reuseRemoteSetup := false
	sha := ""
	if !idempotent && hasPrior && prior.SetupBranch != "" {
		matches, remoteSHA, matchErr := stagedTreeMatchesRemoteBranch(ctx, checkout, prior.SetupBranch)
		if matchErr != nil {
			return result, matchErr
		}
		if matches {
			idempotent, reuseRemoteSetup, sha, branch = true, true, remoteSHA, prior.SetupBranch
		}
	}
	knownSetupHead := config.SetupHeadSHA
	if reuseRemoteSetup {
		knownSetupHead = sha
	}
	if err := reconcileActiveSetupBaselineReconfiguration(ctx, store, config, knownSetupHead, idempotent, options.GitHub); err != nil {
		return result, err
	}
	if idempotent && hasPrior && !reuseRemoteSetup {
		sha, shaErr := git(ctx, checkout, "rev-parse", "HEAD")
		if shaErr != nil {
			return result, shaErr
		}
		if config.SetupBaselineRequired && config.SetupBaselineInitialDigest == "" {
			config.SetupBaselineInitialCandidates, config.SetupBaselineInitialDigest, err = setupBaselineInventoryAtCommit(ctx, checkout, strings.TrimSpace(sha))
			if err != nil {
				return result, fmt.Errorf("bind exact pre-setup baseline inventory: %w", err)
			}
		}
		if err := store.Save(config); err != nil {
			return result, err
		}
		if err := setSetupBaselineStatus(&result, store, config, config.SetupHeadSHA); err != nil {
			return result, err
		}
		if err := completeSetupBaselineRebind(store, config); err != nil {
			return result, err
		}
		result.Applied, result.Idempotent, result.Config = true, true, publicSetupConfig(config)
		result.Branch, result.PRNumber, result.PRURL = prior.SetupBranch, prior.SetupPRNumber, prior.SetupPRURL
		result.CommitSHA = strings.TrimSpace(sha)
		if err := setSetupActivationStatus(&result, store, config, options.Start); err != nil {
			return result, err
		}
		return result, nil
	}
	if !idempotent {
		if _, err := git(ctx, checkout, "-c", "user.name=Hive Setup", "-c", "user.email=hive-setup@users.noreply.github.com", "commit", "-m", "chore: install Hive and Visual Hive", "-m", managedCommitTrailers(inspection.RepositoryID, "setup")); err != nil {
			return result, err
		}
	}
	if sha == "" {
		sha, err = git(ctx, checkout, "rev-parse", "HEAD")
		if err != nil {
			return result, err
		}
		sha = strings.TrimSpace(sha)
	}
	if config.SetupBaselineRequired && config.SetupBaselineInitialDigest == "" {
		config.SetupBaselineInitialCandidates, config.SetupBaselineInitialDigest, err = setupBaselineInventoryAtCommit(ctx, checkout, sha)
		if err != nil {
			return result, fmt.Errorf("bind exact pre-setup baseline inventory: %w", err)
		}
	}
	if !options.DirectBootstrap {
		config.SetupBranch = branch
	}
	if options.DirectBootstrap {
		return finishDirectBootstrap(ctx, options, store, config, result, checkout, directProof, sha)
	}
	if !reuseRemoteSetup {
		if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPush); err != nil {
			return result, err
		}
		if err := pushManagedBranch(ctx, checkout, options.Repository, branch, inspection.RepositoryID, "setup", sha); err != nil {
			return result, err
		}
	}
	// A matching remote setup tree is reused without another commit or
	// force-push, while the PR upsert still recovers an interrupted or
	// manually closed setup PR.
	if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPR); err != nil {
		return result, err
	}
	marker := "<!-- hive-setup: " + strings.ToLower(options.Repository) + " -->"
	body := setupPRBody(marker, plan)
	priorSetupPRNumber, priorSetupHeadSHA, priorIdentityErr := priorSetupPullRequestIdentity(prior, hasPrior)
	if priorIdentityErr != nil {
		return result, priorIdentityErr
	}
	pull, err := options.GitHub.UpsertSetupPullRequest(ctx, options.Repository, branch, sha, defaultBranch, "Install Hive + Visual Hive production automation", body, marker, priorSetupPRNumber, priorSetupHeadSHA)
	if err != nil {
		return result, err
	}
	if err := verifyManagedPullHead("setup", pull, sha); err != nil {
		return result, err
	}
	config.SetupPRNumber, config.SetupPRURL = pull.Number, pull.URL
	config.SetupHeadSHA = strings.ToLower(strings.TrimSpace(sha))
	authorization, err := authorizeManagedSetupPullRequest(ctx, store, options.GitHub, options.Policy, config, "setup", checkout, branch, sha, pull.URL, pull.Number)
	if err != nil {
		return result, err
	}
	if err := store.Save(config); err != nil {
		return result, err
	}
	if err := setSetupBaselineStatus(&result, store, config, config.SetupHeadSHA); err != nil {
		return result, err
	}
	if err := completeSetupBaselineRebind(store, config); err != nil {
		return result, err
	}
	result.Applied, result.Idempotent, result.Config = true, idempotent, publicSetupConfig(config)
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = branch, sha, pull.Number, pull.URL
	result.SetupAuthorizationContext, result.SetupAuthorizationStatusID = authorization.Status.Context, authorization.Status.StatusID
	result.SetupAuthorizationCreatorID, result.SetupAuthorizationReused = authorization.Status.CreatorID, authorization.Status.Reused
	if err := setSetupActivationStatus(&result, store, config, options.Start); err != nil {
		return result, err
	}
	return result, nil
}

// NormalizeNormalHiveProvider returns the exact provider binding used by both
// setup preflight and the installed ordinary Hive repair runtime.
func NormalizeNormalHiveProvider(options SetupOptions) (SetupOptions, error) {
	owner := Config{ExecutionMode: normalizedExecutionMode(options.ExecutionMode), VisualHive: options.VisualHive, Automation: options.Automation}
	if !UsesNormalHiveRuntime(owner) {
		return options, nil
	}
	if !strings.EqualFold(strings.TrimSpace(options.Provider), "codex") {
		return options, fmt.Errorf("normal governed repair runtime requires the Codex provider, got %q", options.Provider)
	}
	model, err := repair.CodexSpecialistProviderModel(options.ProviderArgs)
	if err != nil {
		return options, fmt.Errorf("normal governed repair runtime provider model: %w", err)
	}
	if model != "" {
		return options, nil
	}
	options.ProviderArgs = append(append([]string(nil), options.ProviderArgs...), "--model="+defaultNormalCodexModel)
	return options, nil
}

func publicSetupConfig(config Config) *Config {
	config.ManagedPathPreimages = redactManagedPathPreimages(config.ManagedPathPreimages)
	return &config
}

func setSetupBaselineStatus(result *SetupResult, store *Store, config Config, setupHead string) error {
	if result == nil || !config.SetupBaselineRequired {
		return nil
	}
	baseline, err := ensureSetupBaselineIntent(store, config, strings.ToLower(strings.TrimSpace(setupHead)), config.SetupPRNumber, config.SetupPRURL)
	if err != nil {
		return err
	}
	result.SetupBaselinePending, result.SetupBaselinePhase = baseline.Phase != SetupBaselineProductionVerified || baseline.PendingAudit != nil, baseline.Phase
	result.SetupBaselineNextCommand = "hive run --state-dir " + strconv.Quote(config.StateDir) + " --json"
	return nil
}

func updateSetupAuthorizationBranchForManagedChange(config *Config, branch, stagedDiff string) bool {
	if config == nil || strings.TrimSpace(stagedDiff) == "" || strings.TrimSpace(branch) == "" || config.SetupBranch == branch {
		return false
	}
	config.SetupBranch = branch
	return true
}

func setSetupActivationStatus(result *SetupResult, store *Store, config Config, started bool) error {
	if result == nil || config.Automation != AutomationAutoMerge {
		return nil
	}
	state, exists, err := store.LoadProtectionActivation()
	if err != nil {
		return fmt.Errorf("read protection activation status: %w", err)
	}
	result.ProtectionActivationPending = !exists || !ProtectionActivationMatchesConfig(state, config)
	result.ActivationMessage = setupActivationMessage(config.Automation, started, result.ProtectionActivationPending)
	return nil
}

func setupActivationMessage(automation Automation, started, pending bool) string {
	if automation != AutomationAutoMerge {
		return ""
	}
	if !pending {
		return "Exact GitHub-Actions-App-bound PR visual-hive protection is durably activated; Hive verifies the producing PR workflow provenance before merge and hive doctor verifies live policy without changing it."
	}
	if started {
		return "Merge the exact managed setup PR and complete the trusted setup run. Hive has durably recorded the scheduler request and will activate it only after all non-scheduler doctor checks are green, before any lifecycle write."
	}
	return "Merge the exact managed setup PR, then run hive start or hive run. Hive will complete one trusted default-branch Visual Hive run and activate exact GitHub-Actions-App-bound protection before any lifecycle write."
}

func defaultAllowedRepairPaths() []string {
	return []string{"src/**", "**/src/**", "public/**", "**/public/**", "index.html", "**/index.html", "visual-hive.config.yaml", "scripts/testing/**", "**/scripts/testing/**", "test/**", "tests/**", "**/test/**", "**/tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
}

func defaultAllowedAutoMergePaths() []string {
	return []string{"test/**", "tests/**", "**/*.test.*", "**/*.spec.*", "**/*_test.go"}
}

func effectiveSetupAutoMergePolicy(options SetupOptions, prior Config, hasPrior bool) SetupOptions {
	if !options.AutoMergePathsExplicit {
		if hasPrior && len(prior.AllowedAutoMergePaths) > 0 {
			options.AllowedAutoMergePaths = append([]string(nil), prior.AllowedAutoMergePaths...)
		} else {
			options.AllowedAutoMergePaths = defaultAllowedAutoMergePaths()
		}
	}
	if !options.AutoMergeRiskExplicit {
		if hasPrior && len(prior.AllowedAutoMergeRisk) > 0 {
			options.AllowedAutoMergeRisk = append([]automation.RiskTier(nil), prior.AllowedAutoMergeRisk...)
		} else {
			options.AllowedAutoMergeRisk = []automation.RiskTier{automation.RiskAutomatic}
		}
	}
	options.AllowedAutoMergePaths = sortedUnique(options.AllowedAutoMergePaths)
	sort.Slice(options.AllowedAutoMergeRisk, func(i, j int) bool { return options.AllowedAutoMergeRisk[i] < options.AllowedAutoMergeRisk[j] })
	options.AllowedAutoMergeRisk = uniqueRiskTiers(options.AllowedAutoMergeRisk)
	return options
}

func uniqueRiskTiers(values []automation.RiskTier) []automation.RiskTier {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func VerifyVisualHiveCommit(ctx context.Context, client *hivegithub.Client, repository, ref string) error {
	if client == nil || client.GoGitHub() == nil {
		return fmt.Errorf("GitHub client is required to verify the Visual Hive commit")
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || repo == "" {
		return fmt.Errorf("Visual Hive repository must be owner/name")
	}
	commit, _, err := client.GoGitHub().Repositories.GetCommit(ctx, owner, repo, ref, nil)
	if err != nil {
		return fmt.Errorf("verify immutable Visual Hive commit %s in %s: %w", ref, repository, err)
	}
	if !exactCommitPin(ref, commit.GetSHA()) {
		return fmt.Errorf("Visual Hive ref resolved to %s instead of exact commit %s", commit.GetSHA(), ref)
	}
	return nil
}

// VerifyVisualHiveWorkflowCommits verifies both the configured production
// release and the independently audited producer used by the pull request lane.
func VerifyVisualHiveWorkflowCommits(ctx context.Context, client *hivegithub.Client, repository, productionRef string) error {
	if err := VerifyVisualHiveCommit(ctx, client, repository, productionRef); err != nil {
		return err
	}
	if strings.EqualFold(productionRef, visualHivePullRequestProducerCommit) {
		return nil
	}
	if err := VerifyVisualHiveCommit(ctx, client, repository, visualHivePullRequestProducerCommit); err != nil {
		return fmt.Errorf("verify audited Visual Hive pull request producer: %w", err)
	}
	return nil
}

type installedRepositoryConfig struct {
	SchemaVersion                     string                 `json:"schema_version"`
	Repository                        string                 `json:"repository"`
	RepositoryID                      string                 `json:"repository_id"`
	DefaultBranch                     string                 `json:"default_branch"`
	Coverage                          Coverage               `json:"coverage"`
	Automation                        Automation             `json:"automation"`
	Provider                          string                 `json:"provider"`
	ExecutionMode                     ExecutionMode          `json:"execution_mode"`
	RunIntervalSeconds                int64                  `json:"run_interval_seconds"`
	HostedSchedule                    string                 `json:"hosted_schedule,omitempty"`
	HostedStateBranch                 string                 `json:"hosted_state_branch,omitempty"`
	HiveReleaseRepository             string                 `json:"hive_release_repository,omitempty"`
	HiveReleaseVersion                string                 `json:"hive_release_version,omitempty"`
	HiveCommit                        string                 `json:"hive_commit,omitempty"`
	DistributionManifestSHA256        string                 `json:"distribution_manifest_sha256,omitempty"`
	HostedControllerProtocol          int                    `json:"hosted_controller_protocol,omitempty"`
	HostedWorkflowSHA256              string                 `json:"hosted_workflow_sha256,omitempty"`
	PreviousHostedRelease             *HostedReleaseIdentity `json:"previous_hosted_release,omitempty"`
	ACMMLevel                         int                    `json:"acmm_level"`
	MaxActiveIssues                   int                    `json:"max_active_issues"`
	MaxRepairAttempts                 int                    `json:"max_repair_attempts"`
	VisualHive                        bool                   `json:"visual_hive"`
	VisualHiveRepo                    string                 `json:"visual_hive_repository"`
	VisualHiveRef                     string                 `json:"visual_hive_ref"`
	VisualHiveConfigDigest            string                 `json:"visual_hive_config_digest,omitempty"`
	TestCommands                      [][]string             `json:"test_commands"`
	AllowedRepairPaths                []string               `json:"allowed_repair_paths"`
	AllowedAutoMergePaths             []string               `json:"allowed_auto_merge_paths"`
	AllowedAutoMergeRisk              []automation.RiskTier  `json:"allowed_auto_merge_risk"`
	SetupAuthorizationActorID         int64                  `json:"setup_authorization_actor_id"`
	SetupAuthorizationPreviousActorID int64                  `json:"setup_authorization_previous_actor_id,omitempty"`
}

// VerifyInstalledSetup proves that the exact durable policy and immutable pin
// are already present on the target branch. It is stronger than trusting a
// historical setup PR number, and makes a no-diff rerun recover cleanly when a
// redundant or stale setup PR was closed.
func VerifyInstalledSetup(ctx context.Context, client *hivegithub.Client, config Config) error {
	return verifyInstalledSetupAtRef(ctx, client, config, config.DefaultBranch)
}

// VerifyInstalledSetupAtCommit binds every managed-file read to one immutable
// target commit. Production evidence must pass this check after its hosted run
// and before any durable lifecycle application; a branch-ref preflight alone
// cannot authorize artifacts produced after an out-of-band policy change.
func VerifyInstalledSetupAtCommit(ctx context.Context, client *hivegithub.Client, config Config, commitSHA string) error {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !immutableCommit.MatchString(commitSHA) {
		return fmt.Errorf("exact 40-character target commit is required to verify installed setup")
	}
	return verifyInstalledSetupAtRef(ctx, client, config, commitSHA)
}

func verifyInstalledSetupAtRef(ctx context.Context, client *hivegithub.Client, config Config, ref string) error {
	if client == nil || client.GoGitHub() == nil {
		return fmt.Errorf("GitHub client is required to verify installed setup")
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" || config.DefaultBranch == "" || strings.TrimSpace(ref) == "" {
		return fmt.Errorf("installed repository identity or default branch is incomplete")
	}
	content, _, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, ".hive/integrated.json", &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil || content == nil {
		return fmt.Errorf("read managed setup from target branch: %w", err)
	}
	value, err := content.GetContent()
	if err != nil {
		return fmt.Errorf("decode managed setup from target branch: %w", err)
	}
	var installed installedRepositoryConfig
	if err := json.Unmarshal([]byte(value), &installed); err != nil {
		return fmt.Errorf("parse managed setup from target branch: %w", err)
	}
	expected := installedRepositoryConfig{
		SchemaVersion: managedRepositoryConfigSchema, Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		Coverage: config.Coverage, Automation: config.Automation, Provider: config.Provider, ACMMLevel: config.ACMMLevel,
		ExecutionMode: config.ExecutionMode, RunIntervalSeconds: config.RunIntervalSeconds, HostedSchedule: config.HostedSchedule,
		HostedStateBranch: config.HostedStateBranch, HiveReleaseRepository: config.HiveReleaseRepository,
		HiveReleaseVersion: config.HiveReleaseVersion, HiveCommit: config.HiveCommit,
		DistributionManifestSHA256: config.DistributionManifestSHA256,
		HostedControllerProtocol:   config.HostedControllerProtocol,
		HostedWorkflowSHA256:       config.HostedWorkflowSHA256,
		PreviousHostedRelease:      cloneHostedReleaseIdentity(config.PreviousHostedRelease),
		MaxActiveIssues:            config.MaxActiveIssues, MaxRepairAttempts: config.MaxRepairAttempts, VisualHive: config.VisualHive,
		VisualHiveRepo: config.VisualHiveRepo, VisualHiveRef: config.VisualHiveRef, VisualHiveConfigDigest: config.VisualHiveConfigDigest, TestCommands: config.TestCommands,
		AllowedRepairPaths: config.AllowedRepairPaths, AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
		SetupAuthorizationActorID:         config.SetupAuthorizationActorID,
		SetupAuthorizationPreviousActorID: config.SetupAuthorizationPreviousActorID,
	}
	if !reflect.DeepEqual(installed, expected) {
		return fmt.Errorf("target branch managed setup does not match the durable local policy; merge the current setup/upgrade PR before running Hive")
	}
	if !config.VisualHive {
		return fmt.Errorf("integrated Hive installations require Visual Hive; rerun setup with --visual-hive")
	}
	for relative, expected := range map[string]string{
		".github/workflows/hive-visual-hive.yml": workflow(config),
		".github/workflows/visual-hive-pr.yml":   pullRequestWorkflow(config),
	} {
		actual, readErr := readTargetFile(ctx, client, owner, repo, ref, relative)
		if readErr != nil {
			return fmt.Errorf("verify managed production file %s: %w", relative, readErr)
		}
		if normalizeManagedText(actual) != normalizeManagedText(expected) {
			return fmt.Errorf("managed production file %s does not match the durable immutable pin and policy; rerun setup and merge its exact reviewed PR", relative)
		}
	}
	if config.ExecutionMode == ExecutionHosted {
		if config.HostedControllerProtocol != HostedControllerProtocol || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(config.HostedWorkflowSHA256) {
			return fmt.Errorf("durable hosted controller protocol or workflow digest is invalid")
		}
		expected, workflowErr := GenerateHostedControllerWorkflow(hostedWorkflowConfig(config))
		if workflowErr != nil {
			return fmt.Errorf("generate expected hosted controller workflow: %w", workflowErr)
		}
		actual, readErr := readTargetFile(ctx, client, owner, repo, ref, HostedControllerWorkflowPath)
		if readErr != nil {
			return fmt.Errorf("verify managed hosted controller workflow: %w", readErr)
		}
		expectedDigest := sha256.Sum256([]byte(expected))
		actualDigest := sha256.Sum256([]byte(actual))
		if hex.EncodeToString(expectedDigest[:]) != config.HostedWorkflowSHA256 || hex.EncodeToString(actualDigest[:]) != config.HostedWorkflowSHA256 {
			return fmt.Errorf("managed hosted controller workflow does not match its durable content digest")
		}
		if normalizeManagedText(actual) != normalizeManagedText(expected) {
			return fmt.Errorf("managed hosted controller workflow does not match the durable immutable release and policy; rerun setup and merge its exact reviewed PR")
		}
	}
	visualConfig, readErr := readTargetFile(ctx, client, owner, repo, ref, "visual-hive.config.yaml")
	if readErr != nil {
		return fmt.Errorf("verify managed Visual Hive config: %w", readErr)
	}
	if err := verifyVisualHiveCoverageProfile([]byte(visualConfig), profileForCoverage(config.Coverage)); err != nil {
		return err
	}
	if config.VisualHiveConfigDigest != "" {
		digest := sha256.Sum256([]byte(normalizeManagedText(visualConfig)))
		if !strings.EqualFold(config.VisualHiveConfigDigest, hex.EncodeToString(digest[:])) {
			return fmt.Errorf("managed Visual Hive configuration does not match the exact repository-specific coverage plan; rerun setup and merge its reviewed PR")
		}
	}
	for _, forbidden := range standaloneVisualHiveWriterWorkflowPaths() {
		content, _, response, forbiddenErr := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, forbidden, &gh.RepositoryContentGetOptions{Ref: ref})
		if forbiddenErr == nil || content != nil {
			return fmt.Errorf("standalone Visual Hive lifecycle writer %s must be removed; Hive is the sole lifecycle writer", forbidden)
		}
		if response == nil || response.StatusCode != http.StatusNotFound {
			return fmt.Errorf("verify absence of standalone lifecycle writer %s: %w", forbidden, forbiddenErr)
		}
	}
	return nil
}

func readTargetFile(ctx context.Context, client *hivegithub.Client, owner, repo, branch, relative string) (string, error) {
	content, _, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, relative, &gh.RepositoryContentGetOptions{Ref: branch})
	if err != nil || content == nil {
		return "", fmt.Errorf("read %s from target branch: %w", relative, err)
	}
	value, err := content.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s from target branch: %w", relative, err)
	}
	return value, nil
}

func normalizeManagedText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n")) + "\n"
}

func verifyVisualHiveCoverageProfile(data []byte, expected string) error {
	var value struct {
		Project struct {
			SetupProfile string `yaml:"setupProfile"`
		} `yaml:"project"`
		Integrations struct {
			Hive struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"hive"`
		} `yaml:"integrations"`
	}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("parse managed Visual Hive config: %w", err)
	}
	if value.Project.SetupProfile != expected {
		return fmt.Errorf("Visual Hive setup profile %q does not match coverage depth %q (expected %q); rerun setup and merge its exact reviewed PR", value.Project.SetupProfile, expected, expected)
	}
	if !value.Integrations.Hive.Enabled {
		return fmt.Errorf("managed Visual Hive configuration must set integrations.hive.enabled=true so Hive is the sole lifecycle writer; rerun setup and merge its exact reviewed PR")
	}
	return nil
}

func exactCommitPin(expected, actual string) bool {
	expected, actual = strings.ToLower(strings.TrimSpace(expected)), strings.ToLower(strings.TrimSpace(actual))
	return len(expected) == 40 && expected == actual
}

func priorSetupPullRequestIdentity(prior Config, hasPrior bool) (int, string, error) {
	if !hasPrior {
		return 0, "", nil
	}
	if prior.SetupPRNumber != 0 {
		return prior.SetupPRNumber, prior.SetupHeadSHA, nil
	}
	if prior.SetupPRURL != "" {
		return 0, "", fmt.Errorf("prior setup state has a pull request URL without a pull request number")
	}
	if strings.TrimSpace(prior.SetupHeadSHA) == "" {
		return 0, "", nil
	}
	directBootstrap := prior.DefaultBranch != "" && prior.SetupBranch == prior.DefaultBranch &&
		immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(prior.SetupHeadSHA))) &&
		normalizedExecutionMode(prior.ExecutionMode) == ExecutionLocal && prior.Automation == AutomationRepairPR && prior.VisualHive
	if !directBootstrap {
		return 0, "", fmt.Errorf("prior setup state has a head without a pull request but is not an exact direct-bootstrap installation")
	}
	return 0, "", nil
}

func stagedTreeMatchesRemoteBranch(ctx context.Context, checkout, branch string) (bool, string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return false, "", nil
	}
	desiredTree, err := git(ctx, checkout, "write-tree")
	if err != nil {
		return false, "", fmt.Errorf("write desired setup tree: %w", err)
	}
	remoteRef := "refs/remotes/origin/" + branch
	remoteSHA, err := git(ctx, checkout, "rev-parse", "--verify", remoteRef)
	if err != nil {
		return false, "", nil
	}
	remoteTree, err := git(ctx, checkout, "rev-parse", "--verify", remoteRef+"^{tree}")
	if err != nil {
		return false, "", fmt.Errorf("read existing setup tree: %w", err)
	}
	return strings.TrimSpace(desiredTree) == strings.TrimSpace(remoteTree), strings.TrimSpace(remoteSHA), nil
}

func managedOperationBranch(operation, repositoryID string) string {
	operation = strings.ToLower(strings.TrimSpace(operation))
	repositoryID = regexp.MustCompile(`[^a-zA-Z0-9]+`).ReplaceAllString(strings.TrimSpace(repositoryID), "-")
	repositoryID = strings.Trim(repositoryID, "-")
	return fmt.Sprintf("hive/%s-%s", operation, repositoryID)
}

func managedCommitTrailers(repositoryID, operation string) string {
	return fmt.Sprintf("Hive-Repository-ID: %s\nHive-Operation: %s", strings.TrimSpace(repositoryID), strings.ToLower(strings.TrimSpace(operation)))
}

func pushManagedBranch(ctx context.Context, checkout, repository, branch, repositoryID, operation, localSHA string) error {
	remoteRef := "refs/remotes/origin/" + branch
	remoteSHA, remoteErr := git(ctx, checkout, "rev-parse", "--verify", remoteRef)
	remoteSHA = strings.TrimSpace(remoteSHA)
	lease := "--force-with-lease=refs/heads/" + branch + ":"
	if remoteErr == nil {
		message, err := git(ctx, checkout, "show", "-s", "--format=%B", remoteRef)
		if err != nil {
			return fmt.Errorf("verify existing managed branch %s: %w", branch, err)
		}
		if !hasExactCommitTrailer(message, "Hive-Repository-ID", repositoryID) || !hasExactCommitTrailer(message, "Hive-Operation", operation) {
			return fmt.Errorf("refusing to overwrite remote branch %s because its latest commit lacks exact Hive ownership for repository ID %s and operation %s", branch, repositoryID, operation)
		}
		lease += remoteSHA
	}
	localSHA = strings.TrimSpace(localSHA)
	if localSHA == "" {
		return fmt.Errorf("exact local commit SHA is required before pushing managed branch %s", branch)
	}
	remoteURL := RepositoryCloneURL(repository)
	if _, err := gitTransport(ctx, checkout, remoteURL, "push", lease, remoteURL, localSHA+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("push exact managed branch %s: %w", branch, err)
	}
	return nil
}

func inspectDirectBootstrapTarget(ctx context.Context, client *hivegithub.Client, repository, repositoryID, checkout, defaultBranch string) (directBootstrapProof, error) {
	proof := directBootstrapProof{}
	if client == nil || client.GoGitHub() == nil {
		return proof, fmt.Errorf("GitHub client is required to prove a direct-bootstrap repository")
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(repository), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return proof, fmt.Errorf("direct bootstrap repository identity is invalid")
	}
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, name)
	if err != nil {
		return proof, fmt.Errorf("read direct-bootstrap repository metadata: %w", err)
	}
	liveID := strconv.FormatInt(metadata.GetID(), 10)
	if metadata.GetID() <= 0 || liveID != strings.TrimSpace(repositoryID) || !strings.EqualFold(metadata.GetFullName(), repository) || metadata.GetDefaultBranch() != defaultBranch {
		return proof, fmt.Errorf("direct-bootstrap repository identity/default branch changed (expected %s ID %s branch %s, got %s ID %s branch %s)", repository, repositoryID, defaultBranch, metadata.GetFullName(), liveID, metadata.GetDefaultBranch())
	}
	if metadata.Private == nil || !metadata.GetPrivate() {
		return proof, fmt.Errorf("direct bootstrap is restricted to a repository GitHub proves is private")
	}
	if metadata.GetFork() || metadata.GetArchived() || metadata.GetDisabled() {
		return proof, fmt.Errorf("direct bootstrap requires a non-fork, active disposable repository")
	}
	if !metadata.GetPermissions()["push"] {
		return proof, fmt.Errorf("direct bootstrap requires GitHub to prove push permission on the disposable repository")
	}
	if err := validateDirectBootstrapRemoteBinding(ctx, checkout, repository, defaultBranch); err != nil {
		return proof, err
	}
	baseSHA, err := git(ctx, checkout, "rev-parse", "--verify", "refs/remotes/origin/"+defaultBranch)
	if err != nil {
		return proof, fmt.Errorf("read exact direct-bootstrap base: %w", err)
	}
	baseSHA = strings.ToLower(strings.TrimSpace(baseSHA))
	if !immutableCommit.MatchString(baseSHA) {
		return proof, fmt.Errorf("direct-bootstrap default branch does not resolve to an exact 40-character commit")
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, name, defaultBranch, 0)
	if err != nil {
		return proof, fmt.Errorf("read direct-bootstrap default-branch head: %w", err)
	}
	if branch.GetName() != defaultBranch || !strings.EqualFold(branch.GetCommit().GetSHA(), baseSHA) {
		return proof, fmt.Errorf("direct-bootstrap API head does not match the exact fetched default-branch base")
	}
	countText, err := git(ctx, checkout, "rev-list", "--count", baseSHA)
	if err != nil {
		return proof, fmt.Errorf("count direct-bootstrap seed history: %w", err)
	}
	seedCommits, err := strconv.Atoi(strings.TrimSpace(countText))
	if err != nil || seedCommits != 1 {
		return proof, fmt.Errorf("direct bootstrap requires exactly one seed commit, got %q", strings.TrimSpace(countText))
	}
	managed, err := git(ctx, checkout, "ls-tree", "-r", "--name-only", baseSHA, "--", ".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", ".github/workflows/visual-hive-pr.yml")
	if err != nil {
		return proof, fmt.Errorf("inspect seed for an existing Hive installation: %w", err)
	}
	if strings.TrimSpace(managed) != "" {
		return proof, fmt.Errorf("direct-bootstrap seed already contains managed Hive installation files")
	}
	pulls, response, err := client.GoGitHub().PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil {
		return proof, fmt.Errorf("prove direct-bootstrap repository has no open pull requests: %w", err)
	}
	if len(pulls) != 0 || (response != nil && response.NextPage != 0) {
		return proof, fmt.Errorf("direct bootstrap requires a repository with no open pull requests")
	}
	protection, err := client.BranchProtection(ctx, repository, defaultBranch)
	if err != nil {
		return proof, fmt.Errorf("prove direct-bootstrap default branch has no protection or applicable rules: %w", err)
	}
	if protection.Enabled {
		return proof, fmt.Errorf("direct bootstrap refuses a default branch with existing protection or applicable repository rules")
	}
	return directBootstrapProof{RepositoryID: liveID, DefaultBranch: defaultBranch, BaseSHA: baseSHA, SeedCommits: seedCommits}, nil
}

func validateDirectBootstrapExpectedSeed(options SetupOptions, proof directBootstrapProof) error {
	expected := strings.ToLower(strings.TrimSpace(options.ExpectedSeedSHA))
	if proof.BaseSHA == "" || !strings.EqualFold(proof.BaseSHA, expected) {
		return fmt.Errorf("direct-bootstrap live seed %s does not match the explicitly reviewed seed %s", proof.BaseSHA, expected)
	}
	return nil
}

func validateDirectBootstrapReviewedDigest(options SetupOptions, actual string) error {
	expected := strings.ToLower(strings.TrimSpace(options.ReviewedBaselineDigest))
	actual = strings.ToLower(strings.TrimSpace(actual))
	if !sha256DigestPattern.MatchString(actual) || actual != expected {
		return fmt.Errorf("direct-bootstrap baseline inventory digest %s does not match the explicitly reviewed digest %s", actual, expected)
	}
	return nil
}

func validateDirectBootstrapRemoteBinding(ctx context.Context, checkout, repository, defaultBranch string) error {
	remotes, err := git(ctx, checkout, "remote")
	if err != nil {
		return fmt.Errorf("inspect direct-bootstrap remotes: %w", err)
	}
	if lines := nonEmptyLines(remotes); len(lines) != 1 || lines[0] != "origin" {
		return fmt.Errorf("direct bootstrap requires exactly one canonical origin remote")
	}
	expected := RepositoryCloneURL(repository)
	for _, arguments := range [][]string{{"remote", "get-url", "--all", "origin"}, {"remote", "get-url", "--push", "--all", "origin"}} {
		value, readErr := git(ctx, checkout, arguments...)
		if readErr != nil {
			return fmt.Errorf("inspect direct-bootstrap origin URL: %w", readErr)
		}
		urls := nonEmptyLines(value)
		if len(urls) != 1 || urls[0] != expected {
			return fmt.Errorf("direct bootstrap requires the exact canonical origin URL %q", expected)
		}
	}
	fetch, err := git(ctx, checkout, "config", "--get-all", "remote.origin.fetch")
	if err != nil || strings.TrimSpace(fetch) != "+refs/heads/*:refs/remotes/origin/*" {
		return fmt.Errorf("direct bootstrap requires the canonical origin fetch refspec")
	}
	heads, err := gitTransport(ctx, checkout, expected, "ls-remote", "--heads", expected)
	if err != nil {
		return fmt.Errorf("list direct-bootstrap remote heads: %w", err)
	}
	headLines := nonEmptyLines(heads)
	if len(headLines) != 1 {
		return fmt.Errorf("direct bootstrap requires exactly one remote branch")
	}
	fields := strings.Fields(headLines[0])
	if len(fields) != 2 || fields[1] != "refs/heads/"+defaultBranch || !immutableCommit.MatchString(strings.ToLower(fields[0])) {
		return fmt.Errorf("direct bootstrap remote branch inventory is not the exact default branch")
	}
	tags, err := gitTransport(ctx, checkout, expected, "ls-remote", "--tags", expected)
	if err != nil {
		return fmt.Errorf("list direct-bootstrap remote tags: %w", err)
	}
	if strings.TrimSpace(tags) != "" {
		return fmt.Errorf("direct bootstrap requires a disposable repository with no tags")
	}
	return nil
}

func pushDirectBootstrapDefaultBranch(ctx context.Context, checkout, repository, defaultBranch, baseSHA, headSHA string) error {
	baseSHA, headSHA = strings.ToLower(strings.TrimSpace(baseSHA)), strings.ToLower(strings.TrimSpace(headSHA))
	if !immutableCommit.MatchString(baseSHA) || !immutableCommit.MatchString(headSHA) || baseSHA == headSHA {
		return fmt.Errorf("direct bootstrap requires distinct exact base and head commits")
	}
	if _, err := git(ctx, checkout, "check-ref-format", "refs/heads/"+defaultBranch); err != nil {
		return fmt.Errorf("direct-bootstrap default branch is not a valid ref: %w", err)
	}
	if err := validateDirectBootstrapRemoteBinding(ctx, checkout, repository, defaultBranch); err != nil {
		return err
	}
	remoteURL := RepositoryCloneURL(repository)
	refspec := "+refs/heads/" + defaultBranch + ":refs/remotes/origin/" + defaultBranch
	if _, err := gitTransport(ctx, checkout, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return fmt.Errorf("refresh direct-bootstrap exact base: %w", err)
	}
	remoteSHA, err := git(ctx, checkout, "rev-parse", "--verify", "refs/remotes/origin/"+defaultBranch)
	if err != nil || !strings.EqualFold(strings.TrimSpace(remoteSHA), baseSHA) {
		return fmt.Errorf("direct-bootstrap default branch changed before its exact leased push")
	}
	parents, err := git(ctx, checkout, "rev-list", "--parents", "-n", "1", headSHA)
	if err != nil {
		return fmt.Errorf("inspect direct-bootstrap commit parent: %w", err)
	}
	parentFields := strings.Fields(parents)
	if len(parentFields) != 2 || !strings.EqualFold(parentFields[0], headSHA) || !strings.EqualFold(parentFields[1], baseSHA) {
		return fmt.Errorf("direct-bootstrap commit must be one exact child of the reviewed seed base")
	}
	lease := "--force-with-lease=refs/heads/" + defaultBranch + ":" + baseSHA
	if _, err := gitTransport(ctx, checkout, remoteURL, "push", lease, remoteURL, headSHA+":refs/heads/"+defaultBranch); err != nil {
		return fmt.Errorf("push exact direct-bootstrap default branch: %w", err)
	}
	if _, err := gitTransport(ctx, checkout, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return fmt.Errorf("refresh direct-bootstrap pushed head: %w", err)
	}
	remoteSHA, err = git(ctx, checkout, "rev-parse", "--verify", "refs/remotes/origin/"+defaultBranch)
	if err != nil || !strings.EqualFold(strings.TrimSpace(remoteSHA), headSHA) {
		return fmt.Errorf("direct-bootstrap remote head does not equal the exact pushed commit")
	}
	return nil
}

func finishDirectBootstrap(ctx context.Context, options SetupOptions, store *Store, config Config, result SetupResult, checkout string, initial directBootstrapProof, sha string) (SetupResult, error) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if store == nil || !immutableCommit.MatchString(sha) || initial.BaseSHA == "" || config.SetupBranch != config.DefaultBranch || config.SetupPRNumber != 0 || config.SetupPRURL != "" {
		return result, fmt.Errorf("direct-bootstrap durable identity is incomplete")
	}
	if !options.DirectBootstrap || options.Start || normalizedExecutionMode(config.ExecutionMode) != ExecutionLocal || config.Automation != AutomationRepairPR {
		return result, fmt.Errorf("direct-bootstrap execution boundary changed before publication")
	}
	baselineDigest, err := setupBaselineCandidateDigest(config.SetupBaselineInitialCandidates)
	if err != nil {
		return result, err
	}
	if len(config.SetupBaselineInitialCandidates) == 0 {
		baselineDigest = ""
	}
	if baselineDigest != strings.ToLower(strings.TrimSpace(config.SetupBaselineInitialDigest)) {
		return result, fmt.Errorf("direct-bootstrap reviewed baseline digest changed before publication")
	}
	intent, err := newDirectBootstrapIntent(ctx, options, config, initial, checkout, sha)
	if err != nil {
		return result, err
	}
	if err := store.saveDirectBootstrapIntent(intent); err != nil {
		return result, fmt.Errorf("persist direct-bootstrap intent before remote mutation: %w", err)
	}
	return resumeDirectBootstrap(ctx, options, store, Config{}, false, intent, result)
}

func nonEmptyLines(value string) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func hasExactCommitTrailer(message, key, value string) bool {
	want := strings.TrimSpace(key) + ": " + strings.TrimSpace(value)
	for _, line := range strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		if strings.EqualFold(strings.TrimSpace(line), want) {
			return true
		}
	}
	return false
}

func verifyManagedPullHead(operation string, pull hivegithub.RepairPullRequest, expectedSHA string) error {
	actual, expected := strings.TrimSpace(pull.HeadSHA), strings.TrimSpace(expectedSHA)
	if actual == "" || expected == "" || !strings.EqualFold(actual, expected) {
		return fmt.Errorf("%s PR head %q does not match exact pushed commit %q; refusing to record managed readiness", operation, actual, expected)
	}
	return nil
}

func validateSetupOptions(options SetupOptions) error {
	// Keep the zero value compatible with pre-hosted callers. RunSetup applies
	// the same normalization before validation; the CLI selects hosted
	// explicitly for new setups.
	options.ExecutionMode = normalizedExecutionMode(options.ExecutionMode)
	if options.RunInterval == 0 {
		options.RunInterval = 15 * time.Minute
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(options.Repository) {
		return fmt.Errorf("repository must be owner/name")
	}
	if options.Coverage != CoverageEssential && options.Coverage != CoverageStandard && options.Coverage != CoverageComprehensive && options.Coverage != CoverageCustom {
		return fmt.Errorf("coverage must be essential, standard, comprehensive, or custom")
	}
	if options.Automation != AutomationAdvisory && options.Automation != AutomationIssues && options.Automation != AutomationRepairPR && options.Automation != AutomationAutoMerge {
		return fmt.Errorf("automation must be advisory, issues, repair-pr, or auto-merge")
	}
	if options.StateDir == "" || options.Provider == "" || (options.Apply && options.ProviderCommand == "") {
		return fmt.Errorf("state directory and provider are required")
	}
	if options.ExecutionMode != ExecutionLocal && options.ExecutionMode != ExecutionHosted {
		return fmt.Errorf("execution mode must be local or hosted")
	}
	if options.DirectBootstrap {
		if options.ExecutionMode != ExecutionLocal {
			return fmt.Errorf("direct bootstrap is restricted to local execution")
		}
		if options.Automation != AutomationRepairPR {
			return fmt.Errorf("direct bootstrap is restricted to repair-pr automation")
		}
		if options.Start {
			return fmt.Errorf("direct bootstrap cannot start Hive during the repository mutation")
		}
		if !immutableCommit.MatchString(strings.ToLower(strings.TrimSpace(options.ExpectedSeedSHA))) {
			return fmt.Errorf("direct bootstrap requires the exact expected 40-character seed commit SHA")
		}
		if options.AdoptReviewedBaselines {
			if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(strings.ToLower(strings.TrimSpace(options.ReviewedBaselineDigest))) {
				return fmt.Errorf("reviewed baseline adoption requires the exact reviewed inventory SHA-256 digest")
			}
		} else if strings.TrimSpace(options.ReviewedBaselineDigest) != "" {
			return fmt.Errorf("reviewed baseline digest is valid only with explicit reviewed baseline adoption")
		}
	} else if options.AdoptReviewedBaselines {
		return fmt.Errorf("reviewed baseline adoption is restricted to an explicit direct bootstrap")
	} else if strings.TrimSpace(options.ExpectedSeedSHA) != "" || strings.TrimSpace(options.ReviewedBaselineDigest) != "" {
		return fmt.Errorf("direct-bootstrap seed and baseline bindings require explicit direct bootstrap")
	}
	if options.RunInterval < time.Minute || options.RunInterval > 24*time.Hour {
		return fmt.Errorf("run interval must be from one minute through 24 hours")
	}
	if options.ExecutionMode == ExecutionHosted {
		if options.RunInterval < 5*time.Minute || options.RunInterval%time.Minute != 0 {
			return fmt.Errorf("hosted run interval must be whole minutes and at least five minutes")
		}
		if strings.TrimSpace(options.HostedSchedule) == "" {
			return fmt.Errorf("hosted setup requires a deterministic schedule")
		}
		if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(options.HiveReleaseRepository) ||
			!regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-integrated\.[0-9]+$`).MatchString(options.HiveReleaseVersion) ||
			!immutableCommit.MatchString(strings.ToLower(options.HiveCommit)) ||
			!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(strings.ToLower(options.DistributionManifestSHA256)) {
			return fmt.Errorf("hosted setup requires an immutable integrated release repository, tag, Hive commit, and distribution manifest digest")
		}
		if options.HostedControllerProtocol != HostedControllerProtocol {
			return fmt.Errorf("hosted setup requires controller protocol %d", HostedControllerProtocol)
		}
	}
	if options.MaxActiveIssues < 1 || options.MaxActiveIssues > 100 {
		return fmt.Errorf("maximum active issues must be from 1 through 100")
	}
	if options.MaxRepairAttempts < 1 || options.MaxRepairAttempts > 10 {
		return fmt.Errorf("maximum repair attempts must be from 1 through 10")
	}
	if options.AutoMergePathsExplicit && len(options.AllowedAutoMergePaths) == 0 {
		return fmt.Errorf("an explicit auto-merge path policy requires at least one safe repository-relative glob")
	}
	for _, pattern := range options.AllowedAutoMergePaths {
		cleaned := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
		if cleaned == "" || cleaned != pattern || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "./") || strings.Split(cleaned, "/")[0] == ".." || strings.Contains(cleaned, "/../") {
			return fmt.Errorf("auto-merge path %q must be a normalized repository-relative glob", pattern)
		}
		if _, err := path.Match(cleaned, "hive-policy-validation-probe"); err != nil {
			return fmt.Errorf("auto-merge path %q is not a valid glob: %w", pattern, err)
		}
	}
	if options.AutoMergeRiskExplicit && len(options.AllowedAutoMergeRisk) == 0 {
		return fmt.Errorf("an explicit auto-merge risk policy requires at least one risk tier")
	}
	for _, risk := range options.AllowedAutoMergeRisk {
		if risk < automation.RiskAutomatic || risk > automation.RiskRestricted {
			return fmt.Errorf("auto-merge risk tier %d is invalid", risk)
		}
	}
	if !options.VisualHive {
		return fmt.Errorf("integrated Hive requires Visual Hive deterministic testing; disabling --visual-hive is not supported")
	}
	if options.VisualHive && (options.VisualHiveRepo == "" || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(options.VisualHiveRef)) {
		return fmt.Errorf("Visual Hive setup planning requires a repository and immutable 40-character commit SHA")
	}
	if options.Apply && options.VisualHive && options.VisualHiveCommand == "" {
		return fmt.Errorf("Visual Hive setup apply requires a command")
	}
	return nil
}

func buildSetupPlan(options SetupOptions, inspection RepositoryInspection) SetupPlan {
	warnings := []string{}
	if options.Automation == AutomationAutoMerge && !inspection.BranchProtection {
		warnings = append(warnings, "Default-branch protection was not detected. Hive will create its conservative exact-App-bound protection only after the setup PR is merged and one trusted default-branch Visual Hive run succeeds.")
	}
	if len(inspection.BaselineFiles) == 0 && options.VisualHive {
		warnings = append(warnings, "No reviewed visual baselines were detected; setup must prepare and review baselines before the first production scan.")
	}
	if options.DirectBootstrap {
		warnings = append(warnings, "Direct bootstrap is an explicit disposable-repository exception: apply revalidates a private single-seed repository with no open pull requests or default-branch policy, then advances the default branch under an exact lease without creating a setup pull request.")
	}
	managedFiles := managedSetupFilesForMode(options.VisualHive, options.ExecutionMode)
	return SetupPlan{
		SchemaVersion: PlanSchema, GeneratedAt: time.Now().UTC(), Repository: options.Repository,
		StateDir:      options.StateDir,
		ExecutionMode: options.ExecutionMode, RunIntervalSeconds: int64(options.RunInterval / time.Second),
		HostedSchedule: options.HostedSchedule, HostedStateBranch: hostedStateBranch(inspection.RepositoryID),
		HiveReleaseRepository: options.HiveReleaseRepository, HiveReleaseVersion: options.HiveReleaseVersion,
		HiveCommit: options.HiveCommit, DistributionManifestSHA256: options.DistributionManifestSHA256,
		HostedControllerProtocol: options.HostedControllerProtocol,
		Coverage:                 options.Coverage, Automation: options.Automation, Provider: options.Provider,
		ACMMLevel: acmmForAutomation(options.Automation), VisualHive: options.VisualHive,
		DirectBootstrap: options.DirectBootstrap, AdoptReviewedBaselines: options.AdoptReviewedBaselines,
		ExpectedSeedSHA: strings.ToLower(strings.TrimSpace(options.ExpectedSeedSHA)), ReviewedBaselineDigest: strings.ToLower(strings.TrimSpace(options.ReviewedBaselineDigest)),
		VisualHiveRepository: options.VisualHiveRepo, VisualHiveRef: options.VisualHiveRef, Inspection: inspection,
		MaxActiveIssues:       options.MaxActiveIssues,
		MaxRepairAttempts:     options.MaxRepairAttempts,
		AllowedAutoMergePaths: append([]string(nil), options.AllowedAutoMergePaths...),
		AllowedAutoMergeRisk:  append([]automation.RiskTier(nil), options.AllowedAutoMergeRisk...),
		TestingLayers:         layersForCoverage(options.Coverage),
		FilesToManage:         managedFiles,
		RequiredActions:       setupRequiredActions(options),
		Warnings:              warnings, ReadOnly: true,
	}
}

func setupRequiredActions(options SetupOptions) []string {
	actions := []string{"Review and merge the exact setup PR"}
	if options.DirectBootstrap {
		actions = []string{"Confirm the target is a new private disposable repository and explicitly authorize one exact leased default-branch bootstrap commit"}
	}
	if options.Automation == AutomationAutoMerge {
		actions = append(actions, "Complete the setup PR and trusted setup run to activate exact-App-bound protection before any lifecycle write")
	}
	ownerConfig := Config{ExecutionMode: options.ExecutionMode, VisualHive: options.VisualHive, Automation: options.Automation}
	if UsesNormalHiveRuntime(ownerConfig) {
		actions = append(actions,
			fmt.Sprintf("Configure the existing ordinary Hive/dashboard process with the exact HIVE_STATE_DIR %q and ensure its normal project scope includes %s", options.StateDir, options.Repository),
			"Confirm the existing dashboard listener is HTTP-ready, then restart ordinary Hive/dashboard; do not use hive run or hive start for this runtime, and use hive stop only when status or doctor directs stale legacy-scheduler cleanup",
		)
	} else if options.ExecutionMode == ExecutionHosted {
		actions = append(actions, "Confirm the hosted controller completes with signed durable state; the bootstrap computer may then be removed")
	} else {
		actions = append(actions, "Start or resume the legacy local scheduler only after the exact managed setup is installed and doctor prerequisites are green")
	}
	return append(actions, "Run hive doctor and confirm production_ready=true")
}

func managedSetupFiles(visualHive bool) []string {
	return managedSetupFilesForMode(visualHive, ExecutionLocal)
}

func managedSetupFilesForMode(visualHive bool, mode ExecutionMode) []string {
	files := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	if mode == ExecutionHosted {
		files = append(files, HostedControllerWorkflowPath)
	}
	if visualHive {
		files = append(files, "docs/visual-hive.md", "visual-hive.config.yaml", ".github/workflows/visual-hive-pr.yml")
		files = append(files, standaloneVisualHiveWriterWorkflowPaths()...)
	}
	return files
}

func managedSetupFilesForConfig(config Config) []string {
	return managedSetupFilesForMode(config.VisualHive, normalizedExecutionMode(config.ExecutionMode))
}

func normalizedExecutionMode(mode ExecutionMode) ExecutionMode {
	if mode == "" {
		return ExecutionLocal
	}
	return mode
}

func hostedStateBranch(repositoryID string) string {
	cleaned := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(strings.TrimSpace(repositoryID), "-")
	cleaned = strings.Trim(cleaned, "-")
	if len(cleaned) > 48 {
		cleaned = cleaned[:48]
	}
	if cleaned == "" {
		return ""
	}
	return "hive/state-" + strings.ToLower(cleaned)
}

func hostedWorkflowConfig(config Config) HostedWorkflowConfig {
	return HostedWorkflowConfig{
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		ScheduleCron: config.HostedSchedule, StateBranch: config.HostedStateBranch, StateKeySecret: "HIVE_HOSTED_STATE_KEY",
		ReleaseRepository: config.HiveReleaseRepository, ReleaseTag: config.HiveReleaseVersion,
		HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
		DistributionManifestSHA256: config.DistributionManifestSHA256,
		ProviderRequired:           config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge,
		PreviousRelease:            cloneHostedReleaseIdentity(config.PreviousHostedRelease),
	}
}

func hostedReleaseIdentity(config Config) HostedReleaseIdentity {
	return HostedReleaseIdentity{
		Version: config.HiveReleaseVersion, HiveCommit: strings.ToLower(config.HiveCommit),
		VisualHiveCommit: strings.ToLower(config.VisualHiveRef), DistributionManifestSHA256: strings.ToLower(config.DistributionManifestSHA256),
		HostedControllerProtocol: config.HostedControllerProtocol,
	}
}

func cloneHostedReleaseIdentity(identity *HostedReleaseIdentity) *HostedReleaseIdentity {
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

func equalHostedReleaseIdentity(left, right HostedReleaseIdentity) bool {
	return left.Version == right.Version && strings.EqualFold(left.HiveCommit, right.HiveCommit) &&
		strings.EqualFold(left.VisualHiveCommit, right.VisualHiveCommit) &&
		strings.EqualFold(left.DistributionManifestSHA256, right.DistributionManifestSHA256) && left.HostedControllerProtocol == right.HostedControllerProtocol
}

func standaloneVisualHiveWriterWorkflowPaths() []string {
	return []string{
		".github/workflows/visual-hive-lifecycle.yml",
		".github/workflows/visual-hive-issue-lifecycle.yml",
		".github/workflows/visual-hive-trusted-publisher.yml",
		".github/workflows/visual-hive-failure-issue.yml",
		".github/workflows/visual-hive-hive-handoff.yml",
	}
}

func captureManagedPathPreimages(ctx context.Context, checkout string, managed []string) (map[string]ManagedPathPreimage, error) {
	preimages := make(map[string]ManagedPathPreimage, len(managed))
	total := 0
	for _, relative := range managed {
		candidate := filepath.Join(checkout, filepath.FromSlash(relative))
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			preimages[relative] = ManagedPathPreimage{Existed: false}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", relative, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("managed path %s is not an ordinary file", relative)
		}
		stage, err := git(ctx, checkout, "ls-files", "--stage", "--", relative)
		if err != nil {
			return nil, fmt.Errorf("read Git mode for %s: %w", relative, err)
		}
		fields := strings.Fields(strings.TrimSpace(stage))
		if len(fields) < 4 || (fields[0] != "100644" && fields[0] != "100755") {
			return nil, fmt.Errorf("managed path %s must be one tracked ordinary Git blob before setup", relative)
		}
		blob, err := git(ctx, checkout, "cat-file", "blob", ":"+relative)
		if err != nil {
			return nil, fmt.Errorf("read repository blob for %s: %w", relative, err)
		}
		data := []byte(blob)
		if len(data) > maxManagedPreimageFile || total+len(data) > maxManagedPreimageBytes {
			return nil, fmt.Errorf("managed path preimage exceeds the bounded %d-byte per-file or %d-byte total ledger", maxManagedPreimageFile, maxManagedPreimageBytes)
		}
		digest := sha256.Sum256(data)
		preimages[relative] = ManagedPathPreimage{Existed: true, GitMode: fields[0], Content: append([]byte(nil), data...), ContentSHA256: hex.EncodeToString(digest[:])}
		total += len(data)
	}
	if err := validateManagedPathPreimages(managedPreimagesVersion, preimages, managed); err != nil {
		return nil, err
	}
	return preimages, nil
}

// resolveManagedPathPreimages never treats bytes already written by an older
// Hive installation as repository-owned. New installations capture every
// managed path before the first write. A legacy core-only installation that is
// enabling Visual Hive captures only the newly entering Visual Hive paths; its
// already-managed core paths retain legacy deletion ownership. If an older
// Visual Hive installation has no ledger, provenance cannot be reconstructed,
// so its legacy deletion policy is retained instead of manufacturing one.
func resolveManagedPathPreimages(ctx context.Context, checkout string, prior Config, hasPrior, visualHive bool, mode ExecutionMode) (string, map[string]ManagedPathPreimage, bool, error) {
	managed := managedSetupFilesForMode(visualHive, mode)
	if !hasPrior {
		preimages, err := captureManagedPathPreimages(ctx, checkout, managed)
		return managedPreimagesVersion, preimages, err == nil, err
	}

	if managedPathPreimagesConfigured(prior) {
		priorManaged := managedSetupFilesForConfig(prior)
		if err := validateManagedPathPreimages(prior.ManagedPreimagesVersion, prior.ManagedPathPreimages, priorManaged); err != nil {
			return "", nil, false, fmt.Errorf("existing managed-path preimage ledger is invalid: %w", err)
		}
		preimages := cloneManagedPathPreimages(prior.ManagedPathPreimages)
		if prior.VisualHive == visualHive {
			return prior.ManagedPreimagesVersion, preimages, false, nil
		}
		entering := managedPathDifference(managed, priorManaged)
		captured, err := captureManagedPathPreimages(ctx, checkout, entering)
		if err != nil {
			return "", nil, false, err
		}
		for relative, preimage := range captured {
			preimages[relative] = preimage
		}
		if err := validateManagedPathPreimages(managedPreimagesVersion, preimages, managed); err != nil {
			return "", nil, false, err
		}
		return managedPreimagesVersion, preimages, true, nil
	}

	if prior.VisualHive || !visualHive {
		// A local-to-hosted transition may add the controller even when Visual
		// Hive was already installed. Capture only that newly managed path.
		priorManaged := managedSetupFilesForConfig(prior)
		entering := managedPathDifference(managed, priorManaged)
		if len(entering) == 0 {
			return "", nil, false, nil
		}
		preimages := make(map[string]ManagedPathPreimage, len(managed))
		for _, relative := range priorManaged {
			preimages[relative] = ManagedPathPreimage{Existed: false}
		}
		captured, err := captureManagedPathPreimages(ctx, checkout, entering)
		if err != nil {
			return "", nil, false, err
		}
		for relative, preimage := range captured {
			preimages[relative] = preimage
		}
		if err := validateManagedPathPreimages(managedPreimagesVersion, preimages, managed); err != nil {
			return "", nil, false, err
		}
		return managedPreimagesVersion, preimages, true, nil
	}

	// Legacy Hive owned the core paths, but the Visual Hive paths are entering
	// management for the first time and may belong to a standalone installation.
	preimages := make(map[string]ManagedPathPreimage, len(managed))
	for _, relative := range managedSetupFiles(false) {
		preimages[relative] = ManagedPathPreimage{Existed: false}
	}
	entering := managedPathDifference(managed, managedSetupFiles(false))
	captured, err := captureManagedPathPreimages(ctx, checkout, entering)
	if err != nil {
		return "", nil, false, err
	}
	for relative, preimage := range captured {
		preimages[relative] = preimage
	}
	if err := validateManagedPathPreimages(managedPreimagesVersion, preimages, managed); err != nil {
		return "", nil, false, err
	}
	return managedPreimagesVersion, preimages, true, nil
}

func managedPathDifference(all, alreadyManaged []string) []string {
	seen := make(map[string]struct{}, len(alreadyManaged))
	for _, relative := range alreadyManaged {
		seen[relative] = struct{}{}
	}
	result := make([]string, 0, len(all))
	for _, relative := range all {
		if _, exists := seen[relative]; !exists {
			result = append(result, relative)
		}
	}
	return result
}

func validateManagedPathPreimages(version string, preimages map[string]ManagedPathPreimage, managed []string) error {
	if err := validateManagedPathOwnership(version, preimages, managed); err != nil {
		return err
	}
	total := 0
	for _, relative := range managed {
		preimage := preimages[relative]
		if !preimage.Existed {
			if len(preimage.Content) != 0 {
				return fmt.Errorf("absent managed path preimage %s carries file data", relative)
			}
			continue
		}
		if len(preimage.Content) > maxManagedPreimageFile || total+len(preimage.Content) > maxManagedPreimageBytes {
			return fmt.Errorf("managed path preimage ledger exceeds its bounded size")
		}
		digest := sha256.Sum256(preimage.Content)
		if !strings.EqualFold(preimage.ContentSHA256, hex.EncodeToString(digest[:])) {
			return fmt.Errorf("managed path preimage %s content digest does not match", relative)
		}
		total += len(preimage.Content)
	}
	return nil
}

func validateManagedPathOwnership(version string, preimages map[string]ManagedPathPreimage, managed []string) error {
	if version != managedPreimagesVersion || len(preimages) != len(managed) {
		return fmt.Errorf("managed path preimage ledger is incomplete")
	}
	for _, relative := range managed {
		preimage, exists := preimages[relative]
		if !exists {
			return fmt.Errorf("managed path preimage ledger omits %s", relative)
		}
		if !preimage.Existed {
			if preimage.GitMode != "" || preimage.ContentSHA256 != "" {
				return fmt.Errorf("absent managed path preimage %s carries file metadata", relative)
			}
			continue
		}
		if preimage.GitMode != "100644" && preimage.GitMode != "100755" {
			return fmt.Errorf("managed path preimage %s has unsupported Git mode", relative)
		}
		decoded, err := hex.DecodeString(preimage.ContentSHA256)
		if err != nil || len(decoded) != sha256.Size || preimage.ContentSHA256 != strings.ToLower(preimage.ContentSHA256) {
			return fmt.Errorf("managed path preimage %s has an invalid content digest", relative)
		}
	}
	return nil
}

func cloneManagedPathPreimages(source map[string]ManagedPathPreimage) map[string]ManagedPathPreimage {
	if source == nil {
		return nil
	}
	result := make(map[string]ManagedPathPreimage, len(source))
	for relative, preimage := range source {
		preimage.Content = append([]byte(nil), preimage.Content...)
		result[relative] = preimage
	}
	return result
}

func hasValidManagedPathPreimages(config Config) bool {
	return validateManagedPathPreimages(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managedSetupFilesForConfig(config)) == nil
}

func managedPathPreimagesConfigured(config Config) bool {
	return config.ManagedPreimagesVersion != "" || config.ManagedPathPreimages != nil
}

func restoreManagedPathPreimages(root string, config Config) error {
	managed := managedSetupFilesForConfig(config)
	if err := validateManagedPathPreimages(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managed); err != nil {
		return err
	}
	for _, relative := range managed {
		preimage := config.ManagedPathPreimages[relative]
		if !preimage.Existed {
			if err := removeSetupCheckoutFile(root, relative); err != nil {
				return fmt.Errorf("remove Hive-owned managed path %s: %w", relative, err)
			}
			continue
		}
		if err := writeSetupCheckoutFile(root, relative, preimage.Content); err != nil {
			return fmt.Errorf("restore repository-owned managed path %s: %w", relative, err)
		}
	}
	return nil
}

func managedUninstallRequiredPaths(config Config) (present, absent []string) {
	managed := managedSetupFilesForConfig(config)
	if validateManagedPathOwnership(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managed) != nil {
		if managedPathPreimagesConfigured(config) {
			return nil, nil
		}
		return []string{}, append([]string(nil), managed...)
	}
	for _, relative := range managed {
		if config.ManagedPathPreimages[relative].Existed {
			present = append(present, relative)
		} else {
			absent = append(absent, relative)
		}
	}
	return present, absent
}

func applyManagedPathPreimageModes(ctx context.Context, checkout string, config Config) error {
	if !managedPathPreimagesConfigured(config) {
		return nil
	}
	if err := validateManagedPathPreimages(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managedSetupFilesForConfig(config)); err != nil {
		return err
	}
	for _, relative := range managedSetupFilesForConfig(config) {
		preimage := config.ManagedPathPreimages[relative]
		if !preimage.Existed {
			continue
		}
		flag := "--chmod=-x"
		if preimage.GitMode == "100755" {
			flag = "--chmod=+x"
		}
		if _, err := git(ctx, checkout, "update-index", flag, "--", relative); err != nil {
			return fmt.Errorf("restore Git mode for repository-owned managed path %s: %w", relative, err)
		}
	}
	return nil
}

func managedPathPolicyDigest(config Config) (string, error) {
	if !managedPathPreimagesConfigured(config) {
		return digestPaths(managedSetupFilesForConfig(config))
	}
	if err := validateManagedPathPreimages(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managedSetupFilesForConfig(config)); err != nil {
		return "", err
	}
	payload := struct {
		Version   string                         `json:"version"`
		Paths     []string                       `json:"paths"`
		Preimages map[string]ManagedPathPreimage `json:"preimages"`
	}{Version: config.ManagedPreimagesVersion, Paths: managedSetupFilesForConfig(config), Preimages: config.ManagedPathPreimages}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode managed path restoration policy: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func layersForCoverage(coverage Coverage) []string {
	essential := []string{"unit", "integration", "e2e", "visual", "accessibility"}
	if coverage == CoverageEssential {
		return essential
	}
	standard := append(essential, "api", "mutation", "security")
	if coverage == CoverageStandard {
		return standard
	}
	return append(standard, "performance", "dependency", "cross-browser", "responsive", "theme-state")
}

func acmmForAutomation(value Automation) int {
	switch value {
	case AutomationAdvisory:
		return 2
	case AutomationIssues:
		return 4
	case AutomationRepairPR:
		return 5
	case AutomationAutoMerge:
		return 6
	default:
		return 1
	}
}

func ensureCheckout(ctx context.Context, repository, checkout string) (string, error) {
	return ensureCheckoutWithLegacy(ctx, repository, checkout, nil)
}

func ensureCheckoutWithLegacy(ctx context.Context, repository, checkout string, legacy *legacyManagedCheckoutProof) (string, error) {
	exists, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, repository, legacy)
	if err != nil {
		return "", err
	}
	if !exists {
		if err := os.MkdirAll(filepath.Dir(checkout), 0o700); err != nil {
			return "", err
		}
		remoteURL := RepositoryCloneURL(repository)
		if _, err := gitTransport(ctx, filepath.Dir(checkout), remoteURL, "clone", "--origin", "origin", remoteURL, checkout); err != nil {
			return "", fmt.Errorf("clone target repository: %w", err)
		}
		if err := writeManagedCheckoutOwner(checkout, repository); err != nil {
			return "", fmt.Errorf("bind managed checkout ownership: %w", err)
		}
	}
	if _, err := validateManagedCheckoutBeforeGitWithLegacy(checkout, repository, legacy); err != nil {
		return "", fmt.Errorf("validate exact managed checkout ownership before Git synchronization: %w", err)
	}
	remoteURL := RepositoryCloneURL(repository)
	if _, err := gitTransport(ctx, checkout, remoteURL, "fetch", "--prune", "--no-recurse-submodules", remoteURL, "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}
	ref, err := git(ctx, checkout, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if branch := strings.TrimPrefix(strings.TrimSpace(ref), "origin/"); branch != "" {
			return checkoutDefaultBranch(ctx, checkout, branch)
		}
	}
	for _, branch := range []string{"main", "master"} {
		if _, err := git(ctx, checkout, "rev-parse", "--verify", "origin/"+branch); err == nil {
			return checkoutDefaultBranch(ctx, checkout, branch)
		}
	}
	return "", fmt.Errorf("could not determine default branch")
}

func checkoutDefaultBranch(ctx context.Context, checkout, branch string) (string, error) {
	// The checkout is Hive-owned. Always inspect the current remote default
	// rather than whatever setup/repair branch a previous interrupted run left
	// checked out.
	if _, err := git(ctx, checkout, "reset", "--hard", "origin/"+branch); err != nil {
		return "", fmt.Errorf("reset exact managed checkout to origin/%s: %w", branch, err)
	}
	if _, err := git(ctx, checkout, "clean", "-ffdqx"); err != nil {
		return "", fmt.Errorf("clean exact managed checkout before static setup: %w", err)
	}
	if _, err := git(ctx, checkout, "switch", "--detach", "origin/"+branch); err != nil {
		return "", fmt.Errorf("switch managed checkout to origin/%s: %w", branch, err)
	}
	return branch, nil
}

func runVisualHiveSetup(ctx context.Context, options SetupOptions, checkout string) error {
	setupEnvironment, err := visualHiveSetupEnvironment(options)
	if err != nil {
		return err
	}
	args := append([]string(nil), options.VisualHiveArgs...)
	args = append(args, "recommend", "--repo", checkout, "--profile", profileForCoverage(options.Coverage), "--format", "json")
	configExisted := exists(filepath.Join(checkout, "visual-hive.config.yaml"))
	if !configExisted {
		args = append(args, "--write-config")
	}
	if !exists(filepath.Join(checkout, "docs", "visual-hive.md")) {
		args = append(args, "--write-docs")
	}
	command := exec.CommandContext(ctx, options.VisualHiveCommand, args...)
	command.Dir = checkout
	command.Env = setupEnvironment
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("generate repository-specific Visual Hive setup: %w: %s", err, safeOutput(output.String()))
	}
	if configExisted {
		if err := mergeVisualHiveRecommendation(checkout, profileForCoverage(options.Coverage)); err != nil {
			return fmt.Errorf("migrate existing Visual Hive coverage profile: %w", err)
		}
	} else if err := markVisualHiveConfigOrigin(checkout, "generated", profileForCoverage(options.Coverage)); err != nil {
		return fmt.Errorf("mark generated Visual Hive coverage ownership: %w", err)
	}
	doctorArgs := append([]string(nil), options.VisualHiveArgs...)
	doctorArgs = append(doctorArgs, "doctor", "--config", filepath.Join(checkout, "visual-hive.config.yaml"))
	doctor := exec.CommandContext(ctx, options.VisualHiveCommand, doctorArgs...)
	doctor.Dir, doctor.Env = checkout, setupEnvironment
	output.Reset()
	doctor.Stdout, doctor.Stderr = &output, &output
	if err := doctor.Run(); err != nil {
		return fmt.Errorf("validate generated Visual Hive configuration: %w: %s", err, safeOutput(output.String()))
	}
	// Local setup is a static-only trust zone. Repository install/build/serve,
	// browser installation, screenshot capture, and every target-authored script
	// execute only in the post-merge hosted baseline/production workflows.
	return nil
}

func visualHiveSetupEnvironment(options SetupOptions) ([]string, error) {
	runtimeDir, err := filepath.Abs(filepath.Dir(strings.TrimSpace(options.VisualHiveCommand)))
	if err != nil || strings.TrimSpace(options.VisualHiveCommand) == "" {
		return nil, fmt.Errorf("Visual Hive setup requires an exact runtime command")
	}
	stateDir := strings.TrimSpace(options.StateDir)
	if stateDir == "" {
		return nil, fmt.Errorf("Visual Hive setup requires a persistent state directory for runtime caches")
	}
	corepackHome, err := filepath.Abs(filepath.Join(stateDir, "runtime", "corepack"))
	if err != nil {
		return nil, fmt.Errorf("resolve corepack runtime cache: %w", err)
	}
	if err := os.MkdirAll(corepackHome, 0o700); err != nil {
		return nil, fmt.Errorf("create corepack runtime cache: %w", err)
	}
	environment := safeEnvironment()
	environment = replaceEnvironmentValue(environment, "PATH", runtimeDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	environment = replaceEnvironmentValue(environment, "COREPACK_HOME", corepackHome)
	return environment, nil
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, pair := range environment {
		key, _, _ := strings.Cut(pair, "=")
		if !strings.EqualFold(key, name) {
			result = append(result, pair)
		}
	}
	return append(result, name+"="+value)
}

func visualHiveConfigRequiresBaselines(checkout string) (bool, error) {
	required, _, err := visualHiveScreenshotContractInventory(checkout)
	return required, err
}

func visualHiveScreenshotContractInventory(checkout string) (bool, string, error) {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return false, "", fmt.Errorf("read generated Visual Hive config before baseline bootstrap: %w", err)
	}
	var config struct {
		Contracts []struct {
			ID          any   `yaml:"id"`
			Name        any   `yaml:"name"`
			Path        any   `yaml:"path"`
			Screenshots []any `yaml:"screenshots"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return false, "", fmt.Errorf("parse generated Visual Hive config before baseline bootstrap: %w", err)
	}
	type contractRecord struct {
		Index       int   `json:"index"`
		ID          any   `json:"id,omitempty"`
		Name        any   `json:"name,omitempty"`
		Path        any   `json:"path,omitempty"`
		Screenshots []any `json:"screenshots"`
	}
	records := []contractRecord{}
	for index, contract := range config.Contracts {
		if len(contract.Screenshots) > 0 {
			records = append(records, contractRecord{Index: index, ID: contract.ID, Name: contract.Name, Path: contract.Path, Screenshots: contract.Screenshots})
		}
	}
	if len(records) == 0 {
		return false, "", nil
	}
	canonical, err := json.Marshal(records)
	if err != nil {
		return false, "", fmt.Errorf("bind screenshot contract inventory: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return true, hex.EncodeToString(digest[:]), nil
}

// mergeVisualHiveRecommendation reconciles every coverage-owned section to the
// deterministic recommendation. This makes upgrades and downgrades exact; the
// resulting setup PR remains the review boundary for repository-specific edits.
func mergeVisualHiveRecommendation(checkout, expectedProfile string) error {
	existingData, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return err
	}
	var current map[string]any
	if err := yaml.Unmarshal(existingData, &current); err != nil {
		return fmt.Errorf("parse existing config: %w", err)
	}
	project := ensureStringMap(current, "project")
	enableVisualHiveIntegration(current)
	origin := ""
	if rawOrigin, exists := project["hiveConfigOrigin"]; exists && rawOrigin != nil {
		origin = strings.ToLower(strings.TrimSpace(fmt.Sprint(rawOrigin)))
	}
	// Legacy repository-owned plans predate Hive's origin marker. When their
	// effective coverage profile is unchanged, preserve all repository-owned
	// semantics while adding only the explicit Hive integration ownership bit.
	if origin == "" && strings.EqualFold(strings.TrimSpace(fmt.Sprint(project["setupProfile"])), strings.TrimSpace(expectedProfile)) {
		updated, err := yaml.Marshal(current)
		if err != nil {
			return err
		}
		if bytes.Equal(existingData, updated) {
			return nil
		}
		return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
	}
	var report struct {
		RecommendedConfig map[string]any `json:"recommendedConfig"`
	}
	recommendationData, err := readSetupCheckoutFile(checkout, ".visual-hive/recommendations.json")
	if err != nil {
		return fmt.Errorf("read deterministic recommendation: %w", err)
	}
	if err := json.Unmarshal(recommendationData, &report); err != nil || len(report.RecommendedConfig) == 0 {
		return fmt.Errorf("parse deterministic recommendation: %w", err)
	}
	project["setupProfile"] = expectedProfile
	project["hiveManagedBy"] = "hive-integrated"
	if origin == "" || origin == "existing" {
		// A pre-existing Visual Hive plan belongs to the repository. Preserve its
		// domain-specific targets/contracts and record that generic profile
		// recommendations must not replace them on future coverage changes.
		project["hiveConfigOrigin"] = "existing"
		updated, err := yaml.Marshal(current)
		if err != nil {
			return err
		}
		if bytes.Equal(existingData, updated) {
			return nil
		}
		return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
	}
	project["hiveConfigOrigin"] = "generated"
	for _, key := range []string{"targets", "contracts", "viewports", "visual", "selection", "mutation", "flows"} {
		if value, exists := report.RecommendedConfig[key]; exists {
			current[key] = value
		} else {
			delete(current, key)
		}
	}
	// Coverage-owned recommendations may replace the visual section wholesale.
	// Reapply the integration-owned invariants after that replacement so a
	// merged setup remains byte-for-byte idempotent on the next setup run.
	enableVisualHiveIntegration(current)
	if recommendedCost, exists := report.RecommendedConfig["costPolicy"]; exists {
		current["costPolicy"] = recommendedCost
	}
	updated, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	if bytes.Equal(existingData, updated) {
		return nil
	}
	return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
}

func markVisualHiveConfigOrigin(checkout, origin, profile string) error {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return err
	}
	var current map[string]any
	if err := yaml.Unmarshal(data, &current); err != nil {
		return err
	}
	project := ensureStringMap(current, "project")
	enableVisualHiveIntegration(current)
	project["setupProfile"] = profile
	project["hiveManagedBy"] = "hive-integrated"
	project["hiveConfigOrigin"] = origin
	updated, err := yaml.Marshal(current)
	if err != nil {
		return err
	}
	return writeSetupCheckoutFile(checkout, "visual-hive.config.yaml", updated)
}

func ensureStringMap(parent map[string]any, key string) map[string]any {
	if value := anyStringMap(parent[key]); value != nil {
		parent[key] = value
		return value
	}
	value := map[string]any{}
	parent[key] = value
	return value
}

func enableVisualHiveIntegration(config map[string]any) {
	integrations := ensureStringMap(config, "integrations")
	hive := ensureStringMap(integrations, "hive")
	hive["enabled"] = true
	// Hive's accountable baseline lifecycle accepts only this canonical,
	// repository-local namespace. Preserve the repository's visual policy,
	// but route every integrated baseline candidate through the path that the
	// hosted verifier, approval plan, and merge authority bind exactly.
	visual := ensureStringMap(config, "visual")
	visual["snapshotDir"] = ".visual-hive/snapshots"
}

func anyStringMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func anySlice(value any) []any {
	if result, ok := value.([]any); ok {
		return result
	}
	return nil
}

func mergeMissingMap(current, recommended map[string]any) {
	for key, value := range recommended {
		if _, exists := current[key]; !exists {
			current[key] = value
		}
	}
}

func mergeObjectListByID(current, recommended []any) []any {
	return mergeObjectListByKey(current, recommended, "id")
}

func mergeObjectListByKey(current, recommended []any, key string) []any {
	seen := map[string]bool{}
	for _, item := range current {
		if value := fmt.Sprint(anyStringMap(item)[key]); value != "" {
			seen[value] = true
		}
	}
	for _, item := range recommended {
		value := fmt.Sprint(anyStringMap(item)[key])
		if value != "" && !seen[value] {
			current = append(current, item)
			seen[value] = true
		}
	}
	return current
}

func mergeScalarList(current, recommended []any) []any {
	seen := map[string]bool{}
	for _, item := range current {
		seen[fmt.Sprint(item)] = true
	}
	for _, item := range recommended {
		value := fmt.Sprint(item)
		if !seen[value] {
			current = append(current, item)
			seen[value] = true
		}
	}
	return current
}

func profileForCoverage(coverage Coverage) string {
	if coverage == CoverageComprehensive || coverage == CoverageCustom {
		return "complex-app"
	}
	return "free-local"
}

func visualHiveConfigDigest(checkout string) (string, error) {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return "", fmt.Errorf("read repository-specific Visual Hive config for digest: %w", err)
	}
	digest := sha256.Sum256([]byte(normalizeManagedText(string(data))))
	return hex.EncodeToString(digest[:]), nil
}

func existingVisualBaselines(checkout string) []string {
	committed, ok := committedFileSet(checkout)
	if !ok {
		return nil
	}
	files := []string{}
	for path := range committed {
		clean := filepath.ToSlash(path)
		if strings.HasPrefix(strings.ToLower(clean), ".visual-hive/snapshots/") && strings.EqualFold(filepath.Ext(clean), ".png") {
			files = append(files, clean)
		}
	}
	return sortedUnique(files)
}

func validateDirectBootstrapReviewedBaselines(ctx context.Context, checkout, seedSHA string) ([]SetupBaselineCandidate, string, error) {
	expected, err := directBootstrapExpectedBaselinePaths(checkout)
	if err != nil {
		return nil, "", err
	}
	if len(expected) == 0 {
		return nil, "", fmt.Errorf("direct-bootstrap baseline adoption requires at least one screenshot contract")
	}
	candidates, digest, err := directBootstrapBaselineInventoryAtCommit(ctx, checkout, seedSHA)
	if err != nil {
		return nil, "", err
	}
	if len(candidates) != len(expected) {
		return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline inventory has %d PNGs, but the screenshot contracts require exactly %d", len(candidates), len(expected))
	}
	for _, candidate := range candidates {
		if !expected[candidate.Path] {
			return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline %s does not match an exact configured screenshot contract", candidate.Path)
		}
	}
	return candidates, digest, nil
}

func directBootstrapExpectedBaselinePaths(checkout string) (map[string]bool, error) {
	data, err := readSetupCheckoutFile(checkout, "visual-hive.config.yaml")
	if err != nil {
		return nil, fmt.Errorf("read Visual Hive configuration for direct-bootstrap baseline adoption: %w", err)
	}
	var config struct {
		Visual struct {
			SnapshotDir      string `yaml:"snapshotDir"`
			BaselinePlatform string `yaml:"baselinePlatform"`
		} `yaml:"visual"`
		Contracts []struct {
			ID          string `yaml:"id"`
			Screenshots []struct {
				Name     string `yaml:"name"`
				Viewport string `yaml:"viewport"`
			} `yaml:"screenshots"`
		} `yaml:"contracts"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse Visual Hive configuration for direct-bootstrap baseline adoption: %w", err)
	}
	snapshotDir := config.Visual.SnapshotDir
	if snapshotDir == "" {
		snapshotDir = ".visual-hive/snapshots"
	}
	if strings.Contains(snapshotDir, "\\") || path.Clean(snapshotDir) != snapshotDir || snapshotDir != ".visual-hive/snapshots" {
		return nil, fmt.Errorf("direct-bootstrap baseline adoption requires the canonical .visual-hive/snapshots directory")
	}
	platform := config.Visual.BaselinePlatform
	if platform == "" {
		platform = "shared"
	}
	if platform != "shared" && platform != "platform" {
		return nil, fmt.Errorf("direct-bootstrap baseline adoption requires shared or platform baseline identity")
	}
	if platform == "platform" {
		snapshotDir = path.Join(snapshotDir, "linux")
	}
	expected := map[string]bool{}
	for _, contract := range config.Contracts {
		for _, screenshot := range contract.Screenshots {
			contractID := directBootstrapSafeName.ReplaceAllString(contract.ID, "-")
			name := directBootstrapSafeName.ReplaceAllString(screenshot.Name, "-")
			viewport := directBootstrapSafeName.ReplaceAllString(screenshot.Viewport, "-")
			if contract.ID == "" || screenshot.Name == "" || screenshot.Viewport == "" || contractID == "" || name == "" || viewport == "" {
				return nil, fmt.Errorf("direct-bootstrap screenshot baseline identity requires non-empty contract, name, and viewport values")
			}
			relative := path.Join(snapshotDir, contractID+"__"+name+"__"+viewport+".png")
			if len(relative) > directBootstrapMaxPathBytes || expected[relative] {
				return nil, fmt.Errorf("direct-bootstrap screenshot contracts produce a duplicate or oversized baseline path %q", relative)
			}
			expected[relative] = true
			if len(expected) > directBootstrapMaxBaselines {
				return nil, fmt.Errorf("direct-bootstrap screenshot contract inventory exceeds %d baselines", directBootstrapMaxBaselines)
			}
		}
	}
	return expected, nil
}

func directBootstrapBaselineInventoryAtCommit(ctx context.Context, checkout, commitSHA string) ([]SetupBaselineCandidate, string, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !immutableCommit.MatchString(commitSHA) {
		return nil, "", fmt.Errorf("exact seed commit is required for direct-bootstrap baseline adoption")
	}
	output, err := git(ctx, checkout, "ls-tree", "-r", "-z", commitSHA, "--", ".visual-hive/snapshots")
	if err != nil {
		return nil, "", fmt.Errorf("list direct-bootstrap reviewed baseline tree: %w", err)
	}
	candidates := []SetupBaselineCandidate{}
	var totalBytes int64
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		if len(candidates) >= directBootstrapMaxBaselines {
			return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline inventory exceeds %d PNGs", directBootstrapMaxBaselines)
		}
		metadata, relative, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		relative = filepath.ToSlash(relative)
		if !ok || len(fields) != 3 || fields[0] != "100644" || fields[1] != "blob" || !immutableCommit.MatchString(strings.ToLower(fields[2])) ||
			len(relative) > directBootstrapMaxPathBytes || path.Clean(relative) != relative || !strings.HasPrefix(relative, ".visual-hive/snapshots/") || !strings.HasSuffix(strings.ToLower(relative), ".png") {
			return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline entry %q is not a canonical regular PNG blob", relative)
		}
		sizeText, sizeErr := git(ctx, checkout, "cat-file", "-s", strings.ToLower(fields[2]))
		size, parseErr := strconv.ParseInt(strings.TrimSpace(sizeText), 10, 64)
		if sizeErr != nil || parseErr != nil || size <= 8 || size > directBootstrapMaxBaseline || totalBytes > directBootstrapMaxTotal-size {
			return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline %s has an invalid or excessive size", relative)
		}
		content, readErr := readSetupBaselineBlob(ctx, checkout, strings.ToLower(fields[2]))
		if readErr != nil || int64(len(content)) != size || !bytes.Equal(content[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
			return nil, "", fmt.Errorf("read direct-bootstrap reviewed PNG %s: %w", relative, readErr)
		}
		totalBytes += size
		digest := sha256.Sum256(content)
		candidates = append(candidates, SetupBaselineCandidate{Path: relative, SHA256: hex.EncodeToString(digest[:]), Bytes: size})
	}
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("direct-bootstrap reviewed baseline inventory is empty")
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	digest, err := setupBaselineCandidateDigest(candidates)
	return candidates, digest, err
}

func setupBaselineInventoryAtCommit(ctx context.Context, checkout, commitSHA string) ([]SetupBaselineCandidate, string, error) {
	commitSHA = strings.ToLower(strings.TrimSpace(commitSHA))
	if !immutableCommit.MatchString(commitSHA) {
		return nil, "", fmt.Errorf("exact setup commit is required for baseline inventory")
	}
	command := exec.CommandContext(ctx, "git", "-C", checkout, "ls-tree", "-r", "-z", commitSHA, "--", ".visual-hive/snapshots")
	command.Env = safeEnvironment()
	output, err := command.Output()
	if err != nil {
		return nil, "", fmt.Errorf("list pre-setup baseline tree: %w", err)
	}
	candidates := []SetupBaselineCandidate{}
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		metadata, relative, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		relative = filepath.ToSlash(relative)
		if !ok || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" || !immutableCommit.MatchString(strings.ToLower(fields[2])) ||
			!strings.HasPrefix(relative, ".visual-hive/snapshots/") || !strings.HasSuffix(strings.ToLower(relative), ".png") {
			return nil, "", fmt.Errorf("pre-setup baseline entry %q is not a canonical regular PNG blob", relative)
		}
		content, readErr := readSetupBaselineBlob(ctx, checkout, strings.ToLower(fields[2]))
		if readErr != nil || len(content) <= 8 || !bytes.Equal(content[:8], []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}) {
			return nil, "", fmt.Errorf("read canonical pre-setup PNG %s: %w", relative, readErr)
		}
		digest := sha256.Sum256(content)
		candidates = append(candidates, SetupBaselineCandidate{Path: relative, SHA256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	digest, err := setupBaselineCandidateDigest(candidates)
	return candidates, digest, err
}

func readSetupBaselineBlob(ctx context.Context, checkout, objectSHA string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", "-C", checkout, "cat-file", "blob", objectSHA)
	command.Env = safeEnvironment()
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %w: %s", objectSHA, err, safeOutput(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func writeManagedFiles(root string, config Config, inspection RepositoryInspection) error {
	if _, err := selectedPackageDependencyScopes(config.TestCommands); err != nil {
		return fmt.Errorf("refuse selected package dependency scope: %w", err)
	}
	if managedPathPreimagesConfigured(config) {
		if err := validateManagedPathPreimages(config.ManagedPreimagesVersion, config.ManagedPathPreimages, managedSetupFilesForConfig(config)); err != nil {
			return fmt.Errorf("refuse managed writes with an invalid managed-path preimage ledger: %w", err)
		}
	}
	repositoryConfig := map[string]any{
		"schema_version": managedRepositoryConfigSchema, "repository": config.Repository, "repository_id": config.RepositoryID,
		"default_branch": config.DefaultBranch, "coverage": config.Coverage, "automation": config.Automation,
		"provider": config.Provider, "acmm_level": config.ACMMLevel, "max_active_issues": config.MaxActiveIssues, "max_repair_attempts": config.MaxRepairAttempts, "visual_hive": config.VisualHive,
		"execution_mode": config.ExecutionMode, "run_interval_seconds": config.RunIntervalSeconds,
		"hosted_schedule": config.HostedSchedule, "hosted_state_branch": config.HostedStateBranch,
		"hive_release_repository": config.HiveReleaseRepository, "hive_release_version": config.HiveReleaseVersion,
		"hive_commit": config.HiveCommit, "distribution_manifest_sha256": config.DistributionManifestSHA256,
		"hosted_controller_protocol": config.HostedControllerProtocol, "hosted_workflow_sha256": config.HostedWorkflowSHA256,
		"visual_hive_repository": config.VisualHiveRepo, "visual_hive_ref": config.VisualHiveRef,
		"visual_hive_config_digest": config.VisualHiveConfigDigest,
		"test_commands":             config.TestCommands, "allowed_repair_paths": config.AllowedRepairPaths,
		"allowed_auto_merge_paths": config.AllowedAutoMergePaths, "allowed_auto_merge_risk": config.AllowedAutoMergeRisk,
		"setup_authorization_actor_id": config.SetupAuthorizationActorID,
	}
	if config.SetupAuthorizationPreviousActorID > 0 {
		repositoryConfig["setup_authorization_previous_actor_id"] = config.SetupAuthorizationPreviousActorID
	}
	if config.PreviousHostedRelease != nil {
		repositoryConfig["previous_hosted_release"] = cloneHostedReleaseIdentity(config.PreviousHostedRelease)
	}
	configData, err := json.MarshalIndent(repositoryConfig, "", "  ")
	if err != nil {
		return err
	}
	files := map[string]string{
		".hive/integrated.json":                  string(configData) + "\n",
		"docs/hive-quickstart.md":                quickstart(config, inspection),
		".github/workflows/hive-visual-hive.yml": workflow(config),
	}
	if config.VisualHive {
		files[".github/workflows/visual-hive-pr.yml"] = pullRequestWorkflow(config)
	}
	if config.ExecutionMode == ExecutionHosted {
		if config.HostedControllerProtocol != HostedControllerProtocol || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(config.HostedWorkflowSHA256) {
			return fmt.Errorf("hosted controller protocol or workflow digest is invalid")
		}
		controller, controllerErr := GenerateHostedControllerWorkflow(hostedWorkflowConfig(config))
		if controllerErr != nil {
			return controllerErr
		}
		digest := sha256.Sum256([]byte(controller))
		if hex.EncodeToString(digest[:]) != config.HostedWorkflowSHA256 {
			return fmt.Errorf("generated hosted controller does not match its durable content digest")
		}
		files[HostedControllerWorkflowPath] = controller
	}
	for relative, content := range files {
		if err := writeSetupCheckoutFile(root, relative, []byte(content)); err != nil {
			return err
		}
	}
	if config.VisualHive {
		for _, relative := range standaloneVisualHiveWriterWorkflowPaths() {
			if err := removeSetupCheckoutFile(root, relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func testCommandsForCoverage(inspection RepositoryInspection, coverage Coverage) [][]string {
	maximumRank := 1
	if coverage == CoverageStandard {
		maximumRank = 2
	} else if coverage == CoverageComprehensive || coverage == CoverageCustom {
		maximumRank = 3
	}
	commands := make([][]string, 0, len(inspection.TestCommands))
	for _, command := range inspection.TestCommands {
		rank := commandCoverageRank(command)
		if rank > 0 && rank <= maximumRank {
			commands = append(commands, append([]string(nil), command...))
		}
	}
	commands = preferSafePackageCheckCommands(inspection, commands)
	commands = preferBaselineAwareLintCheckCommands(inspection, commands)
	commands = preferRepositoryComprehensiveSuite(inspection, commands, coverage)
	if maximumRank < 3 || !containsValue(inspection.Languages, "TypeScript/JavaScript") || hasRepositoryUnitCommand(inspection, commands) {
		return sortTestCommands(commands)
	}
	commands = append(commands, []string{"node", "--test"})
	return sortTestCommands(commands)
}

// preferBaselineAwareLintCheckCommands removes a redundant raw lint command
// only when the same selected package exposes a rank-one baseline verifier
// whose committed implementation demonstrably executes the same eslint scope
// and fails on new violations. Unproven pairs remain selected and therefore
// fail visibly instead of silently weakening lint coverage.
func preferBaselineAwareLintCheckCommands(inspection RepositoryInspection, commands [][]string) [][]string {
	checks := map[string]bool{}
	for _, command := range commands {
		packagePath, runner, name, ok := packageScriptCommandParts(command)
		if ok && name == "lint:check" && commandCoverageRank(command) == 1 && inspection.baselineLint[packagePath] {
			checks[runner+"\x00"+packagePath] = true
		}
	}
	if len(checks) == 0 {
		return commands
	}
	result := make([][]string, 0, len(commands))
	for _, command := range commands {
		packagePath, runner, name, ok := packageScriptCommandParts(command)
		if ok && name == "lint" && checks[runner+"\x00"+packagePath] {
			continue
		}
		result = append(result, command)
	}
	return result
}

// preferSafePackageCheckCommands avoids running a repository writer when the
// same package exposes an admitted, deterministic rank-one verifier for the
// exact output. The check must already be selected at this coverage depth and
// retain the same package-manager scope as its build sibling.
func preferSafePackageCheckCommands(inspection RepositoryInspection, commands [][]string) [][]string {
	checks := map[string]bool{}
	for _, command := range commands {
		packagePath, runner, name, ok := packageScriptCommandParts(command)
		if !ok || !strings.HasPrefix(name, "check:") || commandCoverageRank(command) != 1 {
			continue
		}
		suffix := strings.TrimPrefix(name, "check:")
		if suffix == "" {
			continue
		}
		scripts := inspection.packageScripts[packagePath]
		if !safePackageCheckReplacesBuild(scripts, suffix) {
			continue
		}
		checks[runner+"\x00"+packagePath+"\x00"+suffix] = true
	}
	if len(checks) == 0 {
		return commands
	}
	result := make([][]string, 0, len(commands))
	for _, command := range commands {
		packagePath, runner, name, ok := packageScriptCommandParts(command)
		if ok && strings.HasPrefix(name, "build:") {
			suffix := strings.TrimPrefix(name, "build:")
			if suffix != "" && checks[runner+"\x00"+packagePath+"\x00"+suffix] {
				continue
			}
		}
		result = append(result, command)
	}
	return result
}

func safePackageCheckReplacesBuild(scripts map[string]string, suffix string) bool {
	checkName := "check:" + suffix
	buildName := "build:" + suffix
	checkBody, checkExists := scripts[checkName]
	buildBody, buildExists := scripts[buildName]
	if !checkExists || !buildExists ||
		!safeAutomationScript(checkName, checkBody) || !hasSafePackageScriptClosure(checkName, scripts) ||
		!safeAutomationScript(buildName, buildBody) || !hasSafePackageScriptClosure(buildName, scripts) {
		return false
	}
	buildBody = strings.TrimSpace(buildBody)
	if strings.Contains(buildBody, "&&") {
		return false
	}
	if _, transitive := strictPackageRunDependencies(buildBody); transitive {
		return false
	}
	return buildBody != "" && strings.TrimSpace(checkBody) == buildBody+" --check"
}

func commandCoverageRank(command []string) int {
	if len(command) == 0 {
		return 0
	}
	value := strings.ToLower(strings.Join(command, " "))
	name := commandScriptName(command)
	if strings.Contains(value, "python -m pytest") || strings.HasPrefix(value, "go test") || value == "node --test" {
		return 1
	}
	for _, unsafe := range []string{"watch", "update", "--ui", "--open", "headed", " dev", " serve", " preview"} {
		if strings.Contains(name, unsafe) || strings.Contains(value, unsafe) {
			return 0
		}
	}
	for _, deep := range []string{"performance", "perf", "load", "cross-browser", "responsive", "theme", "suite", "proof", "verify", "validation"} {
		if strings.Contains(name, deep) {
			return 3
		}
	}
	for _, standard := range []string{"integration", "e2e", "api", "visual", "a11y", "accessibility", "mutation", "mutate", "security"} {
		if strings.Contains(name, standard) {
			return 2
		}
	}
	if name == "test" || name == "build" || strings.Contains(name, "unit") || strings.Contains(name, "test:ci") || strings.Contains(name, "test:") || strings.Contains(name, "lint") || strings.Contains(name, "typecheck") || strings.Contains(name, "check") || strings.Contains(name, "build") {
		return 1
	}
	return 0
}

func hasRepositoryUnitCommand(inspection RepositoryInspection, commands [][]string) bool {
	for _, command := range commands {
		packagePath, _, script, ok := packageScriptCommandParts(command)
		if !ok {
			if invokesJavaScriptUnitRunner(command) {
				return true
			}
			continue
		}
		scripts := inspection.packageScripts[packagePath]
		terminals, safe := terminalPackageScripts(scripts, script)
		if !safe {
			continue
		}
		for invocation := range terminals {
			if invocation.Forwarded == "" && isJavaScriptUnitRunner(scripts[invocation.Name]) {
				return true
			}
		}
	}
	return false
}

// preferRepositoryComprehensiveSuite preserves the repository author's tested
// ordering when one safe aggregate provably reaches every selected leaf test.
// Orchestration wrappers (for example test:ci:lite) are not run beside that
// aggregate, avoiding repeated browser servers and overlapping smoke tests.
func preferRepositoryComprehensiveSuite(inspection RepositoryInspection, commands [][]string, coverage Coverage) [][]string {
	if coverage != CoverageComprehensive && coverage != CoverageCustom {
		return commands
	}
	replacements := map[string][]string{}
	for packagePath, scripts := range inspection.packageScripts {
		selected := map[string]bool{}
		for _, command := range commands {
			path, _, name, ok := packageScriptCommandParts(command)
			if ok && path == packagePath {
				selected[name] = true
			}
		}
		if len(selected) == 0 {
			continue
		}
		aggregate := comprehensiveAggregateScript(scripts, selected)
		if aggregate == "" {
			continue
		}
		runner := inspection.packageRunners[packagePath]
		if runner == "" {
			runner = "npm"
		}
		replacements[packagePath] = packageScriptCommand(packagePath, runner, aggregate)
	}
	if len(replacements) == 0 {
		return commands
	}
	result := make([][]string, 0, len(commands)+len(replacements))
	for _, command := range commands {
		packagePath, _, _, ok := packageScriptCommandParts(command)
		if ok && replacements[packagePath] != nil {
			continue
		}
		result = append(result, command)
	}
	paths := make([]string, 0, len(replacements))
	for packagePath := range replacements {
		paths = append(paths, packagePath)
	}
	sort.Strings(paths)
	for _, packagePath := range paths {
		result = append(result, replacements[packagePath])
	}
	return uniqueTestCommands(result)
}

func comprehensiveAggregateScript(scripts map[string]string, selected map[string]bool) string {
	for _, candidate := range []string{"test:all", "test:ci:heavy", "test:suite", "vh:suite", "test:ci"} {
		body, exists := scripts[candidate]
		if !exists || !safeAutomationScript(candidate, body) || invokesSupersededUnscopedPlaywrightE2E(candidate, scripts) {
			continue
		}
		dependencies, strict := strictPackageRunDependencies(body)
		if !strict || len(dependencies) < 2 {
			continue
		}
		candidateTerminals, safe := terminalPackageScripts(scripts, candidate)
		if !safe || len(candidateTerminals) == 0 {
			continue
		}
		selectedTerminals := map[packageScriptInvocation]bool{}
		for name := range selected {
			if name == candidate {
				continue
			}
			terminals, selectedSafe := terminalPackageScripts(scripts, name)
			if !selectedSafe {
				selectedTerminals = nil
				break
			}
			for terminal := range terminals {
				selectedTerminals[terminal] = true
			}
		}
		for terminal := range selectedTerminals {
			if terminal.Forwarded != "" && selectedTerminals[packageScriptInvocation{Name: terminal.Name}] {
				delete(selectedTerminals, terminal)
			}
		}
		if len(selectedTerminals) == 0 {
			continue
		}
		coversSelectedTerminals := true
		for terminal := range selectedTerminals {
			if !candidateTerminals[terminal] && !equivalentVitestCoverageLeaf(scripts, terminal, candidateTerminals) {
				coversSelectedTerminals = false
				break
			}
		}
		if coversSelectedTerminals {
			return candidate
		}
	}
	return ""
}

func terminalPackageScripts(scripts map[string]string, root string) (map[packageScriptInvocation]bool, bool) {
	terminals := map[packageScriptInvocation]bool{}
	visiting := map[packageScriptInvocation]bool{}
	complete := map[packageScriptInvocation]bool{}
	var visit func(packageScriptInvocation) bool
	visit = func(invocation packageScriptInvocation) bool {
		if visiting[invocation] {
			return false
		}
		if complete[invocation] {
			return true
		}
		body, exists := scripts[invocation.Name]
		if !exists || !safeAutomationScript(invocation.Name, body) {
			return false
		}
		visiting[invocation] = true
		dependencies, strict := strictPackageRunDependencies(body)
		if !strict || invocation.Forwarded != "" {
			terminals[invocation] = true
		} else {
			for _, dependency := range dependencies {
				if !visit(dependency) {
					return false
				}
			}
		}
		delete(visiting, invocation)
		complete[invocation] = true
		return true
	}
	if !visit(packageScriptInvocation{Name: root}) {
		return nil, false
	}
	return terminals, true
}

func strictPackageRunDependencies(body string) ([]packageScriptInvocation, bool) {
	parts := strings.Split(strings.TrimSpace(body), "&&")
	if len(parts) == 0 {
		return nil, false
	}
	dependencies := make([]packageScriptInvocation, 0, len(parts))
	for _, part := range parts {
		match := strictPackageRunPattern.FindStringSubmatch(strings.TrimSpace(part))
		if len(match) < 4 {
			return nil, false
		}
		forwarded := strings.Fields(strings.TrimSpace(match[4]))
		if len(forwarded) > 0 && forwarded[0] == "--" {
			forwarded = forwarded[1:]
		}
		dependencies = append(dependencies, packageScriptInvocation{Name: match[3], Forwarded: strings.Join(forwarded, "\x00")})
	}
	return dependencies, true
}

func equivalentVitestCoverageLeaf(scripts map[string]string, selected packageScriptInvocation, candidateTerminals map[packageScriptInvocation]bool) bool {
	if selected.Forwarded != "" || !strings.Contains(strings.ToLower(selected.Name), "unit") {
		return false
	}
	selectedTokens, selectedCoverage, selectedOK := normalizedVitestCommand(scripts[selected.Name])
	if !selectedOK || selectedCoverage {
		return false
	}
	for candidate := range candidateTerminals {
		if candidate.Forwarded != "" || !strings.Contains(strings.ToLower(candidate.Name), "coverage") {
			continue
		}
		candidateTokens, candidateCoverage, candidateOK := normalizedVitestCommand(scripts[candidate.Name])
		if candidateOK && candidateCoverage && strings.Join(selectedTokens, "\x00") == strings.Join(candidateTokens, "\x00") {
			return true
		}
	}
	return false
}

func normalizedVitestCommand(body string) ([]string, bool, bool) {
	parts := strings.Fields(strings.TrimSpace(body))
	if len(parts) == 0 {
		return nil, false, false
	}
	for _, part := range parts {
		if !safeShellTokenPattern.MatchString(part) {
			return nil, false, false
		}
	}
	executable := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	if executable != "vitest" && executable != "vitest.cmd" {
		return nil, false, false
	}
	normalized := make([]string, 0, len(parts))
	hadCoverage := false
	for _, part := range parts {
		lower := strings.ToLower(part)
		if lower == "--coverage" || strings.HasPrefix(lower, "--coverage=") || strings.HasPrefix(lower, "--coverage.") {
			hadCoverage = true
			continue
		}
		normalized = append(normalized, part)
	}
	return normalized, hadCoverage, true
}

func isJavaScriptUnitRunner(body string) bool {
	for _, segment := range strings.Split(strings.TrimSpace(body), "&&") {
		parts := strings.Fields(strings.TrimSpace(segment))
		if len(parts) == 0 {
			continue
		}
		valid := true
		for _, part := range parts {
			if !safeShellTokenPattern.MatchString(part) {
				valid = false
				break
			}
		}
		if valid && invokesJavaScriptUnitRunner(parts) {
			return true
		}
	}
	return false
}

func invokesJavaScriptUnitRunner(parts []string) bool {
	for len(parts) > 0 && environmentTokenPattern.MatchString(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return false
	}
	executable := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	parts = parts[1:]
	switch executable {
	case "vitest", "vitest.cmd", "jest", "jest.cmd", "mocha", "mocha.cmd", "ava", "ava.cmd", "tap", "tap.cmd", "uvu", "uvu.cmd":
		return !hasNoExecutionMode(parts)
	case "node", "node.exe":
		if hasNoExecutionMode(parts) {
			return false
		}
		for _, argument := range parts {
			if argument == "--test" {
				return true
			}
		}
	case "react-scripts", "react-scripts.cmd":
		return len(parts) > 0 && parts[0] == "test" && !hasNoExecutionMode(parts[1:])
	case "npx", "npx.cmd":
		if len(parts) > 0 && parts[0] == "--no-install" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "npm", "npm.cmd":
		if len(parts) == 0 || parts[0] != "exec" {
			return false
		}
		parts = parts[1:]
		if len(parts) > 0 && parts[0] == "--" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "pnpm", "pnpm.cmd", "yarn", "yarn.cmd":
		if len(parts) > 0 && parts[0] == "exec" {
			parts = parts[1:]
		}
		return isKnownJavaScriptUnitInvocation(parts)
	case "cross-env", "cross-env.cmd":
		for len(parts) > 0 && environmentTokenPattern.MatchString(parts[0]) {
			parts = parts[1:]
		}
		return len(parts) > 0 && invokesJavaScriptUnitRunner(parts)
	}
	return false
}

func isKnownJavaScriptUnitInvocation(parts []string) bool {
	if len(parts) == 0 {
		return false
	}
	name := strings.ToLower(filepath.Base(filepath.ToSlash(parts[0])))
	switch name {
	case "vitest", "vitest.cmd", "jest", "jest.cmd", "mocha", "mocha.cmd", "ava", "ava.cmd", "tap", "tap.cmd", "uvu", "uvu.cmd":
		return !hasNoExecutionMode(parts[1:])
	default:
		return false
	}
}

func hasNoExecutionMode(arguments []string) bool {
	for index, argument := range arguments {
		value := strings.ToLower(argument)
		for _, denied := range []string{
			"--help", "-h", "help", "--version", "-v", "version", "init", "--init",
			"list", "--list", "--listtests", "--list-tests", "--showconfig", "--show-config",
			"--clearcache", "--clear-cache", "--collect-only", "--collectonly", "--dry-run", "--dryrun",
		} {
			if value == denied || strings.HasPrefix(value, denied+"=") {
				return true
			}
		}
		if (value == "config" || value == "--config") && index == len(arguments)-1 {
			return true
		}
	}
	return false
}

func packageScriptCommandParts(command []string) (packagePath, runner, name string, ok bool) {
	if len(command) == 3 && command[1] == "run" && (command[0] == "npm" || command[0] == "pnpm" || command[0] == "yarn") {
		return ".", command[0], command[2], true
	}
	if len(command) != 5 || command[3] != "run" {
		return "", "", "", false
	}
	switch {
	case command[0] == "npm" && command[1] == "--prefix":
		return filepath.ToSlash(command[2]), "npm", command[4], true
	case command[0] == "pnpm" && command[1] == "--dir":
		return filepath.ToSlash(command[2]), "pnpm", command[4], true
	case command[0] == "yarn" && command[1] == "--cwd":
		return filepath.ToSlash(command[2]), "yarn", command[4], true
	default:
		return "", "", "", false
	}
}

func sortTestCommands(commands [][]string) [][]string {
	commands = cloneCommands(commands)
	sort.SliceStable(commands, func(i, j int) bool {
		left, right := testCommandPriority(commands[i]), testCommandPriority(commands[j])
		if left != right {
			return left < right
		}
		return strings.Join(commands[i], "\x00") < strings.Join(commands[j], "\x00")
	})
	return commands
}

func targetDependencyInstallShell() string {
	return targetPackageInstallShell() + `
          if [ -f pyproject.toml ]; then
            python -m pip install -e .
          elif [ -f requirements.txt ]; then
            python -m pip install -r requirements.txt
          fi
          tooling_playwright="$RUNNER_TEMP/visual-hive-tooling/node_modules/@playwright/test/cli.js"
          node "$tooling_playwright" install --with-deps chromium
          while IFS= read -r -d '' playwright_cli; do
            node "$playwright_cli" install chromium
          done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)`
}

func targetPackageInstallShell() string {
	return `          corepack_enabled=0
          enable_corepack() {
            if [ "$corepack_enabled" -eq 0 ]; then
              corepack enable
              corepack_enabled=1
            fi
          }
          validate_package_scope() {
            local lockfile="$1"
            local package_dir
            package_dir="$(dirname "$lockfile")"
            if [ -f "$package_dir/package.json" ]; then
              return 0
            fi
            if [ "$(basename "$lockfile")" != "package-lock.json" ]; then
              echo "Package lock $lockfile has no sibling package.json" >&2
              return 1
            fi
            if ! node - "$lockfile" <<'NODE'
          const fs = require("fs");
          const lockfile = process.argv[2];
          const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
          let lock;
          try {
            lock = JSON.parse(fs.readFileSync(lockfile, "utf8"));
          } catch (_) {
            process.exit(1);
          }
          if (!isObject(lock) || !isObject(lock.packages) || Object.keys(lock.packages).length !== 0) {
            process.exit(1);
          }
          if (Object.prototype.hasOwnProperty.call(lock, "dependencies") &&
              (!isObject(lock.dependencies) || Object.keys(lock.dependencies).length !== 0)) {
            process.exit(1);
          }
          NODE
            then
              echo "Orphan npm lock $lockfile is not provably inert; add its package.json or remove the stale lock" >&2
              return 1
            fi
            echo "Ignoring provably inert orphan npm lock $lockfile (no sibling package.json)"
          }
          while IFS= read -r -d '' lockfile; do
            package_dir="$(dirname "$lockfile")"
            lock_count=0
            [ -f "$package_dir/package-lock.json" ] && lock_count=$((lock_count + 1))
            [ -f "$package_dir/pnpm-lock.yaml" ] && lock_count=$((lock_count + 1))
            [ -f "$package_dir/yarn.lock" ] && lock_count=$((lock_count + 1))
            if [ "$lock_count" -ne 1 ]; then
              echo "Package scope $package_dir must contain exactly one supported lockfile" >&2
              exit 1
            fi
            validate_package_scope "$lockfile"
          done < <(find . \( -name package-lock.json -o -name pnpm-lock.yaml -o -name yarn.lock \) -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            package_dir="$(dirname "$lockfile")"
            if [ ! -f "$package_dir/package.json" ]; then
              validate_package_scope "$lockfile"
              continue
            fi
            test -f "$package_dir/package-lock.json"
            (cd "$package_dir" && npm ci)
          done < <(find . -name package-lock.json -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            enable_corepack
            package_dir="$(dirname "$lockfile")"
            (cd "$package_dir" && pnpm install --frozen-lockfile)
          done < <(find . -name pnpm-lock.yaml -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          while IFS= read -r -d '' lockfile; do
            enable_corepack
            package_dir="$(dirname "$lockfile")"
            yarn_version="$(cd "$package_dir" && yarn --version)"
            yarn_major="${yarn_version%%.*}"
            if ! [[ "$yarn_major" =~ ^[0-9]+$ ]]; then
              echo "Could not determine Yarn major version for $package_dir" >&2
              exit 1
            fi
            if [ "$yarn_major" -ge 2 ]; then
              (cd "$package_dir" && yarn install --immutable)
            else
              (cd "$package_dir" && yarn install --frozen-lockfile)
            fi
          done < <(find . -name yarn.lock -not -path '*/node_modules/*' -not -path './.git/*' -print0 | sort -z)
          has_ancestor_package_lock() {
            local candidate="$1"
            node - "$candidate" <<'NODE'
          const fs = require("fs");
          const path = require("path");
          const root = fs.realpathSync(process.cwd());
          const target = fs.realpathSync(process.argv[2]);
          const locks = ["package-lock.json", "pnpm-lock.yaml", "yarn.lock"];

          function patternsFor(scope) {
            const patterns = [];
            try {
              const manifest = JSON.parse(fs.readFileSync(path.join(scope, "package.json"), "utf8"));
              if (Array.isArray(manifest.workspaces)) patterns.push(...manifest.workspaces);
              if (manifest.workspaces && Array.isArray(manifest.workspaces.packages)) patterns.push(...manifest.workspaces.packages);
            } catch (_) {}
            try {
              const lines = fs.readFileSync(path.join(scope, "pnpm-workspace.yaml"), "utf8").split(/\r?\n/);
              let inPackages = false;
              for (const line of lines) {
                const inline = line.match(/^\s*packages\s*:\s*\[(.*)\]\s*$/);
                if (inline) {
                  for (const value of inline[1].split(",")) patterns.push(value.trim().replace(/^['"]|['"]$/g, ""));
                  inPackages = false;
                  continue;
                }
                if (/^\s*packages\s*:\s*$/.test(line)) {
                  inPackages = true;
                  continue;
                }
                if (inPackages && /^\S/.test(line)) inPackages = false;
                if (inPackages) {
                  const item = line.match(/^\s*-\s*(['"]?)(.*?)\1\s*(?:#.*)?$/);
                  if (item && item[2]) patterns.push(item[2]);
                }
              }
            } catch (_) {}
            return patterns;
          }

          function globExpression(pattern) {
            pattern = pattern.split(String.fromCharCode(92)).join("/").replace(/^\.\//, "").replace(/^\/+|\/+$/g, "");
            let source = "^";
            for (let index = 0; index < pattern.length; index += 1) {
              const character = pattern[index];
              if (character === "*") {
                if (pattern[index + 1] === "*") {
                  index += 1;
                  if (pattern[index + 1] === "/") {
                    index += 1;
                    source += "(?:.*/)?";
                  } else {
                    source += ".*";
                  }
                } else {
                  source += "[^/]*";
                }
              } else if (character === "?") {
                source += "[^/]";
              } else {
                if (".+()|[]{}^$".includes(character)) source += "\\";
                source += character;
              }
            }
            return new RegExp(source + "$");
          }

          function owns(scope) {
            const relative = path.relative(scope, target).split(path.sep).join("/");
            let matched = false;
            for (let pattern of patternsFor(scope)) {
              pattern = String(pattern).trim().split(String.fromCharCode(92)).join("/");
              const excluded = pattern.startsWith("!");
              if (excluded) pattern = pattern.slice(1);
              if (pattern && globExpression(pattern).test(relative)) matched = !excluded;
            }
            return matched;
          }

          function insideRoot(value) {
            const relative = path.relative(root, value);
            return relative === "" || (relative !== ".." && !relative.startsWith(".." + path.sep));
          }

          let scope = target;
          while (insideRoot(scope)) {
            const hasLock = locks.some((name) => fs.existsSync(path.join(scope, name)));
            if (hasLock && (scope === target || owns(scope))) process.exit(0);
            if (scope === root) break;
            const parent = path.dirname(scope);
            if (parent === scope) break;
            scope = parent;
          }
          process.exit(1);
          NODE
          }
          lockless_package_dirs=()
          while IFS= read -r -d '' manifest; do
            package_dir="$(dirname "$manifest")"
            if has_ancestor_package_lock "$package_dir"; then
              continue
            fi
            lockless_package_dirs+=("$package_dir")
          done < <(find . -name package.json -not -path '*/node_modules/*' -not -path './.git/*' -not -path '*/dist/*' -not -path '*/build/*' -not -path '*/coverage/*' -not -path '*/vendor/*' -not -path './.visual-hive/*' -print0 | sort -z)
          for package_dir in "${lockless_package_dirs[@]}"; do
            (cd "$package_dir" && npm install)
          done`
}

func repositoryTestShell(config Config) string {
	lines := []string{
		"          mkdir -p .visual-hive",
		"          repository_exit=0",
		"          printf '1\\n' > .visual-hive/pipeline-exit-code.txt",
		"          printf '1\\n' > .visual-hive/repository-test-exit-code.txt",
		"          : > .visual-hive/repository-tests.tsv",
		"          run_repository_test() {",
		"            set +e",
		"            \"$@\"",
		"            command_exit=$?",
		"            set -e",
		"            printf '%s\\t%s\\n' \"$command_exit\" \"$*\" >> .visual-hive/repository-tests.tsv",
		"            if [ \"$command_exit\" -ne 0 ] && [ \"$repository_exit\" -eq 0 ]; then",
		"              repository_exit=$command_exit",
		"            fi",
		"            return 0",
		"          }",
	}
	if len(config.TestCommands) == 0 {
		lines = append(lines, "          : # No additional repository unit-test bootstrap was required.")
	}
	for _, command := range config.TestCommands {
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		lines = append(lines, "          run_repository_test "+strings.Join(quoted, " "))
	}
	lines = append(lines, "          printf '%s\\n' \"$repository_exit\" > .visual-hive/repository-test-exit-code.txt")
	return strings.Join(lines, "\n")
}

// repositoryTestWorkflowJobs isolates every repository-authored command in its
// own GitHub-hosted job. Hive queries the exact job conclusions from GitHub's
// API; target files, step outputs, and uploaded artifact bytes are never an
// authority source for opening or resolving repository-test findings.
func repositoryTestWorkflowJobs(config Config) (string, string) {
	var jobs, needs strings.Builder
	dependencyInstall := repositoryTestDependencyInstallShell()
	jobIndex := 0
	for _, command := range config.TestCommands {
		if len(command) == 0 {
			continue
		}
		jobIndex++
		quoted := make([]string, 0, len(command))
		for _, argument := range command {
			quoted = append(quoted, shellQuote(argument))
		}
		fmt.Fprintf(&jobs, `  repository-test-%03d:
    name: Hive repository test %03d
    runs-on: ubuntu-latest
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@%s
        with:
          fetch-depth: 1
          persist-credentials: false
      - uses: actions/setup-node@%s
        with:
          node-version: 22.23.1
      - uses: actions/setup-python@%s
        with:
          python-version: "3.11"
      - name: Install repository test dependencies
        shell: bash
        run: |
%s
      - name: Execute exact repository test command
        shell: bash
        run: |
          set -euo pipefail
          env -u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_OUTPUT -u GITHUB_ENV -u GITHUB_PATH -u GITHUB_STEP_SUMMARY %s

`, jobIndex, jobIndex, checkoutActionSHA, setupNodeActionSHA, setupPythonActionSHA, dependencyInstall, strings.Join(quoted, " "))
		fmt.Fprintf(&needs, "      - repository-test-%03d\n", jobIndex)
	}
	if jobIndex == 0 {
		return "", "    if: ${{ always() }}"
	}
	return strings.TrimSuffix(jobs.String(), "\n"), "    needs:\n" + strings.TrimSuffix(needs.String(), "\n") + "\n    if: ${{ always() }}"
}

func repositoryTestDependencyInstallShell() string {
	return targetPackageInstallShell() + `
          if [ -f pyproject.toml ]; then
            python -m pip install -e .
          elif [ -f requirements.txt ]; then
            python -m pip install -r requirements.txt
          fi
          while IFS= read -r -d '' playwright_cli; do
            node "$playwright_cli" install --with-deps chromium
          done < <(find . -path '*/node_modules/@playwright/test/cli.js' -not -path './.git/*' -print0 | sort -z)`
}

func shellQuote(value string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`).MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func containsValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func quickstart(config Config, inspection RepositoryInspection) string {
	normalOwner := UsesNormalHiveRuntime(config)
	activation := ""
	if config.Automation == AutomationAutoMerge {
		nextCycle := "next started Hive run"
		if normalOwner {
			nextCycle = "next ordinary Hive cycle after the existing service restarts"
		}
		activation = fmt.Sprintf("\n\nAuto-merge protection uses two-phase activation: merge the exact managed setup/upgrade PR first. The %s verifies the installed files and exact production workflow path/event/head, completes both `visual-hive-production` and the actively guarded `visual-hive` eligibility seed on the current default head, and only then creates or verifies strict protection with the PR `visual-hive` context bound to the GitHub Actions App ID. Merge gating accepts that context only from `.github/workflows/visual-hive-pr.yml` on the exact pull-request head. No issue, repair, or merge lifecycle write occurs before activation.", nextCycle)
	}
	runtimeGuidance := "Hive automatically selects this repository's isolated persistent state."
	commands := "hive doctor --json\nhive status --json\nhive run --json\nhive start --json\nhive stop --json\nhive pause\nhive resume\nhive approve-merge --pr NUMBER --head EXACT_HEAD_SHA --plan --json\nhive approve-merge --pr NUMBER --head EXACT_HEAD_SHA --base EXACT_BASE_SHA --diff-digest EXACT_DIFF_SHA256 --reason \"reviewed exact path-held repair\" --json\nhive revoke-merge-approval --reason \"review withdrawn\" --json\nhive retry-repair --finding FINGERPRINT --recurrence N --attempt N --failure-class infrastructure --failure-id FAILURE_ID --reason \"dependency restored\" --json"
	if normalOwner {
		stateDir := shellQuote(config.StateDir)
		runtimeGuidance = fmt.Sprintf("The existing ordinary Hive/dashboard process owns this local Visual Hive repair runtime. Configure that same process with the exact `HIVE_STATE_DIR=%s`, ensure its normal project scope includes `%s`, and confirm its existing dashboard listener is HTTP-ready before restarting ordinary Hive/dashboard. Do not use `hive run` or `hive start` for normal operation. Use `hive stop` only when `hive status` or `hive doctor` directs cleanup of a stale legacy scheduler; it does not stop ordinary Hive/dashboard.", config.StateDir, config.Repository)
		commands = fmt.Sprintf("hive doctor --state-dir %s --json\nhive status --state-dir %s --json\nhive pause --state-dir %s --json\nhive resume --state-dir %s --json\nhive approve-merge --state-dir %s --pr NUMBER --head EXACT_HEAD_SHA --plan --json\nhive approve-merge --state-dir %s --pr NUMBER --head EXACT_HEAD_SHA --base EXACT_BASE_SHA --diff-digest EXACT_DIFF_SHA256 --reason \"reviewed exact path-held repair\" --json\nhive revoke-merge-approval --state-dir %s --reason \"review withdrawn\" --json\nhive retry-repair --state-dir %s --finding FINGERPRINT --recurrence N --attempt N --failure-class infrastructure --failure-id FAILURE_ID --reason \"dependency restored\" --json", stateDir, stateDir, stateDir, stateDir, stateDir, stateDir, stateDir, stateDir)
	}
	return fmt.Sprintf("# Hive + Visual Hive\n\nThis repository is managed by Hive with `%s` coverage and `%s` automation authority. Visual Hive runs deterministic checks; Hive alone owns issues, repair branches, pull requests, merges, and closure. Hive keeps at most %d managed findings active as GitHub issues at once and permits at most %d bounded repair attempts per finding; every additional finding remains durable as a bead until capacity is available.%s\n\n## Operator commands\n\n%s Run:\n\n```sh\n%s\n```\n\nIf status reports `workflow_dispatch_recovery`, use its exact `revoke_plan_command` (preferred for uncertain transport) or `retry_plan_command`; never delete the dispatch state manually.\n\nDefault branch: `%s`. Detected languages: %s.\n", config.Coverage, config.Automation, config.MaxActiveIssues, config.MaxRepairAttempts, activation, runtimeGuidance, commands, config.DefaultBranch, strings.Join(inspection.Languages, ", "))
}

func setupPRBody(marker string, plan SetupPlan) string {
	activation := ""
	if plan.Automation == AutomationAutoMerge {
		activation = " After this exact PR is merged, the next trusted `hive run` verifies these files and the exact production workflow provenance, completes the production verdict plus actively guarded PR-context eligibility seed, and only then activates exact GitHub-Actions-App-bound branch protection. A scheduler requested during setup remains deferred until those non-scheduler doctor gates are green. Merge gating still accepts `visual-hive` only from the exact PR workflow; no lifecycle write occurs before activation."
	}
	return fmt.Sprintf("%s\n\nInstalls Hive + Visual Hive as one production testing and repair experience.\n\n- Coverage: **%s**\n- Automation authority: **%s**\n- ACMM enforcement: **L%d**\n- Active issue WIP limit: **%d**\n- Repair attempt limit: **%d**\n- Provider: **%s**\n- Visual Hive source: `%s@%s`\n- Detected languages: %s\n- Testing layers: %s\n\nThe Visual workflow is read-only and uploads provenance-bound evidence. Hive is the only GitHub lifecycle writer.%s", marker, plan.Coverage, plan.Automation, plan.ACMMLevel, plan.MaxActiveIssues, plan.MaxRepairAttempts, plan.Provider, plan.VisualHiveRepository, plan.VisualHiveRef, strings.Join(plan.Inspection.Languages, ", "), strings.Join(plan.TestingLayers, ", "), activation)
}

func authorizeSetup(store *Store, policy automation.Policy, repository string, action automation.Action) error {
	decision := policy.Authorize(automation.ActionRequest{Action: action, Agent: "setup", Repository: repository, SetupApproved: true})
	if err := store.AuditStrict(AuditEntry{Action: string(action), Allowed: decision.Allowed, Repository: repository, Detail: strings.Join(decision.Reasons, "; ")}); err != nil {
		return fmt.Errorf("persist setup authorization audit: %w", err)
	}
	if !decision.Allowed {
		return fmt.Errorf("%s denied: %s", action, strings.Join(decision.Reasons, "; "))
	}
	return nil
}

func enrichRemoteInspection(ctx context.Context, client *hivegithub.Client, repository string, inspection *RepositoryInspection) {
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || client.GoGitHub() == nil {
		return
	}
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, repo)
	if err == nil {
		inspection.RepositoryID = fmt.Sprintf("%d", metadata.GetID())
		if metadata.GetDefaultBranch() != "" {
			inspection.DefaultBranch = metadata.GetDefaultBranch()
		}
		for key, value := range metadata.GetPermissions() {
			inspection.Permissions[key] = value
		}
	}
	if _, _, err := client.GoGitHub().Repositories.GetBranchProtection(ctx, owner, repo, inspection.DefaultBranch); err == nil {
		inspection.BranchProtection = true
	}
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	if operation := localGitOperation(args); operation == "clone" || operation == "fetch" || operation == "push" || operation == "ls-remote" {
		return "", fmt.Errorf("network-capable Git operation %q must use the explicit authenticated transport boundary", operation)
	}
	if localGitOperationCanExecuteRepositoryConfig(args) {
		if _, err := inspectControllerGitConfig(ctx, dir, false); err != nil {
			return "", err
		}
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = gittransport.LocalEnvironment(safeEnvironment())
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, safeOutput(output.String()))
	}
	return output.String(), nil
}

type controllerGitConfigSnapshot struct {
	present bool
	digest  [sha256.Size]byte
}

// gitTransport is the only authenticated Git boundary in the integrated
// package. The caller must supply the exact canonical URL as an argument to a
// bounded network operation; target-controlled remote names are never used.
func gitTransport(ctx context.Context, dir, remoteURL string, args ...string) (string, error) {
	operation, configDir, localRemote, err := validateGitTransportRequest(dir, remoteURL, args)
	if err != nil {
		return "", err
	}
	before, err := inspectControllerGitConfig(ctx, configDir, true)
	if err != nil {
		return "", fmt.Errorf("validate repository Git config before %s: %w", operation, err)
	}
	if operation == "push" {
		if err := checkpoint.BeforeMutation(ctx, "Git push"); err != nil {
			return "", err
		}
	}

	commandArgs := []string{"-c", "protocol.allow=never"}
	if localRemote {
		commandArgs = append(commandArgs, "-c", "protocol.file.allow=always")
	} else {
		commandArgs = append(commandArgs, "-c", "protocol.https.allow=always")
	}
	commandArgs = append(commandArgs,
		"-c", "http.sslVerify=true",
		"-c", "http.followRedirects=false",
		"-c", "http.proxy=",
	)
	commandArgs = append(commandArgs, args...)
	transportCtx := ctx
	if !localRemote {
		transportCtx, err = gittransport.RefreshControllerToken(ctx)
		if err != nil {
			return "", fmt.Errorf("refresh authenticated Git credential for %s: %w", operation, err)
		}
	}
	command := exec.CommandContext(transportCtx, "git", commandArgs...)
	command.Dir = dir
	command.Env = gittransport.TransportEnvironmentForURL(transportCtx, safeEnvironment(), remoteURL)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	runErr := command.Run()

	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	after, auditErr := inspectControllerGitConfig(auditCtx, configDir, true)
	cancel()
	if auditErr != nil {
		return output.String(), fmt.Errorf("revalidate repository Git config after %s: %w", operation, auditErr)
	}
	if before.present && (!after.present || before.digest != after.digest) {
		return output.String(), fmt.Errorf("repository Git config changed during authenticated %s", operation)
	}
	if runErr != nil {
		return output.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), runErr, safeOutput(output.String()))
	}
	return output.String(), nil
}

func validateGitTransportRequest(dir, remoteURL string, args []string) (operation, configDir string, localRemote bool, err error) {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" || strings.ContainsAny(remoteURL, "\r\n\x00") || len(args) < 2 {
		return "", "", false, fmt.Errorf("authenticated Git transport requires an exact bounded remote URL and operation")
	}
	operation = strings.ToLower(strings.TrimSpace(args[0]))
	if operation != "clone" && operation != "fetch" && operation != "push" && operation != "ls-remote" {
		return "", "", false, fmt.Errorf("authenticated Git transport refuses operation %q", operation)
	}
	remoteCount, remoteIndex := 0, -1
	for index, argument := range args {
		if strings.ContainsAny(argument, "\r\n\x00") {
			return "", "", false, fmt.Errorf("authenticated Git transport refuses an unsafe argument")
		}
		lower := strings.ToLower(argument)
		for _, forbidden := range []string{"--config", "--upload-pack", "--receive-pack", "--exec"} {
			if lower == forbidden || strings.HasPrefix(lower, forbidden+"=") {
				return "", "", false, fmt.Errorf("authenticated Git transport refuses helper override %q", argument)
			}
		}
		if argument == remoteURL {
			remoteCount++
			remoteIndex = index
		}
	}
	if remoteCount != 1 || remoteIndex <= 0 {
		return "", "", false, fmt.Errorf("authenticated Git %s must use the exact explicit remote URL once", operation)
	}
	localRemote, err = validateExplicitGitRemoteURL(remoteURL)
	if err != nil {
		return "", "", false, err
	}
	configDir = dir
	if operation == "clone" {
		if remoteIndex != len(args)-2 || !filepath.IsAbs(args[len(args)-1]) {
			return "", "", false, fmt.Errorf("authenticated Git clone requires one exact absolute destination after the remote URL")
		}
		configDir = filepath.Clean(args[len(args)-1])
	}
	return operation, configDir, localRemote, nil
}

func validateExplicitGitRemoteURL(remoteURL string) (bool, error) {
	if filepath.IsAbs(remoteURL) {
		return true, nil
	}
	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return false, fmt.Errorf("authenticated Git transport requires a credential-free canonical GitHub HTTPS URL")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || !strings.HasSuffix(parts[1], ".git") || strings.TrimSuffix(parts[1], ".git") == "" {
		return false, fmt.Errorf("authenticated Git transport URL does not identify one exact GitHub repository")
	}
	return false, nil
}

func inspectControllerGitConfig(ctx context.Context, dir string, transport bool) (controllerGitConfigSnapshot, error) {
	snapshot := controllerGitConfigSnapshot{}
	metadata, err := os.Lstat(filepath.Join(dir, ".git"))
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if metadata.Mode()&os.ModeSymlink != 0 {
		return snapshot, fmt.Errorf("repository Git metadata must not be a symbolic link")
	}
	snapshot.present = true
	hasher := sha256.New()
	localRaw, localEntries, err := readRawControllerGitConfig(ctx, dir, "--local")
	if err != nil {
		return controllerGitConfigSnapshot{}, err
	}
	worktreeEnabled, err := rawWorktreeConfigEnabled(localEntries)
	if err != nil {
		return controllerGitConfigSnapshot{}, err
	}
	scopes := []struct {
		name    string
		raw     []byte
		entries []controllerGitConfigEntry
	}{{name: "--local", raw: localRaw, entries: localEntries}}
	if worktreeEnabled {
		worktreeRaw, worktreeEntries, readErr := readRawControllerGitConfig(ctx, dir, "--worktree")
		if readErr != nil {
			return controllerGitConfigSnapshot{}, readErr
		}
		scopes = append(scopes, struct {
			name    string
			raw     []byte
			entries []controllerGitConfigEntry
		}{name: "--worktree", raw: worktreeRaw, entries: worktreeEntries})
	} else {
		scopes = append(scopes, struct {
			name    string
			raw     []byte
			entries []controllerGitConfigEntry
		}{name: "--worktree-disabled", raw: []byte("absent\x00")})
	}
	for _, scope := range scopes {
		_, _ = hasher.Write([]byte(scope.name))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write(scope.raw)
		for _, entry := range scope.entries {
			if unsafeGitExecutionConfig(entry.key, entry.value) || (transport && unsafeGitTransportConfigKey(entry.key)) {
				return controllerGitConfigSnapshot{}, fmt.Errorf("repository Git config key %q is unsafe for controller execution", entry.key)
			}
		}
	}
	copy(snapshot.digest[:], hasher.Sum(nil))
	return snapshot, nil
}

type controllerGitConfigEntry struct {
	key   string
	value string
}

func readRawControllerGitConfig(ctx context.Context, dir, scope string) ([]byte, []controllerGitConfigEntry, error) {
	command := exec.CommandContext(ctx, "git", "config", scope, "--no-includes", "--null", "--list")
	command.Dir = dir
	command.Env = gittransport.LocalEnvironment(safeEnvironment())
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Run(); err != nil {
		return nil, nil, fmt.Errorf("read raw %s Git config: %w: %s", strings.TrimPrefix(scope, "--"), err, safeOutput(output.String()))
	}
	raw := append([]byte(nil), output.Bytes()...)
	return raw, parseRawGitConfigRecords(raw), nil
}

func parseRawGitConfigRecords(raw []byte) []controllerGitConfigEntry {
	entries := []controllerGitConfigEntry{}
	for _, record := range bytes.Split(raw, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		key, value, found := bytes.Cut(record, []byte{'\n'})
		if !found {
			value = nil
		}
		entries = append(entries, controllerGitConfigEntry{key: strings.ToLower(strings.TrimSpace(string(key))), value: string(value)})
	}
	return entries
}

func rawWorktreeConfigEnabled(entries []controllerGitConfigEntry) (bool, error) {
	enabled := false
	for _, entry := range entries {
		if entry.key != "extensions.worktreeconfig" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(entry.value)) {
		case "true", "yes", "on", "1":
			enabled = true
		case "false", "no", "off", "0":
			enabled = false
		default:
			return false, fmt.Errorf("repository Git config has invalid extensions.worktreeConfig value")
		}
	}
	return enabled, nil
}

func unsafeGitExecutionConfig(key, value string) bool {
	lowerValue := strings.ToLower(strings.TrimSpace(value))
	if key == "include.path" || strings.HasPrefix(key, "includeif.") || strings.HasPrefix(key, "credential.") ||
		key == "core.attributesfile" || key == "core.worktree" || key == "core.alternaterefscommand" || key == "core.hookspath" ||
		key == "core.askpass" || key == "core.sshcommand" || key == "core.gitproxy" || key == "core.editor" || key == "core.fsmonitor" ||
		key == "sequence.editor" || key == "interactive.difffilter" || key == "diff.external" || key == "extensions.partialclone" ||
		(strings.HasPrefix(key, "gpg.") && strings.HasSuffix(key, ".program")) || key == "gpg.program" {
		return true
	}
	if key == "core.bare" && lowerValue != "false" && lowerValue != "no" && lowerValue != "off" && lowerValue != "0" {
		return true
	}
	if strings.HasPrefix(key, "submodule.") && strings.HasSuffix(key, ".update") && strings.HasPrefix(strings.TrimSpace(value), "!") {
		return true
	}
	if strings.HasPrefix(key, "remote.") && (strings.HasSuffix(key, ".promisor") || strings.HasSuffix(key, ".partialclonefilter")) {
		return true
	}
	return (strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".smudge") || strings.HasSuffix(key, ".process"))) ||
		(strings.HasPrefix(key, "diff.") && (strings.HasSuffix(key, ".command") || strings.HasSuffix(key, ".textconv"))) ||
		(strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver"))
}

func unsafeGitTransportConfigKey(key string) bool {
	if key == "include.path" || strings.HasPrefix(key, "includeif.") || strings.HasPrefix(key, "url.") || strings.HasPrefix(key, "http.") ||
		strings.HasPrefix(key, "credential.") || strings.HasPrefix(key, "protocol.") || key == "core.sshcommand" || key == "core.gitproxy" || key == "core.askpass" {
		return true
	}
	if strings.HasPrefix(key, "remote.") {
		for _, suffix := range []string{".pushurl", ".proxy", ".proxyauthmethod", ".uploadpack", ".receivepack", ".vcs"} {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	return false
}

func localGitOperationCanExecuteRepositoryConfig(args []string) bool {
	switch localGitOperation(args) {
	case "add", "commit", "diff", "checkout", "switch", "reset", "merge", "update-index":
		return true
	default:
		return false
	}
}

func localGitOperation(args []string) string {
	for index := 0; index < len(args); index++ {
		if args[index] == "-c" && index+1 < len(args) {
			index++
			continue
		}
		return strings.ToLower(strings.TrimSpace(args[index]))
	}
	return ""
}

func safeEnvironment() []string {
	secret := regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY)`)
	executionAffecting := regexp.MustCompile(`(?i)^(NODE_OPTIONS|NODE_PATH|BASH_ENV|ENV|SHELLOPTS|CDPATH|GIT_.*|NPM_CONFIG_.*|COREPACK_.*|PNPM_HOME|YARN_.*)$`)
	result := []string{"GIT_TERMINAL_PROMPT=0"}
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if !secret.MatchString(name) && !executionAffecting.MatchString(name) && !strings.EqualFold(name, "GH_TOKEN") && !strings.EqualFold(name, "GITHUB_TOKEN") {
			result = append(result, pair)
		}
	}
	return result
}

func safeOutput(value string) string {
	value = regexp.MustCompile(`(?i)(github_pat_[A-Za-z0-9_]{10,}|gh[pousr]_[A-Za-z0-9]{10,}|sk-[A-Za-z0-9_-]{10,})`).ReplaceAllString(value, "[REDACTED]")
	value = strings.TrimSpace(value)
	if len(value) > 2048 {
		value = value[len(value)-2048:]
	}
	return value
}

func safeRepoName(repository string) string {
	return strings.ReplaceAll(strings.ToLower(repository), "/", "-")
}

func managedSetupCheckoutPath(storeDir, stateDir, repository string, prior Config, hasPrior bool) (string, error) {
	current := filepath.Join(storeDir, "checkout")
	if !hasPrior {
		return current, nil
	}
	if strings.TrimSpace(prior.StateDir) == "" || !sameFilesystemPath(prior.StateDir, stateDir) {
		return "", fmt.Errorf("durable setup state path does not match selected state directory %s", stateDir)
	}
	candidate := strings.TrimSpace(prior.CheckoutDir)
	legacy := filepath.Join(storeDir, "checkouts", safeRepoName(repository))
	if candidate == "" || (!sameFilesystemPath(candidate, current) && !sameFilesystemPath(candidate, legacy)) {
		return "", fmt.Errorf("durable setup checkout path is outside the supported bounded or legacy state layout")
	}
	return filepath.Clean(candidate), nil
}

func cloneCommands(values [][]string) [][]string {
	result := make([][]string, len(values))
	for index := range values {
		result[index] = append([]string(nil), values[index]...)
	}
	return result
}

func stageManagedPaths(ctx context.Context, checkout string, paths []string) error {
	stageable := make([]string, 0, len(paths))
	for _, relative := range paths {
		if _, err := os.Lstat(filepath.Join(checkout, filepath.FromSlash(relative))); err == nil {
			stageable = append(stageable, relative)
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		tracked, err := git(ctx, checkout, "ls-files", "--", relative)
		if err != nil {
			return err
		}
		if strings.TrimSpace(tracked) != "" {
			stageable = append(stageable, relative)
		}
	}
	if len(stageable) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, stageable...)
	_, err := git(ctx, checkout, args...)
	return err
}

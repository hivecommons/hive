package integrated

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	directBootstrapIntentSchema = "hive.direct-bootstrap-intent.v1"
	directBootstrapIntentFile   = "direct-bootstrap.json"
	directBootstrapPrepared     = "prepared"
	directBootstrapPushed       = "pushed"
	directBootstrapConfigSaved  = "config_saved"
)

type directBootstrapPersistedConfig Config

type directBootstrapIntent struct {
	SchemaVersion          string                         `json:"schema_version"`
	Phase                  string                         `json:"phase"`
	Repository             string                         `json:"repository"`
	RepositoryID           string                         `json:"repository_id"`
	DefaultBranch          string                         `json:"default_branch"`
	BaseSHA                string                         `json:"base_sha"`
	HeadSHA                string                         `json:"head_sha"`
	HeadTreeSHA            string                         `json:"head_tree_sha"`
	SeedCommits            int                            `json:"seed_commits"`
	ExpectedSeedSHA        string                         `json:"expected_seed_sha"`
	ReviewedBaselineDigest string                         `json:"reviewed_baseline_digest,omitempty"`
	RequestDigest          string                         `json:"request_digest"`
	ConfigDigest           string                         `json:"config_digest"`
	IntentDigest           string                         `json:"intent_digest"`
	Config                 directBootstrapPersistedConfig `json:"config"`
	CreatedAt              time.Time                      `json:"created_at"`
	UpdatedAt              time.Time                      `json:"updated_at"`
}

type directBootstrapAuditBinding struct {
	SchemaVersion            string                   `json:"schema_version"`
	IntentDigest             string                   `json:"intent_digest"`
	RequestDigest            string                   `json:"request_digest"`
	ConfigDigest             string                   `json:"config_digest"`
	RepositoryID             string                   `json:"repository_id"`
	DefaultBranch            string                   `json:"default_branch"`
	SeedCommits              int                      `json:"seed_commits"`
	ExpectedSeedSHA          string                   `json:"expected_seed_sha"`
	BaseSHA                  string                   `json:"base_sha"`
	HeadSHA                  string                   `json:"head_sha"`
	HeadTreeSHA              string                   `json:"head_tree_sha"`
	SetupPRNumber            int                      `json:"setup_pr_number"`
	ScreenshotContractDigest string                   `json:"screenshot_contract_digest,omitempty"`
	ReviewedBaselineDigest   string                   `json:"reviewed_baseline_digest,omitempty"`
	ReviewedBaselines        []SetupBaselineCandidate `json:"reviewed_baselines,omitempty"`
}

func newDirectBootstrapIntent(ctx context.Context, options SetupOptions, config Config, proof directBootstrapProof, checkout, headSHA string) (directBootstrapIntent, error) {
	headSHA = strings.ToLower(strings.TrimSpace(headSHA))
	config.SetupHeadSHA, config.SetupPRNumber, config.SetupPRURL = headSHA, 0, ""
	headTree, err := git(ctx, checkout, "rev-parse", "--verify", headSHA+"^{tree}")
	if err != nil {
		return directBootstrapIntent{}, fmt.Errorf("read direct-bootstrap candidate tree: %w", err)
	}
	requestDigest, err := directBootstrapRequestDigest(options)
	if err != nil {
		return directBootstrapIntent{}, err
	}
	configDigest, err := directBootstrapConfigDigest(config)
	if err != nil {
		return directBootstrapIntent{}, err
	}
	now := time.Now().UTC()
	intent := directBootstrapIntent{
		SchemaVersion: directBootstrapIntentSchema, Phase: directBootstrapPrepared,
		Repository: config.Repository, RepositoryID: config.RepositoryID, DefaultBranch: config.DefaultBranch,
		BaseSHA: strings.ToLower(proof.BaseSHA), HeadSHA: headSHA, HeadTreeSHA: strings.ToLower(strings.TrimSpace(headTree)), SeedCommits: proof.SeedCommits,
		ExpectedSeedSHA: strings.ToLower(strings.TrimSpace(options.ExpectedSeedSHA)), ReviewedBaselineDigest: strings.ToLower(strings.TrimSpace(options.ReviewedBaselineDigest)),
		RequestDigest: requestDigest, ConfigDigest: configDigest, Config: directBootstrapPersistedConfig(config), CreatedAt: now, UpdatedAt: now,
	}
	intent.IntentDigest, err = directBootstrapIntentDigest(intent)
	if err != nil {
		return directBootstrapIntent{}, err
	}
	if err := validateDirectBootstrapIntent(intent); err != nil {
		return directBootstrapIntent{}, err
	}
	return intent, nil
}

func directBootstrapRequestDigest(options SetupOptions) (string, error) {
	payload := struct {
		Repository             string        `json:"repository"`
		Coverage               Coverage      `json:"coverage"`
		Automation             Automation    `json:"automation"`
		Provider               string        `json:"provider"`
		ProviderCommand        string        `json:"provider_command"`
		ProviderArgs           []string      `json:"provider_args"`
		ExecutionMode          ExecutionMode `json:"execution_mode"`
		RunIntervalSeconds     int64         `json:"run_interval_seconds"`
		VisualHive             bool          `json:"visual_hive"`
		StateDir               string        `json:"state_dir"`
		Start                  bool          `json:"start"`
		DirectBootstrap        bool          `json:"direct_bootstrap"`
		AdoptReviewedBaselines bool          `json:"adopt_reviewed_baselines"`
		ExpectedSeedSHA        string        `json:"expected_seed_sha"`
		ReviewedBaselineDigest string        `json:"reviewed_baseline_digest"`
		VisualHiveCommand      string        `json:"visual_hive_command"`
		VisualHiveArgs         []string      `json:"visual_hive_args"`
		VisualHiveRepo         string        `json:"visual_hive_repo"`
		VisualHiveRef          string        `json:"visual_hive_ref"`
		MaxActiveIssues        int           `json:"max_active_issues"`
		MaxRepairAttempts      int           `json:"max_repair_attempts"`
	}{
		Repository: options.Repository, Coverage: options.Coverage, Automation: options.Automation,
		Provider: options.Provider, ProviderCommand: options.ProviderCommand, ProviderArgs: append([]string(nil), options.ProviderArgs...),
		ExecutionMode: normalizedExecutionMode(options.ExecutionMode), RunIntervalSeconds: int64(options.RunInterval / time.Second), VisualHive: options.VisualHive,
		StateDir: options.StateDir, Start: options.Start, DirectBootstrap: options.DirectBootstrap, AdoptReviewedBaselines: options.AdoptReviewedBaselines,
		ExpectedSeedSHA: strings.ToLower(strings.TrimSpace(options.ExpectedSeedSHA)), ReviewedBaselineDigest: strings.ToLower(strings.TrimSpace(options.ReviewedBaselineDigest)),
		VisualHiveCommand: options.VisualHiveCommand, VisualHiveArgs: append([]string(nil), options.VisualHiveArgs...), VisualHiveRepo: options.VisualHiveRepo,
		VisualHiveRef: strings.ToLower(strings.TrimSpace(options.VisualHiveRef)), MaxActiveIssues: options.MaxActiveIssues, MaxRepairAttempts: options.MaxRepairAttempts,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode direct-bootstrap request binding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func directBootstrapConfigDigest(config Config) (string, error) {
	config.SchemaVersion = ConfigSchema
	config.UpdatedAt = time.Time{}
	data, err := json.Marshal(directBootstrapPersistedConfig(config))
	if err != nil {
		return "", fmt.Errorf("encode direct-bootstrap config binding: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func directBootstrapIntentDigest(intent directBootstrapIntent) (string, error) {
	payload := struct {
		SchemaVersion          string `json:"schema_version"`
		Repository             string `json:"repository"`
		RepositoryID           string `json:"repository_id"`
		DefaultBranch          string `json:"default_branch"`
		BaseSHA                string `json:"base_sha"`
		HeadSHA                string `json:"head_sha"`
		HeadTreeSHA            string `json:"head_tree_sha"`
		SeedCommits            int    `json:"seed_commits"`
		ExpectedSeedSHA        string `json:"expected_seed_sha"`
		ReviewedBaselineDigest string `json:"reviewed_baseline_digest"`
		RequestDigest          string `json:"request_digest"`
		ConfigDigest           string `json:"config_digest"`
	}{
		SchemaVersion: intent.SchemaVersion, Repository: intent.Repository, RepositoryID: intent.RepositoryID, DefaultBranch: intent.DefaultBranch,
		BaseSHA: intent.BaseSHA, HeadSHA: intent.HeadSHA, HeadTreeSHA: intent.HeadTreeSHA, SeedCommits: intent.SeedCommits,
		ExpectedSeedSHA: intent.ExpectedSeedSHA, ReviewedBaselineDigest: intent.ReviewedBaselineDigest,
		RequestDigest: intent.RequestDigest, ConfigDigest: intent.ConfigDigest,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateDirectBootstrapIntent(intent directBootstrapIntent) error {
	config := Config(intent.Config)
	if intent.SchemaVersion != directBootstrapIntentSchema || (intent.Phase != directBootstrapPrepared && intent.Phase != directBootstrapPushed && intent.Phase != directBootstrapConfigSaved) ||
		!regexpRepository.MatchString(intent.Repository) || strings.TrimSpace(intent.RepositoryID) == "" || intent.DefaultBranch == "" || intent.SeedCommits != 1 ||
		!immutableCommit.MatchString(intent.BaseSHA) || !immutableCommit.MatchString(intent.HeadSHA) || !immutableCommit.MatchString(intent.HeadTreeSHA) || intent.BaseSHA == intent.HeadSHA ||
		intent.ExpectedSeedSHA != intent.BaseSHA || !sha256DigestPattern.MatchString(intent.RequestDigest) || !sha256DigestPattern.MatchString(intent.ConfigDigest) || !sha256DigestPattern.MatchString(intent.IntentDigest) ||
		intent.CreatedAt.IsZero() || intent.UpdatedAt.IsZero() || intent.UpdatedAt.Before(intent.CreatedAt) {
		return fmt.Errorf("direct-bootstrap intent identity is invalid")
	}
	if config.Repository != intent.Repository || config.RepositoryID != intent.RepositoryID || config.DefaultBranch != intent.DefaultBranch ||
		config.SetupBranch != intent.DefaultBranch || config.SetupPRNumber != 0 || config.SetupPRURL != "" || config.SetupHeadSHA != intent.HeadSHA ||
		normalizedExecutionMode(config.ExecutionMode) != ExecutionLocal || config.Automation != AutomationRepairPR || config.SetupBaselineRequired || !config.VisualHive {
		return fmt.Errorf("direct-bootstrap intent config violates the no-PR local repair boundary")
	}
	computedConfig, err := directBootstrapConfigDigest(config)
	if err != nil || computedConfig != intent.ConfigDigest {
		return fmt.Errorf("direct-bootstrap intent config digest is invalid")
	}
	computedIntent, err := directBootstrapIntentDigest(intent)
	if err != nil || computedIntent != intent.IntentDigest {
		return fmt.Errorf("direct-bootstrap intent digest is invalid")
	}
	baselineDigest := ""
	if len(config.SetupBaselineInitialCandidates) > 0 {
		baselineDigest, err = setupBaselineCandidateDigest(config.SetupBaselineInitialCandidates)
		if err != nil || baselineDigest != config.SetupBaselineInitialDigest || baselineDigest != intent.ReviewedBaselineDigest || config.SetupBaselineContractDigest == "" {
			return fmt.Errorf("direct-bootstrap intent reviewed baseline binding is invalid")
		}
	} else if config.SetupBaselineInitialDigest != "" || config.SetupBaselineContractDigest != "" || intent.ReviewedBaselineDigest != "" {
		return fmt.Errorf("direct-bootstrap intent contains an incomplete reviewed baseline binding")
	}
	return nil
}

// Local copies avoid coupling the recovery schema to permissive UI parsing.
var (
	regexpRepository    = mustCompileDirectBootstrap(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	sha256DigestPattern = mustCompileDirectBootstrap(`^[a-f0-9]{64}$`)
)

func mustCompileDirectBootstrap(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

func (store *Store) saveDirectBootstrapIntent(intent directBootstrapIntent) error {
	intent.UpdatedAt = time.Now().UTC()
	if intent.CreatedAt.IsZero() {
		intent.CreatedAt = intent.UpdatedAt
	}
	if err := validateDirectBootstrapIntent(intent); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	return writeDurableStateFile(filepath.Join(store.dir, directBootstrapIntentFile), append(data, '\n'))
}

func (store *Store) loadDirectBootstrapIntent() (directBootstrapIntent, bool, error) {
	data, err := readOrdinaryStateFile(filepath.Join(store.dir, directBootstrapIntentFile))
	if errors.Is(err, os.ErrNotExist) {
		return directBootstrapIntent{}, false, nil
	}
	if err != nil {
		return directBootstrapIntent{}, false, err
	}
	var intent directBootstrapIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return directBootstrapIntent{}, false, fmt.Errorf("decode direct-bootstrap intent: %w", err)
	}
	if err := validateDirectBootstrapIntent(intent); err != nil {
		return directBootstrapIntent{}, false, err
	}
	return intent, true, nil
}

func (store *Store) deleteDirectBootstrapIntent() error {
	path := filepath.Join(store.dir, directBootstrapIntentFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncStateParentDirectory(path)
}

func recoverDirectBootstrap(ctx context.Context, options SetupOptions, store *Store, prior Config, hasPrior bool, intent directBootstrapIntent) (SetupResult, error) {
	config := Config(intent.Config)
	plan := buildSetupPlan(options, RepositoryInspection{DefaultBranch: config.DefaultBranch, RepositoryID: config.RepositoryID})
	result := SetupResult{Plan: plan, ProtectionActivationPending: false, ActivationMessage: setupActivationMessage(options.Automation, false, false)}
	return resumeDirectBootstrap(ctx, options, store, prior, hasPrior, intent, result)
}

func resumeDirectBootstrap(ctx context.Context, options SetupOptions, store *Store, prior Config, hasPrior bool, intent directBootstrapIntent, result SetupResult) (SetupResult, error) {
	if err := validateDirectBootstrapIntent(intent); err != nil {
		return result, err
	}
	requestDigest, err := directBootstrapRequestDigest(options)
	if err != nil || requestDigest != intent.RequestDigest || !strings.EqualFold(options.ExpectedSeedSHA, intent.ExpectedSeedSHA) || !strings.EqualFold(options.ReviewedBaselineDigest, intent.ReviewedBaselineDigest) {
		return result, fmt.Errorf("direct-bootstrap retry does not match the exact reviewed request binding")
	}
	config := Config(intent.Config)
	if filepath.Clean(config.StateDir) != filepath.Clean(options.StateDir) || !strings.EqualFold(config.Repository, options.Repository) {
		return result, fmt.Errorf("direct-bootstrap retry state/repository binding changed")
	}
	if options.GitHub == nil || options.GitHub.GoGitHub() == nil {
		return result, fmt.Errorf("GitHub client is required to recover direct bootstrap")
	}
	actor, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil || actor.ID != config.SetupAuthorizationActorID {
		return result, fmt.Errorf("direct-bootstrap retry requires the exact setup authorizer ID %d: %w", config.SetupAuthorizationActorID, err)
	}
	if err := VerifyVisualHiveWorkflowCommits(ctx, options.GitHub, config.VisualHiveRepo, config.VisualHiveRef); err != nil {
		return result, err
	}
	if hasPrior {
		priorDigest, digestErr := directBootstrapConfigDigest(prior)
		if digestErr != nil || priorDigest != intent.ConfigDigest {
			return result, fmt.Errorf("durable config changed during direct-bootstrap recovery")
		}
	}
	remoteHead, err := inspectDirectBootstrapRecoveryTarget(ctx, options.GitHub, intent)
	if err != nil {
		return result, err
	}
	detail, err := directBootstrapAuditDetail(intent)
	if err != nil {
		return result, err
	}
	wasAlreadyPushed := strings.EqualFold(remoteHead, intent.HeadSHA)
	authorized, err := directBootstrapAuditRecorded(store, "direct_bootstrap_authorized", config.Repository, detail)
	if err != nil {
		return result, err
	}
	if strings.EqualFold(remoteHead, intent.BaseSHA) {
		if intent.Phase != directBootstrapPrepared {
			return result, fmt.Errorf("direct-bootstrap remote returned to its seed after a durable pushed phase")
		}
		if err := authorizeSetup(store, options.Policy, options.Repository, automation.ActionSetupPush); err != nil {
			return result, err
		}
		if !authorized {
			if err := store.AuditStrict(AuditEntry{Action: "direct_bootstrap_authorized", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
				return result, fmt.Errorf("persist direct-bootstrap authorization audit: %w", err)
			}
		}
		if err := pushDirectBootstrapDefaultBranch(ctx, config.CheckoutDir, config.Repository, config.DefaultBranch, intent.BaseSHA, intent.HeadSHA); err != nil {
			return result, err
		}
		remoteHead, err = inspectDirectBootstrapRecoveryTarget(ctx, options.GitHub, intent)
		if err != nil || !strings.EqualFold(remoteHead, intent.HeadSHA) {
			return result, fmt.Errorf("reinspect direct-bootstrap exact pushed head: %w", err)
		}
	} else if !authorized {
		return result, fmt.Errorf("direct-bootstrap head is already published but its strict pre-push authorization audit is absent")
	}
	if intent.Phase == directBootstrapPrepared {
		intent.Phase = directBootstrapPushed
		if err := store.saveDirectBootstrapIntent(intent); err != nil {
			return result, fmt.Errorf("persist direct-bootstrap pushed phase: %w", err)
		}
	}
	if err := VerifyInstalledSetupAtCommit(ctx, options.GitHub, config, intent.HeadSHA); err != nil {
		return result, fmt.Errorf("verify direct-bootstrap managed files at exact installed head: %w", err)
	}
	if err := store.Save(config); err != nil {
		return result, fmt.Errorf("persist direct-bootstrap installed configuration: %w", err)
	}
	if intent.Phase != directBootstrapConfigSaved {
		intent.Phase = directBootstrapConfigSaved
		if err := store.saveDirectBootstrapIntent(intent); err != nil {
			return result, fmt.Errorf("persist direct-bootstrap config-saved phase: %w", err)
		}
	}
	completed, err := directBootstrapAuditRecorded(store, "direct_bootstrap_completed", config.Repository, detail)
	if err != nil {
		return result, err
	}
	if !completed {
		if err := store.AuditStrict(AuditEntry{Action: "direct_bootstrap_completed", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return result, fmt.Errorf("persist direct-bootstrap completion audit: %w", err)
		}
	}
	if err := store.deleteDirectBootstrapIntent(); err != nil {
		return result, fmt.Errorf("retire completed direct-bootstrap intent: %w", err)
	}
	result.Applied, result.Idempotent, result.Config = true, wasAlreadyPushed, publicSetupConfig(config)
	result.Branch, result.CommitSHA, result.PRNumber, result.PRURL = "", intent.HeadSHA, 0, ""
	return result, nil
}

func inspectDirectBootstrapRecoveryTarget(ctx context.Context, client *hivegithub.Client, intent directBootstrapIntent) (string, error) {
	config := Config(intent.Config)
	exists, err := validateManagedCheckoutBeforeGit(config.CheckoutDir, config.Repository)
	if err != nil || !exists {
		return "", fmt.Errorf("direct-bootstrap recovery checkout ownership is invalid: %w", err)
	}
	if err := validateOrdinarySetupCheckout(config.CheckoutDir); err != nil {
		return "", err
	}
	owner, name, _ := strings.Cut(config.Repository, "/")
	metadata, _, err := client.GoGitHub().Repositories.Get(ctx, owner, name)
	if err != nil || metadata.GetID() <= 0 || strconv.FormatInt(metadata.GetID(), 10) != config.RepositoryID || !strings.EqualFold(metadata.GetFullName(), config.Repository) || metadata.GetDefaultBranch() != config.DefaultBranch || metadata.Private == nil || !metadata.GetPrivate() || metadata.GetFork() || metadata.GetArchived() || metadata.GetDisabled() || !metadata.GetPermissions()["push"] {
		return "", fmt.Errorf("direct-bootstrap recovery repository identity/private disposable shape changed: %w", err)
	}
	if err := validateDirectBootstrapRemoteBinding(ctx, config.CheckoutDir, config.Repository, config.DefaultBranch); err != nil {
		return "", err
	}
	remoteURL := RepositoryCloneURL(config.Repository)
	refspec := "+refs/heads/" + config.DefaultBranch + ":refs/remotes/origin/" + config.DefaultBranch
	if _, err := gitTransport(ctx, config.CheckoutDir, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return "", fmt.Errorf("refresh direct-bootstrap recovery head: %w", err)
	}
	remoteHead, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", "refs/remotes/origin/"+config.DefaultBranch)
	if err != nil {
		return "", err
	}
	remoteHead = strings.ToLower(strings.TrimSpace(remoteHead))
	if remoteHead != intent.BaseSHA && remoteHead != intent.HeadSHA {
		return "", fmt.Errorf("direct-bootstrap recovery default head drifted to %s", remoteHead)
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, name, config.DefaultBranch, 0)
	if err != nil || !strings.EqualFold(branch.GetCommit().GetSHA(), remoteHead) {
		return "", fmt.Errorf("direct-bootstrap recovery API/default head mismatch: %w", err)
	}
	pulls, response, err := client.GoGitHub().PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil || len(pulls) != 0 || (response != nil && response.NextPage != 0) {
		return "", fmt.Errorf("direct-bootstrap recovery requires no open pull requests: %w", err)
	}
	protection, err := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
	if err != nil || (protection.Enabled && remoteHead == intent.BaseSHA) {
		return "", fmt.Errorf("direct-bootstrap recovery refuses changed default-branch protection/rules: %w", err)
	}
	parents, err := git(ctx, config.CheckoutDir, "rev-list", "--parents", "-n", "1", intent.HeadSHA)
	fields := strings.Fields(parents)
	if err != nil || len(fields) != 2 || fields[0] != intent.HeadSHA || fields[1] != intent.BaseSHA {
		return "", fmt.Errorf("direct-bootstrap candidate commit/parent is unavailable or changed: %w", err)
	}
	tree, err := git(ctx, config.CheckoutDir, "rev-parse", "--verify", intent.HeadSHA+"^{tree}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(tree), intent.HeadTreeSHA) {
		return "", fmt.Errorf("direct-bootstrap candidate tree changed or is unavailable: %w", err)
	}
	count, err := git(ctx, config.CheckoutDir, "rev-list", "--count", remoteHead)
	wantCount := "1"
	if remoteHead == intent.HeadSHA {
		wantCount = "2"
	}
	if err != nil || strings.TrimSpace(count) != wantCount {
		return "", fmt.Errorf("direct-bootstrap recovery history shape changed: %w", err)
	}
	return remoteHead, nil
}

func directBootstrapAuditDetail(intent directBootstrapIntent) (string, error) {
	config := Config(intent.Config)
	payload := directBootstrapAuditBinding{
		SchemaVersion: "hive.direct-bootstrap-audit.v1", IntentDigest: intent.IntentDigest, RequestDigest: intent.RequestDigest, ConfigDigest: intent.ConfigDigest,
		RepositoryID: intent.RepositoryID, DefaultBranch: intent.DefaultBranch, SeedCommits: intent.SeedCommits,
		ExpectedSeedSHA: intent.ExpectedSeedSHA, BaseSHA: intent.BaseSHA, HeadSHA: intent.HeadSHA, HeadTreeSHA: intent.HeadTreeSHA, SetupPRNumber: 0,
		ScreenshotContractDigest: config.SetupBaselineContractDigest, ReviewedBaselineDigest: intent.ReviewedBaselineDigest,
		ReviewedBaselines: append([]SetupBaselineCandidate(nil), config.SetupBaselineInitialCandidates...),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode direct-bootstrap audit binding: %w", err)
	}
	return string(data), nil
}

func directBootstrapAuditRecorded(store *Store, action, repository, detail string) (bool, error) {
	file, err := os.Open(store.auditPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var entry AuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return false, fmt.Errorf("direct-bootstrap audit ledger contains invalid JSON: %w", err)
		}
		if entry.Allowed && entry.Action == action && strings.EqualFold(entry.Repository, repository) && entry.Detail == detail {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func replayCompletedDirectBootstrap(ctx context.Context, options SetupOptions, store *Store, prior Config) (SetupResult, bool, error) {
	if prior.SetupPRNumber != 0 || prior.SetupPRURL != "" || prior.SetupBranch != prior.DefaultBranch || !immutableCommit.MatchString(prior.SetupHeadSHA) ||
		normalizedExecutionMode(prior.ExecutionMode) != ExecutionLocal || prior.Automation != AutomationRepairPR || !prior.VisualHive {
		return SetupResult{}, false, nil
	}
	requestDigest, err := directBootstrapRequestDigest(options)
	if err != nil {
		return SetupResult{}, true, err
	}
	configDigest, err := directBootstrapConfigDigest(prior)
	if err != nil {
		return SetupResult{}, true, err
	}
	binding, found, err := findCompletedDirectBootstrapAudit(store, prior.Repository, requestDigest, configDigest, prior.SetupHeadSHA)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("exact completed direct-bootstrap audit receipt is absent")
		}
		return SetupResult{}, true, err
	}
	if err := validateCompletedDirectBootstrapAuditBinding(binding, prior); err != nil {
		return SetupResult{}, true, err
	}
	if !strings.EqualFold(binding.ExpectedSeedSHA, options.ExpectedSeedSHA) || !strings.EqualFold(binding.ReviewedBaselineDigest, options.ReviewedBaselineDigest) {
		return SetupResult{}, true, fmt.Errorf("completed direct-bootstrap review binding changed")
	}
	now := time.Now().UTC()
	intent := directBootstrapIntent{
		SchemaVersion: directBootstrapIntentSchema, Phase: directBootstrapConfigSaved,
		Repository: prior.Repository, RepositoryID: prior.RepositoryID, DefaultBranch: prior.DefaultBranch,
		BaseSHA: binding.BaseSHA, HeadSHA: binding.HeadSHA, HeadTreeSHA: binding.HeadTreeSHA, SeedCommits: binding.SeedCommits,
		ExpectedSeedSHA: binding.ExpectedSeedSHA, ReviewedBaselineDigest: binding.ReviewedBaselineDigest,
		RequestDigest: binding.RequestDigest, ConfigDigest: binding.ConfigDigest, IntentDigest: binding.IntentDigest,
		Config: directBootstrapPersistedConfig(prior), CreatedAt: now, UpdatedAt: now,
	}
	if err := validateDirectBootstrapIntent(intent); err != nil {
		return SetupResult{}, true, err
	}
	actor, err := options.GitHub.AuthenticatedNumericUser(ctx)
	if err != nil || actor.ID != prior.SetupAuthorizationActorID {
		return SetupResult{}, true, fmt.Errorf("completed direct-bootstrap replay requires setup authorizer ID %d: %w", prior.SetupAuthorizationActorID, err)
	}
	remoteHead, err := inspectDirectBootstrapRecoveryTarget(ctx, options.GitHub, intent)
	if err != nil || remoteHead != intent.HeadSHA {
		return SetupResult{}, true, fmt.Errorf("completed direct-bootstrap head drifted: %w", err)
	}
	if err := VerifyInstalledSetupAtCommit(ctx, options.GitHub, prior, prior.SetupHeadSHA); err != nil {
		return SetupResult{}, true, err
	}
	result := SetupResult{Plan: buildSetupPlan(options, RepositoryInspection{DefaultBranch: prior.DefaultBranch, RepositoryID: prior.RepositoryID})}
	result.Applied, result.Idempotent, result.Config = true, true, publicSetupConfig(prior)
	result.CommitSHA = prior.SetupHeadSHA
	return result, true, nil
}

func validateCompletedDirectBootstrapAuditBinding(binding directBootstrapAuditBinding, config Config) error {
	if binding.SchemaVersion != "hive.direct-bootstrap-audit.v1" ||
		binding.RepositoryID != config.RepositoryID || binding.DefaultBranch != config.DefaultBranch || binding.SetupPRNumber != 0 ||
		!strings.EqualFold(binding.ScreenshotContractDigest, config.SetupBaselineContractDigest) ||
		!strings.EqualFold(binding.ReviewedBaselineDigest, config.SetupBaselineInitialDigest) ||
		!equalSetupBaselineCandidates(binding.ReviewedBaselines, config.SetupBaselineInitialCandidates) {
		return fmt.Errorf("completed direct-bootstrap audit receipt does not match the exact durable reviewed-baseline binding")
	}
	return nil
}

func findCompletedDirectBootstrapAudit(store *Store, repository, requestDigest, configDigest, headSHA string) (directBootstrapAuditBinding, bool, error) {
	file, err := os.Open(store.auditPath)
	if err != nil {
		return directBootstrapAuditBinding{}, false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var found directBootstrapAuditBinding
	for scanner.Scan() {
		var entry AuditEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return directBootstrapAuditBinding{}, false, err
		}
		if !entry.Allowed || entry.Action != "direct_bootstrap_completed" || !strings.EqualFold(entry.Repository, repository) {
			continue
		}
		var binding directBootstrapAuditBinding
		if err := json.Unmarshal([]byte(entry.Detail), &binding); err != nil {
			return directBootstrapAuditBinding{}, false, fmt.Errorf("decode completed direct-bootstrap audit binding: %w", err)
		}
		if binding.RequestDigest == requestDigest && binding.ConfigDigest == configDigest && binding.HeadSHA == strings.ToLower(headSHA) {
			found = binding
		}
	}
	if err := scanner.Err(); err != nil {
		return directBootstrapAuditBinding{}, false, err
	}
	return found, found.IntentDigest != "", nil
}

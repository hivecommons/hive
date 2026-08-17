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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/internal/gittransport"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const orphanSetupResetPlanSchema = "hive.orphaned-setup-reset-plan.v1"

// OrphanSetupResetOptions binds recovery to the repository already selected by
// the authenticated dashboard. StateDir is a temporary durable recovery area;
// it is never treated as an installed Visual Hive controller contract.
type OrphanSetupResetOptions struct {
	Repository           string
	StateDir             string
	GitHub               *hivegithub.Client
	GitTransportToken    string
	AuthorizationActor   hivegithub.AuthenticatedUserIdentity
	RuntimeBindingDigest string
}

// OrphanSetupResetPlan is a non-mutating proof that one exact merged setup can
// be safely restored to its pre-setup tree through Hive's normal uninstall PR.
type OrphanSetupResetPlan struct {
	SchemaVersion        string                    `json:"schema_version"`
	Repository           string                    `json:"repository"`
	RepositoryID         string                    `json:"repository_id"`
	DefaultBranch        string                    `json:"default_branch"`
	CurrentHeadSHA       string                    `json:"current_head_sha"`
	SetupPRNumber        int                       `json:"setup_pr_number"`
	SetupPRURL           string                    `json:"setup_pr_url"`
	SetupBranch          string                    `json:"setup_branch"`
	SetupBaseSHA         string                    `json:"setup_base_sha"`
	SetupHeadSHA         string                    `json:"setup_head_sha"`
	SetupMergeSHA        string                    `json:"setup_merge_sha"`
	SetupMergedAt        time.Time                 `json:"setup_merged_at"`
	SetupAuthorizerID    int64                     `json:"setup_authorizer_id"`
	OperatorID           int64                     `json:"operator_id,omitempty"`
	WriterID             int64                     `json:"writer_id,omitempty"`
	RuntimeBindingDigest string                    `json:"runtime_binding_digest,omitempty"`
	ManagedPaths         []OrphanSetupPathBinding  `json:"managed_paths"`
	RecoveryAvailable    bool                      `json:"recovery_available"`
	PlanSHA256           string                    `json:"plan_sha256"`
	Installed            installedRepositoryConfig `json:"-"`
}

// OrphanSetupPathBinding proves both that the current managed byte is exactly
// the reviewed setup byte and what Git blob must be restored from the setup
// PR's immutable base.
type OrphanSetupPathBinding struct {
	Path                  string `json:"path"`
	CurrentContentSHA256  string `json:"current_content_sha256,omitempty"`
	SetupContentSHA256    string `json:"setup_content_sha256,omitempty"`
	PreimageExists        bool   `json:"preimage_exists"`
	PreimageContentSHA256 string `json:"preimage_content_sha256,omitempty"`
}

// PlanOrphanedSetupReset performs no GitHub mutation and does not persist
// reconstructed state. It refuses any lifecycle newer than the exact merged
// setup because approval history cannot be reconstructed safely.
func PlanOrphanedSetupReset(ctx context.Context, options OrphanSetupResetOptions) (OrphanSetupResetPlan, error) {
	plan := OrphanSetupResetPlan{SchemaVersion: orphanSetupResetPlanSchema}
	if options.GitHub == nil || options.GitHub.GoGitHub() == nil || strings.TrimSpace(options.StateDir) == "" {
		return plan, fmt.Errorf("GitHub client and dedicated recovery state directory are required")
	}
	owner, name, ok := strings.Cut(strings.TrimSpace(options.Repository), "/")
	if !ok || owner == "" || name == "" {
		return plan, fmt.Errorf("orphaned setup recovery requires one exact owner/repository")
	}
	repository, _, err := options.GitHub.GoGitHub().Repositories.Get(ctx, owner, name)
	if err != nil {
		return plan, fmt.Errorf("read orphaned setup repository: %w", err)
	}
	if repository.GetID() <= 0 || !strings.EqualFold(repository.GetFullName(), options.Repository) || repository.GetDefaultBranch() == "" {
		return plan, fmt.Errorf("orphaned setup repository identity is incomplete or mismatched")
	}
	plan.Repository, plan.RepositoryID = repository.GetFullName(), fmt.Sprint(repository.GetID())
	plan.DefaultBranch = repository.GetDefaultBranch()
	branch, _, err := options.GitHub.GoGitHub().Repositories.GetBranch(ctx, owner, name, plan.DefaultBranch, 3)
	if err != nil || branch == nil || !immutableCommit.MatchString(strings.ToLower(branch.GetCommit().GetSHA())) {
		return plan, fmt.Errorf("read exact default-branch head before orphaned setup recovery: %w", err)
	}
	plan.CurrentHeadSHA = strings.ToLower(branch.GetCommit().GetSHA())

	installedBytes, exists, err := readRepositoryFileAtRef(ctx, options.GitHub, owner, name, ".hive/integrated.json", plan.CurrentHeadSHA)
	if err != nil || !exists {
		return plan, fmt.Errorf("read installed repository contract before orphaned setup recovery: %w", err)
	}
	if err := json.Unmarshal(installedBytes, &plan.Installed); err != nil || plan.Installed.SchemaVersion != managedRepositoryConfigSchema {
		return plan, fmt.Errorf("installed repository contract is invalid or unsupported")
	}
	if !strings.EqualFold(plan.Installed.Repository, plan.Repository) || plan.Installed.RepositoryID != plan.RepositoryID ||
		plan.Installed.DefaultBranch != plan.DefaultBranch || !plan.Installed.VisualHive || plan.Installed.SetupAuthorizationActorID <= 0 {
		return plan, fmt.Errorf("installed repository contract does not bind the exact repository or setup authorizer")
	}
	plan.SetupAuthorizerID = plan.Installed.SetupAuthorizationActorID
	if options.AuthorizationActor.ID > 0 || strings.TrimSpace(options.AuthorizationActor.Login) != "" {
		installedConfig := orphanInstalledAuthorizationConfig(plan.Installed)
		operator, operatorErr := resolveManagedOperatorIdentity(ctx, options.GitHub, installedConfig, options.AuthorizationActor, "setup-reset")
		if operatorErr != nil {
			return plan, operatorErr
		}
		writer, writerOK := setupAuthorizationWriter(installedConfig)
		if !writerOK || writer.ID <= 0 || !setupIdentityDigest.MatchString(strings.ToLower(strings.TrimSpace(options.RuntimeBindingDigest))) {
			return plan, fmt.Errorf("orphaned setup recovery requires an exact App writer/runtime binding")
		}
		plan.OperatorID, plan.WriterID, plan.RuntimeBindingDigest = operator.ID, writer.ID, strings.ToLower(strings.TrimSpace(options.RuntimeBindingDigest))
	}
	plan.SetupBranch = managedOperationBranch("setup", plan.RepositoryID)
	pull, err := exactMergedSetupAtHead(ctx, options.GitHub, owner, name, plan)
	if err != nil {
		return plan, err
	}
	plan.SetupPRNumber, plan.SetupPRURL = pull.GetNumber(), pull.GetHTMLURL()
	plan.SetupBaseSHA = strings.ToLower(pull.GetBase().GetSHA())
	plan.SetupHeadSHA = strings.ToLower(pull.GetHead().GetSHA())
	plan.SetupMergeSHA = strings.ToLower(pull.GetMergeCommitSHA())
	plan.SetupMergedAt = pull.GetMergedAt().Time.UTC()

	managed := managedSetupFilesForMode(true, plan.Installed.ExecutionMode)
	changed, err := listExactPullRequestFiles(ctx, options.GitHub, owner, name, pull.GetNumber())
	if err != nil {
		return plan, err
	}
	if err := validateOrphanedSetupChangedFiles(changed, managed); err != nil {
		return plan, err
	}
	if err := refusePostSetupLifecycle(ctx, options.GitHub, owner, name, plan); err != nil {
		return plan, err
	}

	bindings := make([]OrphanSetupPathBinding, 0, len(managed))
	for _, relative := range managed {
		current, currentExists, readErr := readRepositoryFileAtRef(ctx, options.GitHub, owner, name, relative, plan.CurrentHeadSHA)
		if readErr != nil {
			return plan, readErr
		}
		setup, setupExists, readErr := readRepositoryFileAtRef(ctx, options.GitHub, owner, name, relative, plan.SetupHeadSHA)
		if readErr != nil {
			return plan, readErr
		}
		if currentExists != setupExists || !bytes.Equal(current, setup) {
			return plan, fmt.Errorf("managed path %s no longer matches the exact merged setup head", relative)
		}
		preimage, preimageExists, readErr := readRepositoryFileAtRef(ctx, options.GitHub, owner, name, relative, plan.SetupBaseSHA)
		if readErr != nil {
			return plan, readErr
		}
		binding := OrphanSetupPathBinding{Path: relative, PreimageExists: preimageExists}
		if currentExists {
			binding.CurrentContentSHA256 = sha256Hex(current)
			binding.SetupContentSHA256 = sha256Hex(setup)
		}
		if preimageExists {
			binding.PreimageContentSHA256 = sha256Hex(preimage)
		}
		bindings = append(bindings, binding)
	}
	plan.ManagedPaths = bindings
	plan.RecoveryAvailable = true
	plan.PlanSHA256, err = orphanSetupResetPlanDigest(plan)
	if err != nil {
		return plan, err
	}
	return plan, nil
}

func orphanInstalledAuthorizationConfig(installed installedRepositoryConfig) Config {
	return Config{
		Repository:                         installed.Repository,
		SetupAuthorizationActorID:          installed.SetupAuthorizationActorID,
		SetupAuthorizationActorLogin:       installed.SetupAuthorizationActorLogin,
		SetupAuthorizationWriterID:         installed.SetupAuthorizationWriterID,
		SetupAuthorizationWriterLogin:      installed.SetupAuthorizationWriterLogin,
		SetupAuthorizationWriterType:       installed.SetupAuthorizationWriterType,
		SetupAuthorizationAppID:            installed.SetupAuthorizationAppID,
		SetupAuthorizationInstallationID:   installed.SetupAuthorizationInstallationID,
		SetupAuthorizationPermissionDigest: installed.SetupAuthorizationPermissionDigest,
		SetupAuthorizationAppBindingDigest: installed.SetupAuthorizationAppBindingDigest,
	}
}

// PrepareOrphanedSetupReset rechecks the exact plan, reconstructs only the
// missing preimage/config state, and delegates the GitHub mutation to the
// existing managed two-phase uninstall implementation.
func PrepareOrphanedSetupReset(ctx context.Context, options OrphanSetupResetOptions, expectedPlanSHA256 string) (ManagementResult, error) {
	result := ManagementResult{SchemaVersion: "hive.management.v1", Operation: OperationUninstall}
	plan, err := PlanOrphanedSetupReset(ctx, options)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(strings.TrimSpace(expectedPlanSHA256), plan.PlanSHA256) {
		return result, fmt.Errorf("orphaned setup recovery plan changed: expected %s, current %s", expectedPlanSHA256, plan.PlanSHA256)
	}
	if entries, readErr := os.ReadDir(options.StateDir); readErr == nil && len(entries) != 0 {
		store, storeErr := NewStore(filepath.Join(options.StateDir, "integrated"))
		if storeErr != nil {
			return result, storeErr
		}
		existing, loadErr := store.Load()
		if loadErr != nil || !orphanRecoveryConfigMatchesPlan(existing, plan, options.StateDir) {
			return result, fmt.Errorf("dedicated orphaned setup recovery state is nonempty and not the exact resumable plan")
		}
		return RunManagement(ctx, ManagementOptions{
			Operation: OperationUninstall, StateDir: options.StateDir, GitHub: options.GitHub, GitTransportToken: options.GitTransportToken,
			AuthorizationActor: options.AuthorizationActor,
		})
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return result, fmt.Errorf("inspect orphaned setup recovery state: %w", readErr)
	}
	if err := os.MkdirAll(filepath.Dir(options.StateDir), 0o700); err != nil {
		return result, fmt.Errorf("create orphaned setup recovery parent: %w", err)
	}
	preparationRoot, err := os.MkdirTemp(filepath.Dir(options.StateDir), ".hive-orphaned-setup-reset-*")
	if err != nil {
		return result, fmt.Errorf("create isolated orphaned setup preparation directory: %w", err)
	}
	defer os.RemoveAll(preparationRoot)
	preparationCheckout := orphanSetupResetCheckoutDir(preparationRoot)
	checkout := orphanSetupResetCheckoutDir(options.StateDir)
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	defaultBranch, err := ensureCheckout(ctx, plan.Repository, preparationCheckout)
	if err != nil {
		return result, err
	}
	if defaultBranch != plan.DefaultBranch {
		return result, fmt.Errorf("default branch changed during orphaned setup recovery")
	}
	if _, err := git(ctx, preparationCheckout, "switch", "--detach", plan.SetupBaseSHA); err != nil {
		return result, fmt.Errorf("check out exact setup preimage: %w", err)
	}
	preimages, err := captureManagedPathPreimages(ctx, preparationCheckout, managedSetupFilesForMode(true, plan.Installed.ExecutionMode))
	if err != nil {
		return result, fmt.Errorf("capture exact setup preimages: %w", err)
	}
	config := configFromInstalledRepository(plan.Installed)
	config.SchemaVersion = ConfigSchema
	config.Repository, config.RepositoryID, config.DefaultBranch = plan.Repository, plan.RepositoryID, plan.DefaultBranch
	config.CheckoutDir, config.StateDir = checkout, options.StateDir
	config.SetupBranch, config.SetupPRNumber, config.SetupPRURL, config.SetupHeadSHA = plan.SetupBranch, plan.SetupPRNumber, plan.SetupPRURL, plan.SetupHeadSHA
	config.ManagedPreimagesVersion, config.ManagedPathPreimages = managedPreimagesVersion, preimages
	config.InstalledAt, config.UpdatedAt = plan.SetupMergedAt, time.Now().UTC()
	if !orphanRecoveryConfigMatchesPlan(config, plan, options.StateDir) {
		return result, fmt.Errorf("cloned setup preimages do not match the exact orphaned recovery plan")
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	if err := store.Save(config); err != nil {
		return result, fmt.Errorf("persist bounded orphaned setup recovery state: %w", err)
	}
	return RunManagement(ctx, ManagementOptions{
		Operation: OperationUninstall, StateDir: options.StateDir, GitHub: options.GitHub, GitTransportToken: options.GitTransportToken,
		AuthorizationActor: options.AuthorizationActor,
	})
}

func orphanSetupResetCheckoutDir(stateDir string) string {
	return filepath.Join(stateDir, "integrated", "checkout")
}

func orphanRecoveryConfigMatchesPlan(config Config, plan OrphanSetupResetPlan, stateDir string) bool {
	if !strings.EqualFold(config.Repository, plan.Repository) || config.RepositoryID != plan.RepositoryID || config.DefaultBranch != plan.DefaultBranch ||
		filepath.Clean(config.StateDir) != filepath.Clean(stateDir) || config.SetupBranch != plan.SetupBranch || config.SetupPRNumber != plan.SetupPRNumber ||
		!strings.EqualFold(config.SetupHeadSHA, plan.SetupHeadSHA) || !hasValidManagedPathPreimages(config) || len(config.ManagedPathPreimages) != len(plan.ManagedPaths) {
		return false
	}
	for _, binding := range plan.ManagedPaths {
		preimage, exists := config.ManagedPathPreimages[binding.Path]
		if !exists || preimage.Existed != binding.PreimageExists {
			return false
		}
		if binding.PreimageExists && !strings.EqualFold(preimage.ContentSHA256, binding.PreimageContentSHA256) {
			return false
		}
	}
	return true
}

func configFromInstalledRepository(installed installedRepositoryConfig) Config {
	return Config{
		Repository: installed.Repository, RepositoryID: installed.RepositoryID, DefaultBranch: installed.DefaultBranch,
		Coverage: installed.Coverage, Automation: installed.Automation, Provider: installed.Provider,
		ExecutionMode: installed.ExecutionMode, RunIntervalSeconds: installed.RunIntervalSeconds,
		HostedSchedule: installed.HostedSchedule, HostedStateBranch: installed.HostedStateBranch,
		HiveReleaseRepository: installed.HiveReleaseRepository, HiveReleaseVersion: installed.HiveReleaseVersion,
		HiveCommit: installed.HiveCommit, DistributionManifestSHA256: installed.DistributionManifestSHA256,
		HostedControllerProtocol: installed.HostedControllerProtocol, HostedWorkflowSHA256: installed.HostedWorkflowSHA256,
		PreviousHostedRelease: cloneHostedReleaseIdentity(installed.PreviousHostedRelease),
		ACMMLevel:             installed.ACMMLevel, MaxActiveIssues: installed.MaxActiveIssues, MaxRepairAttempts: installed.MaxRepairAttempts,
		VisualHive: installed.VisualHive, VisualHiveRepo: installed.VisualHiveRepo, VisualHiveRef: installed.VisualHiveRef,
		VisualHiveConfigDigest: installed.VisualHiveConfigDigest, TestCommands: installed.TestCommands,
		AllowedRepairPaths: installed.AllowedRepairPaths, AllowedAutoMergePaths: installed.AllowedAutoMergePaths,
		AllowedAutoMergeRisk: installed.AllowedAutoMergeRisk, SetupAuthorizationActorID: installed.SetupAuthorizationActorID,
		SetupAuthorizationActorLogin: installed.SetupAuthorizationActorLogin,
		SetupAuthorizationWriterID:   installed.SetupAuthorizationWriterID, SetupAuthorizationWriterLogin: installed.SetupAuthorizationWriterLogin,
		SetupAuthorizationWriterType: installed.SetupAuthorizationWriterType, SetupAuthorizationAppID: installed.SetupAuthorizationAppID,
		SetupAuthorizationInstallationID:   installed.SetupAuthorizationInstallationID,
		SetupAuthorizationPermissionDigest: installed.SetupAuthorizationPermissionDigest,
		SetupAuthorizationAppBindingDigest: installed.SetupAuthorizationAppBindingDigest,
		SetupAuthorizationPreviousActorID:  installed.SetupAuthorizationPreviousActorID,
	}
}

func exactMergedSetupAtHead(ctx context.Context, client *hivegithub.Client, owner, name string, plan OrphanSetupResetPlan) (*gh.PullRequest, error) {
	options := &gh.PullRequestListOptions{State: "closed", Head: owner + ":" + plan.SetupBranch, Base: plan.DefaultBranch, ListOptions: gh.ListOptions{PerPage: 100}}
	var matched *gh.PullRequest
	for {
		pulls, response, err := client.GoGitHub().PullRequests.List(ctx, owner, name, options)
		if err != nil {
			return nil, fmt.Errorf("list setup pull requests for orphaned recovery: %w", err)
		}
		for _, listed := range pulls {
			pull, _, getErr := client.GoGitHub().PullRequests.Get(ctx, owner, name, listed.GetNumber())
			if getErr != nil {
				return nil, getErr
			}
			if pull.GetMerged() && strings.EqualFold(pull.GetMergeCommitSHA(), plan.CurrentHeadSHA) &&
				pull.GetHead().GetRef() == plan.SetupBranch && pull.GetBase().GetRef() == plan.DefaultBranch &&
				fmt.Sprint(pull.GetHead().GetRepo().GetID()) == plan.RepositoryID {
				if matched != nil {
					return nil, fmt.Errorf("multiple merged setup pull requests claim the current head")
				}
				matched = pull
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	if matched == nil || matched.GetMergedAt().IsZero() || !immutableCommit.MatchString(strings.ToLower(matched.GetBase().GetSHA())) ||
		!immutableCommit.MatchString(strings.ToLower(matched.GetHead().GetSHA())) {
		return nil, fmt.Errorf("the current default-branch head is not one unambiguous merged Hive setup pull request")
	}
	return matched, nil
}

func listExactPullRequestFiles(ctx context.Context, client *hivegithub.Client, owner, name string, number int) ([]string, error) {
	options := &gh.ListOptions{PerPage: 100}
	var result []string
	seen := map[string]bool{}
	for {
		files, response, err := client.GoGitHub().PullRequests.ListFiles(ctx, owner, name, number, options)
		if err != nil {
			return nil, fmt.Errorf("list exact setup pull request files: %w", err)
		}
		for _, file := range files {
			path := strings.TrimSpace(file.GetFilename())
			if path == "" || seen[path] {
				return nil, fmt.Errorf("setup pull request file inventory is invalid or duplicated")
			}
			seen[path], result = true, append(result, path)
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	sort.Strings(result)
	return result, nil
}

func validateOrphanedSetupChangedFiles(changed, managed []string) error {
	allowed := make(map[string]bool, len(managed))
	for _, relative := range managed {
		allowed[relative] = true
	}
	if len(changed) == 0 {
		return fmt.Errorf("merged setup pull request changed no files")
	}
	foundContract := false
	for _, relative := range changed {
		if !allowed[relative] {
			return fmt.Errorf("merged setup pull request changed unmanaged path %s", relative)
		}
		foundContract = foundContract || relative == ".hive/integrated.json"
	}
	if !foundContract {
		return fmt.Errorf("merged setup pull request omitted its managed repository contract")
	}
	return nil
}

func refusePostSetupLifecycle(ctx context.Context, client *hivegithub.Client, owner, name string, plan OrphanSetupResetPlan) error {
	_, directory, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, name, ".visual-hive/snapshots", &gh.RepositoryContentGetOptions{Ref: plan.CurrentHeadSHA})
	if err == nil && len(directory) != 0 {
		return fmt.Errorf("orphaned setup recovery refuses repositories with committed Visual Hive baselines")
	}
	if err != nil && !isGitHubNotFound(err) {
		return fmt.Errorf("inspect Visual Hive baselines before recovery: %w", err)
	}

	pullOptions := &gh.PullRequestListOptions{State: "all", Sort: "created", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		pulls, response, listErr := client.GoGitHub().PullRequests.List(ctx, owner, name, pullOptions)
		if listErr != nil {
			return fmt.Errorf("inspect lifecycle pull requests after orphaned setup: %w", listErr)
		}
		stop := false
		for _, pull := range pulls {
			if !pull.GetCreatedAt().After(plan.SetupMergedAt) {
				stop = true
				continue
			}
			head := pull.GetHead().GetRef()
			body := pull.GetBody()
			if strings.HasPrefix(head, "hive/setup-baseline-") || strings.HasPrefix(head, "hive/repair-") ||
				strings.HasPrefix(head, "hive/visual-hive-") || strings.Contains(body, "<!-- hive-visual-hive") {
				return fmt.Errorf("orphaned setup recovery refuses post-setup lifecycle pull request #%d", pull.GetNumber())
			}
		}
		if stop || response == nil || response.NextPage == 0 {
			break
		}
		pullOptions.Page = response.NextPage
	}
	issueOptions := &gh.IssueListByRepoOptions{State: "all", Since: plan.SetupMergedAt, ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		issues, response, listErr := client.GoGitHub().Issues.ListByRepo(ctx, owner, name, issueOptions)
		if listErr != nil {
			return fmt.Errorf("inspect lifecycle issues after orphaned setup: %w", listErr)
		}
		for _, issue := range issues {
			if issue.IsPullRequest() || !issue.GetCreatedAt().After(plan.SetupMergedAt) {
				continue
			}
			for _, label := range issue.Labels {
				name := label.GetName()
				if name == "hive/managed" || name == "visual-hive/live" || strings.HasPrefix(name, "visual-hive/") {
					return fmt.Errorf("orphaned setup recovery refuses post-setup lifecycle issue #%d", issue.GetNumber())
				}
			}
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		issueOptions.ListOptions.Page = response.NextPage
	}
	return nil
}

func readRepositoryFileAtRef(ctx context.Context, client *hivegithub.Client, owner, name, path, ref string) ([]byte, bool, error) {
	file, directory, _, err := client.GoGitHub().Repositories.GetContents(ctx, owner, name, path, &gh.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isGitHubNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s at %s: %w", path, ref, err)
	}
	if file == nil || len(directory) != 0 || file.GetType() != "file" {
		return nil, false, fmt.Errorf("managed path %s at %s is not one ordinary file", path, ref)
	}
	content, err := file.GetContent()
	if err != nil {
		return nil, false, fmt.Errorf("decode %s at %s: %w", path, ref, err)
	}
	return []byte(content), true, nil
}

func isGitHubNotFound(err error) bool {
	var response *gh.ErrorResponse
	return errors.As(err, &response) && response.Response != nil && response.Response.StatusCode == http.StatusNotFound
}

func orphanSetupResetPlanDigest(plan OrphanSetupResetPlan) (string, error) {
	binding := struct {
		SchemaVersion        string                   `json:"schema_version"`
		Repository           string                   `json:"repository"`
		RepositoryID         string                   `json:"repository_id"`
		DefaultBranch        string                   `json:"default_branch"`
		CurrentHeadSHA       string                   `json:"current_head_sha"`
		SetupPRNumber        int                      `json:"setup_pr_number"`
		SetupBaseSHA         string                   `json:"setup_base_sha"`
		SetupHeadSHA         string                   `json:"setup_head_sha"`
		SetupMergeSHA        string                   `json:"setup_merge_sha"`
		SetupAuthorizerID    int64                    `json:"setup_authorizer_id"`
		OperatorID           int64                    `json:"operator_id,omitempty"`
		WriterID             int64                    `json:"writer_id,omitempty"`
		RuntimeBindingDigest string                   `json:"runtime_binding_digest,omitempty"`
		ManagedPaths         []OrphanSetupPathBinding `json:"managed_paths"`
	}{
		SchemaVersion: plan.SchemaVersion, Repository: plan.Repository, RepositoryID: plan.RepositoryID,
		DefaultBranch: plan.DefaultBranch, CurrentHeadSHA: plan.CurrentHeadSHA, SetupPRNumber: plan.SetupPRNumber,
		SetupBaseSHA: plan.SetupBaseSHA, SetupHeadSHA: plan.SetupHeadSHA, SetupMergeSHA: plan.SetupMergeSHA,
		SetupAuthorizerID: plan.SetupAuthorizerID, OperatorID: plan.OperatorID, WriterID: plan.WriterID,
		RuntimeBindingDigest: plan.RuntimeBindingDigest, ManagedPaths: plan.ManagedPaths,
	}
	data, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

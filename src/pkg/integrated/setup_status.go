package integrated

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kubestellar/hive/pkg/automation"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

var setupBaselineAuthorizationPath = regexp.MustCompile(`^\.visual-hive/snapshots/(?:[^/]+/)*[^/]+\.png$`)

type managedSetupAuthorization struct {
	Binding SetupAuthorizationBinding
	Diff    SetupDiffEvidence
	Status  hivegithub.SetupAuthorizationStatusResult
}

func authorizeManagedSetupPullRequest(ctx context.Context, store *Store, client *hivegithub.Client, policy automation.Policy, config Config, operation, checkout, branch, expectedHead string, pullURL string, pullNumber int) (managedSetupAuthorization, error) {
	result := managedSetupAuthorization{}
	if store == nil || client == nil || client.GoGitHub() == nil || config.SetupAuthorizationActorID <= 0 || pullNumber <= 0 {
		return result, fmt.Errorf("setup status authorization requires store, GitHub client, numeric actor, and pull request")
	}
	owner, repository, ok := strings.Cut(strings.TrimSpace(config.Repository), "/")
	if !ok || owner == "" || repository == "" {
		return result, fmt.Errorf("invalid setup authorization repository")
	}
	live, _, err := client.GoGitHub().PullRequests.Get(ctx, owner, repository, pullNumber)
	if err != nil {
		return result, fmt.Errorf("read exact setup pull request before authorization: %w", err)
	}
	expectedHead = strings.ToLower(strings.TrimSpace(expectedHead))
	baseSHA := strings.ToLower(strings.TrimSpace(live.GetBase().GetSHA()))
	repositoryID, idErr := strconv.ParseInt(strings.TrimSpace(config.RepositoryID), 10, 64)
	if idErr != nil || repositoryID <= 0 || !setupAuthorizationSHA.MatchString(expectedHead) || !setupAuthorizationSHA.MatchString(baseSHA) {
		return result, fmt.Errorf("setup authorization requires immutable repository ID and exact base/head SHAs")
	}
	if live.GetNumber() != pullNumber || live.GetState() != "open" || live.GetMerged() || live.GetHead().GetSHA() != expectedHead || live.GetHead().GetRef() != branch ||
		live.GetBase().GetRef() != config.DefaultBranch || !strings.EqualFold(live.GetHead().GetRepo().GetFullName(), config.Repository) ||
		!strings.EqualFold(live.GetBase().GetRepo().GetFullName(), config.Repository) || live.GetHead().GetRepo().GetID() != repositoryID || live.GetBase().GetRepo().GetID() != repositoryID ||
		strings.TrimSpace(live.GetHTMLURL()) == "" || (strings.TrimSpace(pullURL) != "" && live.GetHTMLURL() != strings.TrimSpace(pullURL)) {
		return result, fmt.Errorf("setup pull request no longer matches its exact same-repository branch/head/base identity")
	}
	remoteURL := RepositoryCloneURL(config.Repository)
	refspec := "+refs/heads/" + config.DefaultBranch + ":refs/remotes/origin/" + config.DefaultBranch
	if _, err := gitTransport(ctx, checkout, remoteURL, "fetch", "--no-tags", "--no-recurse-submodules", remoteURL, refspec); err != nil {
		return result, fmt.Errorf("fetch exact setup authorization base: %w", err)
	}
	fetchedBase, err := git(ctx, checkout, "rev-parse", "origin/"+config.DefaultBranch)
	if err != nil || !strings.EqualFold(strings.TrimSpace(fetchedBase), baseSHA) {
		return result, fmt.Errorf("setup pull request base changed while authorization was prepared; rerun setup")
	}
	requiredPresent := []string{".hive/integrated.json", ".github/workflows/hive-visual-hive.yml", "docs/hive-quickstart.md"}
	requiredAbsent := []string{}
	if operation == string(OperationUninstall) {
		if managedPathPreimagesConfigured(config) && !hasValidManagedPathPreimages(config) {
			return result, fmt.Errorf("setup authorization refuses an invalid managed-path preimage ledger")
		}
		requiredPresent, requiredAbsent = managedUninstallRequiredPaths(config)
	} else if config.VisualHive {
		requiredPresent = append(requiredPresent, ".github/workflows/visual-hive-pr.yml", "docs/visual-hive.md", "visual-hive.config.yaml")
		requiredAbsent = append(requiredAbsent, standaloneVisualHiveWriterWorkflowPaths()...)
	}
	lifecycleWriter, lifecycleWriterBound := setupAuthorizationWriter(config)
	if !lifecycleWriterBound {
		return result, fmt.Errorf("setup authorization has no exact Hive lifecycle writer binding")
	}
	writer, writerBound := visualHiveGitHubWriter(config)
	if !writerBound {
		return result, fmt.Errorf("setup authorization has no exact Visual Hive status writer binding")
	}
	if config.VisualHiveGitHubAppID > 0 && !hasVisualHiveStatusClient(ctx) {
		return result, fmt.Errorf("dedicated Visual Hive status App runtime is unavailable; use managed upgrade/rebind")
	}
	binding, diff, err := BuildSetupAuthorizationBindingWithDiff(ctx, SetupAuthorizationRequest{
		CheckoutDir: checkout, Repository: config.Repository, RepositoryID: config.RepositoryID, PullRequest: pullNumber,
		HeadRef: branch, HeadSHA: expectedHead, BaseRef: config.DefaultBranch, BaseSHA: baseSHA, AuthorizerID: config.SetupAuthorizationActorID,
		WriterID: writer.ID, WriterLogin: writer.Login, WriterType: writer.Type,
		RequiredPresent: requiredPresent, RequiredAbsent: requiredAbsent,
	})
	if err != nil {
		return result, err
	}
	allowed := map[string]bool{}
	for _, candidate := range managedSetupFilesForConfig(config) {
		allowed[candidate] = true
	}
	for _, changed := range diff.ChangedPaths {
		// Baseline PNGs are allowed for uninstall only because the exact
		// out-of-band status binds every changed path and blob, while final
		// verification independently requires the pre-install inventory.
		baselineAllowed := setupBaselineAuthorizationPath.MatchString(changed)
		if !allowed[changed] && !baselineAllowed {
			return result, fmt.Errorf("setup authorization refuses unmanaged changed path %s", changed)
		}
	}
	statusContext, err := binding.StatusContext()
	if err != nil {
		return result, err
	}
	if err := authorizeSetup(store, policy, config.Repository, automation.ActionSetupStatus); err != nil {
		return result, err
	}
	statusClient := visualHiveStatusClient(ctx, client)
	status, err := statusClient.EnsureSetupAuthorizationStatus(ctx, hivegithub.SetupAuthorizationStatusRequest{
		Repository: config.Repository, HeadSHA: expectedHead, Context: statusContext, TargetURL: live.GetHTMLURL(),
		Description: fmt.Sprintf("Hive exact %s PR #%d diff %.12s", operation, pullNumber, diff.SHA256), ExpectedCreatorID: writer.ID,
		ExpectedCreatorLogin: writer.Login, ExpectedCreatorType: writer.Type,
	})
	if err != nil {
		return result, fmt.Errorf("authorize exact setup pull request status: %w", err)
	}
	detail := fmt.Sprintf("operation=%s pr=%d head=%s base=%s branch=%s diff=%s context=%s actor_id=%d lifecycle_writer_id=%d lifecycle_writer_login=%s status_writer_id=%d status_writer_login=%s status_writer_type=%s status_id=%d reused=%t recovered=%t", operation, pullNumber, expectedHead, baseSHA, branch, diff.SHA256, statusContext, config.SetupAuthorizationActorID, lifecycleWriter.ID, lifecycleWriter.Login, writer.ID, writer.Login, writer.Type, status.StatusID, status.Reused, status.Recovered)
	if err := store.AuditStrict(AuditEntry{Action: "bind_setup_status", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return result, fmt.Errorf("persist exact setup status audit: %w", err)
	}
	result.Binding, result.Diff, result.Status = binding, diff, status
	return result, nil
}

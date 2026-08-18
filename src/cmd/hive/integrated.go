package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/pkg/automation"
	"github.com/kubestellar/hive/pkg/config"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/hostedbootstrap"
	"github.com/kubestellar/hive/pkg/hostedcontrol"
	"github.com/kubestellar/hive/pkg/hostedstate"
	"github.com/kubestellar/hive/pkg/integrated"
	"github.com/kubestellar/hive/pkg/repair"
	"github.com/kubestellar/hive/pkg/visualhive"
)

const defaultVisualHiveRepository = "DavidDiaz0317/visual-hive"

var hostedIntegratedStateRoot = filepath.Clean("/data/integrated")

func runIntegratedCommand(command string, args []string) int {
	if err := validateHostedIntegratedStateRoot(); err != nil {
		fmt.Fprintln(os.Stderr, "hosted integrated state validation failed:", err)
		return 1
	}
	switch command {
	case "setup":
		return runSetupCommand(args)
	case "status":
		return runIntegratedStatus(args)
	case "doctor":
		return runIntegratedDoctor(args)
	case "start":
		return runIntegratedStart(args)
	case "stop":
		return runIntegratedStop(args)
	case "daemon":
		return runIntegratedDaemon(args)
	case "installer-transition":
		return runInstallerTransition(args)
	case "pause", "resume":
		return runIntegratedPause(command, args)
	case "set-coverage", "set-automation":
		return runIntegratedSetting(command, args)
	case "set-issue-limit":
		return runIntegratedIssueLimit(args)
	case "set-retry-limit":
		return runIntegratedRetryLimit(args)
	case "run":
		return runIntegratedRun(args)
	case "hosted-cycle":
		return runHostedCycleCommand(args)
	case "approve-merge":
		return runIntegratedApproveMerge(args)
	case "approve-baseline":
		return runIntegratedApproveBaseline(args)
	case "revoke-merge-approval":
		return runIntegratedRevokeMergeApproval(args)
	case "retry-repair":
		return runIntegratedRetryRepair(args)
	case "recover-dispatch":
		return runIntegratedRecoverDispatch(args)
	case "transfer-setup-authorizer":
		return runIntegratedAuthorizerTransfer(args)
	case "upgrade", "rollback", "uninstall":
		return runIntegratedManagement(command, args)
	default:
		fmt.Fprintf(os.Stderr, "unknown integrated Hive command %q\n", command)
		return 2
	}
}

func runIntegratedAuthorizerTransfer(args []string) int {
	flags := flag.NewFlagSet("hive transfer-setup-authorizer", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	newAuthorizer := flags.String("new-authorizer", "", "exact GitHub login of the proposed human setup authorizer")
	reason := flags.String("reason", "", "bounded accountable reason for transferring setup authority")
	cancelTransfer := flags.Bool("cancel", false, "audit and abort an unmerged transfer, closing its exact PR and deleting its exact-head branch")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if !*cancelTransfer && (strings.TrimSpace(*newAuthorizer) == "" || strings.TrimSpace(*reason) == "") {
		fmt.Fprintln(os.Stderr, "--new-authorizer and an accountable --reason are required")
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	daemon := readIntegratedDaemonStatus(*stateDir)
	wasRunning := daemon.Running
	restartInterval := 15 * time.Minute
	if daemon.IntervalSeconds >= 60 {
		restartInterval = time.Duration(daemon.IntervalSeconds) * time.Second
	}
	if wasRunning {
		if _, err := stopIntegratedDaemon(*stateDir); err != nil {
			fmt.Fprintln(os.Stderr, "setup authorizer transfer could not stop the persistent scheduler:", err)
			return 1
		}
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunAuthorizerTransfer(ctx, integrated.AuthorizerTransferOptions{
		StateDir: *stateDir, NewAuthorizer: *newAuthorizer, Reason: *reason, Cancel: *cancelTransfer, GitHub: client, GitTransportToken: token,
	})
	if wasRunning {
		if _, startErr := ensureIntegratedDaemonStarted(*stateDir, restartInterval); startErr != nil {
			if err == nil {
				fmt.Fprintln(os.Stderr, "setup authorizer transfer succeeded but scheduler restart failed:", startErr)
				return 1
			}
		}
	}
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.setup-authorizer-transfer.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "setup authorizer transfer failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Completed {
		fmt.Printf("Setup authorizer transferred to %s (numeric ID %d) after exact merge verification.\n", result.NewAuthorizerLogin, result.NewAuthorizerID)
		return 0
	}
	if result.Cancelled {
		fmt.Printf("Setup authorizer transfer cancelled; authority remains with %s (numeric ID %d).\n", result.OldAuthorizerLogin, result.OldAuthorizerID)
		return 0
	}
	fmt.Printf("Setup authorizer transfer PR ready: %s\n", result.PRURL)
	fmt.Printf("Local authority remains with %s (numeric ID %d). After the exact PR merges, run: %s\n", result.OldAuthorizerLogin, result.OldAuthorizerID, result.NextCommand)
	fmt.Printf("To abort before merge through the supported audited path, run: %s\n", result.CancelCommand)
	return 0
}

type hostedSetupBootstrapResult struct {
	State          hostedbootstrap.Result
	ProviderSecret *hostedbootstrap.ProviderSecretResult
}

func bootstrapHostedSetup(ctx context.Context, client *hivegithub.Client, stateDir string, result integrated.SetupResult) (hostedSetupBootstrapResult, error) {
	if client == nil || client.GoGitHub() == nil || result.Config == nil {
		return hostedSetupBootstrapResult{}, fmt.Errorf("hosted bootstrap requires the applied setup configuration and GitHub client")
	}
	config := *result.Config
	owner, repository, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repository == "" {
		return hostedSetupBootstrapResult{}, fmt.Errorf("hosted bootstrap repository identity is incomplete")
	}
	repositoryID, err := strconv.ParseInt(config.RepositoryID, 10, 64)
	if err != nil || repositoryID <= 0 {
		return hostedSetupBootstrapResult{}, fmt.Errorf("hosted bootstrap requires the numeric GitHub repository ID")
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, repository, config.DefaultBranch, 0)
	if err != nil || branch == nil || branch.GetCommit() == nil || len(branch.GetCommit().GetSHA()) != 40 {
		return hostedSetupBootstrapResult{}, fmt.Errorf("read exact default head for hosted bootstrap: %w", err)
	}
	paths, err := hostedcontrol.PortableInventory(stateDir)
	if err != nil {
		return hostedSetupBootstrapResult{}, fmt.Errorf("inventory initial portable hosted state: %w", err)
	}
	metadata := hostedstate.Metadata{
		Repository:  hostedstate.RepositoryIdentity{FullName: config.Repository, ID: repositoryID},
		StateBranch: config.HostedStateBranch, Sequence: 1, ExecutionOwner: "bootstrap",
		Release: hostedstate.ReleaseIdentity{
			Version: config.HiveReleaseVersion, HiveCommit: config.HiveCommit, VisualHiveCommit: config.VisualHiveRef,
			DistributionManifestSHA256: config.DistributionManifestSHA256, HostedControllerProtocol: config.HostedControllerProtocol,
		},
		Controller: hostedstate.ControllerIdentity{
			Event: "bootstrap", WorkflowPath: integrated.HostedControllerWorkflowPath,
			WorkflowSHA: strings.ToLower(result.CommitSHA), DefaultHeadSHA: strings.ToLower(branch.GetCommit().GetSHA()),
		},
		CreatedAt: time.Now().UTC(),
	}
	var restoreRelease *hostedstate.ReleaseIdentity
	if config.PreviousHostedRelease != nil {
		restoreRelease = &hostedstate.ReleaseIdentity{
			Version: config.PreviousHostedRelease.Version, HiveCommit: config.PreviousHostedRelease.HiveCommit, VisualHiveCommit: config.PreviousHostedRelease.VisualHiveCommit,
			DistributionManifestSHA256: config.PreviousHostedRelease.DistributionManifestSHA256, HostedControllerProtocol: config.PreviousHostedRelease.HostedControllerProtocol,
		}
	}
	stateResult, err := hostedbootstrap.Bootstrap(ctx, client.GoGitHub(), hostedbootstrap.Options{
		Owner: owner, Repository: repository, StateBranch: config.HostedStateBranch,
		StateRoot: stateDir, StatePaths: paths, Metadata: metadata, RestoreRelease: restoreRelease,
	})
	if err != nil {
		return hostedSetupBootstrapResult{}, err
	}
	bootstrapResult := hostedSetupBootstrapResult{State: stateResult}
	if hostedAutomationRequiresProviderSecret(config.Automation) {
		providerSecret, secretErr := hostedbootstrap.EnsureProviderSecret(ctx, client.GoGitHub(), owner, repository, []byte(os.Getenv(hostedbootstrap.ProviderSecretName)))
		if secretErr != nil {
			return hostedSetupBootstrapResult{}, secretErr
		}
		bootstrapResult.ProviderSecret = &providerSecret
	}
	return bootstrapResult, nil
}

func hostedAutomationRequiresProviderSecret(automation integrated.Automation) bool {
	return automation == integrated.AutomationRepairPR || automation == integrated.AutomationAutoMerge
}

func runIntegratedRecoverDispatch(args []string) int {
	flags := flag.NewFlagSet("hive recover-dispatch", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	actionValue := flags.String("action", "", "recovery action: retry or revoke")
	correlation := flags.String("correlation", "", "exact 64-character workflow dispatch correlation from hive status")
	requestDigest := flags.String("request-digest", "", "exact ambiguous request digest returned by --plan")
	planDigest := flags.String("plan-digest", "", "exact recovery plan digest returned by --plan")
	plannedAtValue := flags.String("planned-at", "", "exact RFC3339Nano plan time returned by --plan")
	reason := flags.String("reason", "", "bounded accountable operator reason for retry or revocation")
	planOnly := flags.Bool("plan", false, "read-only: exhaustively prove no matching run and return exact apply bindings")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key for ambiguous request retry")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	action := integrated.WorkflowDispatchRecoveryAction(strings.ToLower(strings.TrimSpace(*actionValue)))
	if strings.TrimSpace(*correlation) == "" || (action != integrated.WorkflowDispatchRecoveryRetry && action != integrated.WorkflowDispatchRecoveryRevoke) {
		fmt.Fprintln(os.Stderr, "--action (retry or revoke) and --correlation are required")
		return 2
	}
	var plannedAt time.Time
	if !*planOnly {
		var err error
		plannedAt, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(*plannedAtValue))
		if err != nil || strings.TrimSpace(*requestDigest) == "" || strings.TrimSpace(*planDigest) == "" || strings.TrimSpace(*reason) == "" {
			fmt.Fprintln(os.Stderr, "apply requires the exact --request-digest, --plan-digest, --planned-at, and an accountable --reason returned by --plan")
			return 2
		}
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		operation := hostedRecoverDispatchPlan
		if !*planOnly {
			operation = hostedRecoverDispatchApply
		}
		return runHostedOperatorCommand(*stateDir, operation, *requestID, *githubTokenEnv, *githubAPIURL, hostedRecoveryArguments{
			Action: string(action), Correlation: *correlation, RequestDigest: *requestDigest, PlanDigest: *planDigest,
			PlannedAt: plannedAt, Reason: *reason,
		}, *jsonOutput)
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := integrated.RecoverWorkflowDispatch(ctx, integrated.RecoverWorkflowDispatchOptions{
		StateDir: *stateDir, Action: action, CorrelationID: *correlation,
		ExpectedRequestDigest: *requestDigest, ExpectedPlanDigest: *planDigest, PlannedAt: plannedAt,
		Reason: *reason, PlanOnly: *planOnly, GitHub: client,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.recover-workflow-dispatch.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "recover workflow dispatch failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Planned {
		fmt.Printf("Recovery plan found no exact matching run and expires at %s. Apply: %s\n", result.ExpiresAt.Format(time.RFC3339), result.ApplyCommand)
	} else if result.Revoked {
		fmt.Printf("Revoked ambiguous dispatch %s. Run: %s\n", result.CorrelationID, result.NextCommand)
	} else {
		fmt.Printf("Authorized one audited retry for dispatch %s. Run: %s\n", result.CorrelationID, result.NextCommand)
	}
	return 0
}

func runIntegratedRetryRepair(args []string) int {
	flags := flag.NewFlagSet("hive retry-repair", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	fingerprint := flags.String("finding", "", "exact repository finding fingerprint from hive status")
	recurrence := flags.Int("recurrence", -1, "exact finding recurrence from hive status")
	attempt := flags.Int("attempt", 0, "exact paused repair attempt ordinal")
	failureClass := flags.String("failure-class", "", "infrastructure or patch_engine")
	failureID := flags.String("failure-id", "", "exact failure ID from hive status")
	reason := flags.String("reason", "", "bounded operator reason for resuming the exact checkpoint")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key for ambiguous request retry")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	class := repair.FailureClass(strings.ToLower(strings.TrimSpace(*failureClass)))
	if strings.TrimSpace(*fingerprint) == "" || *recurrence < 0 || *attempt <= 0 || (class != repair.FailureInfrastructure && class != repair.FailurePatchEngine) || strings.TrimSpace(*failureID) == "" || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, "--finding, --recurrence, --attempt, --failure-class (infrastructure or patch_engine), --failure-id, and --reason are required")
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return runHostedOperatorCommand(*stateDir, hostedRetryRepair, *requestID, *githubTokenEnv, *githubAPIURL, hostedRetryArguments{
			Finding: *fingerprint, Recurrence: *recurrence, Attempt: *attempt, FailureClass: string(class), FailureID: *failureID, Reason: *reason,
		}, *jsonOutput)
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := integrated.RetryRepair(ctx, integrated.RetryRepairOptions{
		StateDir: *stateDir, RepositoryFingerprint: *fingerprint, ExpectedRecurrence: *recurrence, ExpectedAttempt: *attempt,
		ExpectedFailureClass: class, ExpectedFailureID: *failureID, Reason: *reason, GitHub: client,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.retry-repair.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "retry repair failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Resumed repair %s at attempt %d without spending a new model attempt. Run: %s\n", result.Attempt.RepositoryFingerprint, result.Attempt.Attempt, result.NextCommand)
	return 0
}

func runIntegratedApproveMerge(args []string) int {
	flags := flag.NewFlagSet("hive approve-merge", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	prNumber := flags.Int("pr", 0, "Hive repair pull request number")
	headSHA := flags.String("head", "", "exact 40-character tested pull request head SHA")
	baseSHA := flags.String("base", "", "exact reviewed base SHA returned by --plan")
	diffDigest := flags.String("diff-digest", "", "exact reviewed raw diff SHA-256 returned by --plan")
	reason := flags.String("reason", "", "bounded reason for overriding only the path allowlist hold")
	planOnly := flags.Bool("plan", false, "read-only: return the exact base and diff digest required for approval")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key for ambiguous request retry")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if *prNumber <= 0 || strings.TrimSpace(*headSHA) == "" || (!*planOnly && (strings.TrimSpace(*baseSHA) == "" || strings.TrimSpace(*diffDigest) == "" || strings.TrimSpace(*reason) == "")) {
		fmt.Fprintln(os.Stderr, "plan requires --pr and --head; apply also requires --base, --diff-digest, and --reason; actor is bound to the authenticated GitHub login")
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		operation := hostedApproveMergePlan
		if !*planOnly {
			operation = hostedApproveMergeApply
		}
		return runHostedOperatorCommand(*stateDir, operation, *requestID, *githubTokenEnv, *githubAPIURL, hostedMergeArguments{
			PRNumber: *prNumber, HeadSHA: strings.ToLower(*headSHA), BaseSHA: strings.ToLower(*baseSHA), DiffDigest: strings.ToLower(*diffDigest), Reason: *reason,
		}, *jsonOutput)
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := integrated.ApproveMerge(ctx, integrated.ApproveMergeOptions{
		StateDir: *stateDir, PRNumber: *prNumber, ExpectedHeadSHA: *headSHA, ExpectedBaseSHA: *baseSHA,
		ExpectedDiff: *diffDigest, Reason: *reason, PlanOnly: *planOnly, GitHub: client,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.approve-merge.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "approve merge failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Planned {
		fmt.Printf("Approval plan is bound to base %s, head %s, and diff %s. Apply: %s\n", result.Gate.BaseSHA, result.Gate.HeadSHA, result.DiffDigest, result.ApplyCommand)
	} else {
		fmt.Printf("Approved exact repair PR #%d at %s. Run: %s\n", result.Approval.PRNumber, result.Approval.HeadSHA, result.NextCommand)
	}
	return 0
}

func runIntegratedApproveBaseline(args []string) int {
	flags := flag.NewFlagSet("hive approve-baseline", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	repositoryID := flags.String("repo-id", "", "exact immutable repository ID returned by --plan")
	runID := flags.Int64("run-id", 0, "exact hosted capture workflow run ID returned by --plan")
	artifactID := flags.Int64("artifact-id", 0, "exact verified hosted artifact ID returned by --plan")
	prNumber := flags.Int("pr", 0, "exact held setup baseline PR number returned by --plan")
	headSHA := flags.String("head", "", "exact candidate-only PR head SHA returned by --plan")
	baseSHA := flags.String("base", "", "exact reviewed base SHA returned by --plan")
	diffDigest := flags.String("diff-digest", "", "exact reviewed raw diff SHA-256 returned by --plan")
	candidateDigest := flags.String("candidate-digest", "", "exact hosted candidate list digest returned by --plan")
	actorID := flags.Int64("actor-id", 0, "exact numeric setup authorizer ID returned by --plan")
	planDigest := flags.String("plan-digest", "", "exact complete approval plan digest returned by --plan")
	reason := flags.String("reason", "", "bounded accountable reason for approving the exact hosted baseline set")
	planOnly := flags.Bool("plan", false, "read-only: inspect and bind every hosted artifact, PR, diff, candidate, and authorizer field")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key for ambiguous request retry")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if !*planOnly && (strings.TrimSpace(*repositoryID) == "" || *runID <= 0 || *artifactID <= 0 || *prNumber <= 0 || strings.TrimSpace(*headSHA) == "" ||
		strings.TrimSpace(*baseSHA) == "" || strings.TrimSpace(*diffDigest) == "" || strings.TrimSpace(*candidateDigest) == "" || *actorID <= 0 ||
		strings.TrimSpace(*planDigest) == "" || strings.TrimSpace(*reason) == "") {
		fmt.Fprintln(os.Stderr, "apply requires every exact --repo-id, --run-id, --artifact-id, --pr, --head, --base, --diff-digest, --candidate-digest, --actor-id, --plan-digest, and --reason returned by --plan")
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		operation := hostedApproveBaselinePlan
		if !*planOnly {
			operation = hostedApproveBaselineApply
		}
		return runHostedOperatorCommand(*stateDir, operation, *requestID, *githubTokenEnv, *githubAPIURL, hostedBaselineArguments{
			RepositoryID: *repositoryID, RunID: *runID, ArtifactID: *artifactID, PRNumber: *prNumber, HeadSHA: strings.ToLower(*headSHA),
			BaseSHA: strings.ToLower(*baseSHA), DiffDigest: strings.ToLower(*diffDigest), CandidateDigest: strings.ToLower(*candidateDigest),
			ActorID: *actorID, PlanDigest: strings.ToLower(*planDigest), Reason: *reason,
		}, *jsonOutput)
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := integrated.ApproveSetupBaseline(ctx, integrated.ApproveSetupBaselineOptions{
		StateDir: *stateDir, PlanOnly: *planOnly, ExpectedRepositoryID: *repositoryID, ExpectedCaptureRunID: *runID, ExpectedArtifactID: *artifactID,
		ExpectedPRNumber: *prNumber, ExpectedHeadSHA: *headSHA, ExpectedBaseSHA: *baseSHA, ExpectedDiffDigest: *diffDigest,
		ExpectedCandidateDigest: *candidateDigest, ExpectedActorID: *actorID, ExpectedPlanDigest: *planDigest, Reason: *reason, GitHub: client, GitTransportToken: token,
	})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.approve-setup-baseline.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "approve setup baseline failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Planned {
		fmt.Printf("Setup baseline approval plan bound run %d, artifact %d, PR #%d, actor ID %d, and candidate digest %s. Apply: %s\n", result.CaptureRunID, result.ArtifactID, result.PRNumber, result.ActorID, result.CandidateDigest, result.ApplyCommand)
	} else if result.Merged {
		fmt.Printf("Approved and merged exact setup baselines at %s. Run: %s\n", result.MergeSHA, result.NextCommand)
	} else {
		fmt.Printf("Approved exact setup baselines; Hive will merge after all exact gates pass. Run: %s\n", result.NextCommand)
	}
	return 0
}

func runIntegratedRevokeMergeApproval(args []string) int {
	flags := flag.NewFlagSet("hive revoke-merge-approval", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	reason := flags.String("reason", "", "bounded accountable reason for revocation")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(os.Stderr, "--reason is required")
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := integrated.RevokeMergeApproval(ctx, integrated.RevokeMergeApprovalOptions{StateDir: *stateDir, Reason: *reason, GitHub: client})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.revoke-merge-approval.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "revoke merge approval failed:", err)
		}
		return 1
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Merge approval revoked=%t for %s.\n", result.Revoked, result.Repository)
	return 0
}

func runIntegratedManagement(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	var lifecycleBeadsDirs stringListFlag
	flags.Var(&lifecycleBeadsDirs, "beads-dir", "ordinary Hive role beads directory for normal-runtime uninstall; repeatable")
	visualRef := ""
	flags.StringVar(&visualRef, "version", "", "immutable Visual Hive commit SHA")
	flags.StringVar(&visualRef, "visual-hive-ref", "", "immutable Visual Hive commit SHA")
	deleteState := flags.Bool("delete-state", false, "request exact post-merge uninstall finalization; preparation always preserves state")
	cancelPending := flags.Bool("cancel", false, "cancel the exact pending unmerged uninstall cleanup and keep automation paused")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if command != "uninstall" && len(lifecycleBeadsDirs) != 0 {
		fmt.Fprintln(os.Stderr, "--beads-dir is supported only for uninstall")
		return 2
	}
	if command != "uninstall" && visualRef == "" && command != "rollback" {
		fmt.Fprintln(os.Stderr, "--visual-hive-ref is required")
		return 2
	}
	if *cancelPending && (command != "uninstall" || *deleteState) {
		fmt.Fprintln(os.Stderr, "--cancel is only valid for uninstall and cannot be combined with --delete-state")
		return 2
	}
	managementRuntimeCommand := ""
	managementRuntimeArgs := []string(nil)
	if command == "upgrade" || command == "rollback" {
		targetRef := strings.ToLower(strings.TrimSpace(visualRef))
		if targetRef == "" {
			prior, exists, loadErr := loadExistingSetupConfig(*stateDir)
			if loadErr != nil || !exists || strings.TrimSpace(prior.PreviousVersion) == "" {
				fmt.Fprintln(os.Stderr, "rollback requires a durable previous Visual Hive version:", loadErr)
				return 2
			}
			targetRef = strings.ToLower(strings.TrimSpace(prior.PreviousVersion))
		}
		resolvedCommand, resolvedArgs, resolveErr := resolveVisualHiveLauncher("", nil, os.Getenv("HIVE_VISUAL_HIVE_HOME"))
		if resolveErr != nil {
			fmt.Fprintln(os.Stderr, command+" requires the signed integrated release matching the requested Visual Hive commit:", resolveErr)
			return 2
		}
		if len(resolvedArgs) == 0 {
			fmt.Fprintln(os.Stderr, command+" could not identify the packaged Visual Hive entrypoint")
			return 2
		}
		manifest, manifestErr := integrated.ValidateVisualHiveRelease(resolvedArgs[0], targetRef)
		if manifestErr != nil || !strings.EqualFold(manifest.GitCommit, targetRef) {
			fmt.Fprintln(os.Stderr, command+" requires installing the signed integrated release for exact Visual Hive commit "+targetRef+":", manifestErr)
			return 2
		}
		visualRef, managementRuntimeCommand, managementRuntimeArgs = targetRef, resolvedCommand, resolvedArgs
	}
	daemon := readIntegratedDaemonStatus(*stateDir)
	wasRunning := daemon.Running
	restartInterval := 15 * time.Minute
	if daemon.IntervalSeconds >= 60 {
		restartInterval = time.Duration(daemon.IntervalSeconds) * time.Second
	}
	if wasRunning {
		if _, stopErr := stopIntegratedDaemon(*stateDir); stopErr != nil {
			fmt.Fprintln(os.Stderr, command+" could not stop the persistent scheduler:", stopErr)
			return 1
		}
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		if wasRunning {
			_, _ = ensureIntegratedDaemonStarted(*stateDir, restartInterval)
		}
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	client := hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	selectionCleared := false
	if command == "uninstall" && *deleteState {
		var clearErr error
		selectionCleared, clearErr = integrated.ForgetCurrentState(integratedStateRoot(), *stateDir)
		if clearErr != nil {
			fmt.Fprintln(os.Stderr, "uninstall could not durably retire its current repository selection:", clearErr)
			return 1
		}
	}
	result, err := integrated.RunManagement(ctx, integrated.ManagementOptions{
		Operation: integrated.ManagementOperation(command), StateDir: *stateDir, VisualHiveRef: visualRef,
		VisualHiveCommand: managementRuntimeCommand, VisualHiveArgs: managementRuntimeArgs,
		LifecycleBeadsDirs: append([]string(nil), lifecycleBeadsDirs...), DeleteState: *deleteState, Cancel: *cancelPending, GitHub: client, GitTransportToken: token,
	})
	if err != nil {
		if selectionCleared {
			if info, statErr := os.Stat(*stateDir); statErr == nil && info.IsDir() {
				if restoreErr := integrated.RememberCurrentState(integratedStateRoot(), *stateDir); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("restore current repository selection after failed uninstall finalization: %w", restoreErr))
				}
			}
		}
		shouldRestart := shouldRestartManagementScheduler(command, *stateDir)
		if wasRunning && shouldRestart {
			_, _ = ensureIntegratedDaemonStarted(*stateDir, restartInterval)
		}
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.management.v1", "operation": command, "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, command+" failed:", err)
		}
		return 1
	}
	if wasRunning && command != "uninstall" {
		if _, startErr := ensureIntegratedDaemonStarted(*stateDir, restartInterval); startErr != nil {
			fmt.Fprintln(os.Stderr, command+" succeeded but scheduler restart failed:", startErr)
			return 1
		}
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.StateDeleted {
		fmt.Printf("%s finalized; managed local state deleted after exact cleanup verification.\n", command)
		return 0
	}
	if result.Cancelled {
		fmt.Println("Pending uninstall cancelled; its exact unmerged PR/ref were retired and automation remains paused. Run hive resume explicitly or start uninstall again.")
		return 0
	}
	if result.FinalizationPending {
		if result.PRURL != "" {
			fmt.Printf("%s cleanup PR ready: %s\n", command, result.PRURL)
		} else {
			fmt.Printf("%s target is already clean.\n", command)
		}
		fmt.Printf("State preserved. After the cleanup merge and lifecycle reconciliation, run: %s\n", result.NextCommand)
		return 0
	}
	if result.Idempotent && result.PRURL == "" {
		fmt.Printf("%s already at the requested state.\n", command)
	} else {
		fmt.Printf("%s PR ready: %s\n", command, result.PRURL)
	}
	return 0
}

func shouldRestartManagementScheduler(command, stateDir string) bool {
	if command != "uninstall" {
		return true
	}
	current, exists, err := loadExistingSetupConfig(stateDir)
	return err == nil && exists && !current.Paused
}

func runIntegratedRun(args []string) int {
	flags := flag.NewFlagSet("hive run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	timeout := flags.Duration("timeout", 45*time.Minute, "maximum hosted run and lifecycle duration")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	requestID := flags.String("request-id", "", "exact hosted idempotency key used only when resuming an ambiguous dispatch")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return dispatchHostedOperation(*stateDir, "cycle", *requestID, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	token := resolveGitHubToken(*githubTokenEnv)
	if token == "" {
		fmt.Fprintln(os.Stderr, "GitHub authorization is required")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		if *jsonOutput {
			return encodeJSON(map[string]any{"schema_version": "hive.production-run.v1", "error": err.Error()})
		}
		fmt.Fprintln(os.Stderr, "Hive production run failed:", err)
		return 1
	}
	durable, err := store.Load()
	if err != nil {
		if *jsonOutput {
			return encodeJSON(map[string]any{"schema_version": "hive.production-run.v1", "error": err.Error()})
		}
		fmt.Fprintln(os.Stderr, "Hive production run failed:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+time.Minute)
	defer cancel()
	result, err := runClaimedIntegratedOneShot(ctx, *stateDir, *timeout, durable, newIntegratedSpecialistRuntime,
		func(runCtx context.Context, runStateDir string, runTimeout time.Duration, specialists *integratedSpecialistRuntime) (integrated.RunResult, error) {
			options := integrated.RunOptions{StateDir: runStateDir, Timeout: runTimeout, GitHub: client, GitTransportToken: token}
			if specialists != nil {
				options.Specialists = specialists.Manager
				options.SpecialistWorkDir = specialists.WorkDir
			}
			return integrated.RunOnce(runCtx, options)
		})
	if err != nil {
		if *jsonOutput {
			_ = encodeJSON(map[string]any{"schema_version": "hive.production-run.v1", "error": err.Error(), "partial": result})
		} else {
			fmt.Fprintln(os.Stderr, "Hive production run failed:", err)
		}
		return 1
	}
	activated, activationErr := activateDeferredSchedulerIfReady(*stateDir, *githubTokenEnv, *githubAPIURL)
	if activationErr != nil {
		fmt.Fprintln(os.Stderr, "Hive production run completed, but deferred scheduler activation failed:", activationErr)
		return 1
	}
	if activated {
		fmt.Fprintln(os.Stderr, "Hive setup and doctor prerequisites are green; the requested persistent scheduler is now running.")
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	fmt.Printf("Run %s completed: findings=%d issues-updated=%d repairs=%d\n", result.Workflow.RunURL, result.Lifecycle.Created+result.Lifecycle.Updated, result.Outbox.Succeeded, len(result.Repairs))
	return 0
}

func runSetupCommand(args []string) int {
	flags := flag.NewFlagSet("hive setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	repository := flags.String("repo", "", "GitHub repository owner/name")
	coverageValue := flags.String("coverage", "", "essential, standard, comprehensive, or custom")
	automationValue := flags.String("automation", "", "advisory, issues, repair-pr, or auto-merge")
	provider := flags.String("provider", "codex", "repair model provider")
	providerCommand := flags.String("provider-command", os.Getenv("HIVE_CODEX_COMMAND"), "repair provider executable; auto-detected for Codex")
	runtimeMode := flags.String("runtime", "hosted", "execution runtime: hosted or local (new installations default to hosted)")
	visualHive := flags.Bool("visual-hive", true, "install Visual Hive deterministic testing")
	visualCommand := flags.String("visual-hive-command", "", "Visual Hive CLI launcher; defaults to the packaged runtime")
	visualHome := flags.String("visual-hive-home", os.Getenv("HIVE_VISUAL_HIVE_HOME"), "directory containing an immutable Visual Hive release bundle")
	visualRepo := flags.String("visual-hive-repo", valueOrEnv("VISUAL_HIVE_REPOSITORY", defaultVisualHiveRepository), "Visual Hive source repository")
	visualRef := flags.String("visual-hive-ref", os.Getenv("VISUAL_HIVE_REF"), "immutable Visual Hive commit SHA")
	maxActiveIssues := flags.Int("max-active-issues", 5, "maximum concurrently open Hive-managed findings")
	maxRepairAttempts := flags.Int("max-repair-attempts", 4, "maximum bounded model-backed repair attempts per finding")
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	planOnly := flags.Bool("plan", false, "produce a read-only setup plan")
	start := flags.Bool("start", false, "start after the setup PR is merged and doctor is green")
	directBootstrap := flags.Bool("direct-bootstrap", false, "explicitly install one exact commit into a new private disposable repository without a setup PR")
	adoptReviewedBaselines := flags.Bool("adopt-reviewed-baselines", false, "with --direct-bootstrap, bind exact human-reviewed baseline PNGs already present in the seed commit")
	expectedSeedSHA := flags.String("expected-seed-sha", "", "with --direct-bootstrap, exact 40-character seed commit reviewed before setup")
	reviewedBaselineDigest := flags.String("reviewed-baseline-digest", "", "with --adopt-reviewed-baselines, SHA-256 of the exact reviewed baseline inventory")
	runInterval := flags.Duration("run-interval", 15*time.Minute, "persistent production scan interval")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	dashboardOperatorHandoffPath := flags.String("dashboard-operator-handoff", "", "internal root-sealed dashboard operator handoff")
	dashboardRequestID := flags.String("dashboard-request-id", "", "internal dashboard request binding")
	dashboardPlanSHA256 := flags.String("dashboard-plan-sha256", "", "internal dashboard plan binding")
	var providerArgs stringListFlag
	var visualArgs stringListFlag
	var autoMergePaths stringListFlag
	var autoMergeRisks stringListFlag
	flags.Var(&providerArgs, "provider-arg", "repair provider launcher argument; repeatable (normal local repair defaults to --model=gpt-5.6-sol)")
	flags.Var(&visualArgs, "visual-hive-arg", "Visual Hive launcher argument before the CLI subcommand; repeatable")
	flags.Var(&autoMergePaths, "auto-merge-path", "repository-relative glob eligible for autonomous merge; repeatable (default: test-only paths)")
	flags.Var(&autoMergeRisks, "auto-merge-risk", "eligible risk tier: automatic, low, medium, or restricted; repeatable (default: automatic)")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	explicit := map[string]bool{}
	flags.Visit(func(candidate *flag.Flag) { explicit[candidate.Name] = true })
	if !explicit["state-dir"] && strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")) == "" && strings.TrimSpace(*repository) != "" {
		selected, selectErr := repositoryIntegratedStateDir(*repository)
		if selectErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", selectErr)
			return 1
		}
		*stateDir = selected
	}
	prior, hasPrior, priorErr := loadExistingSetupConfig(*stateDir)
	if priorErr != nil {
		fmt.Fprintln(os.Stderr, "setup failed: load existing installation:", priorErr)
		return 1
	}
	if hasPrior {
		if err := inheritExistingSetup(prior, explicit, repository, coverageValue, automationValue, provider, providerCommand, visualCommand, visualRepo, visualRef, visualHive, maxActiveIssues, maxRepairAttempts, &providerArgs, &visualArgs); err != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", err)
			return 2
		}
		if !explicit["runtime"] {
			*runtimeMode = string(prior.ExecutionMode)
			if *runtimeMode == "" {
				*runtimeMode = string(integrated.ExecutionLocal)
			}
		}
		if !explicit["run-interval"] && prior.RunIntervalSeconds > 0 {
			*runInterval = time.Duration(prior.RunIntervalSeconds) * time.Second
		}
	}
	if cli := os.Getenv("VISUAL_HIVE_CLI"); cli != "" && !explicit["visual-hive-arg"] {
		visualArgs = stringListFlag{cli}
	}
	if *repository == "" || *coverageValue == "" || *automationValue == "" {
		if *jsonOutput || !isInteractiveTerminal() {
			fmt.Fprintln(os.Stderr, "--repo, --coverage, and --automation are required in noninteractive mode")
			return 2
		}
		reader := bufio.NewReader(os.Stdin)
		if *repository == "" {
			*repository = prompt(reader, "Repository (owner/name)")
		}
		if *coverageValue == "" {
			*coverageValue = prompt(reader, "Coverage depth (essential, standard, comprehensive, custom)")
		}
		if *automationValue == "" {
			*automationValue = prompt(reader, "Automation authority (advisory, issues, repair-pr, auto-merge)")
		}
	}
	if !explicit["state-dir"] && strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")) == "" && !hasPrior {
		selected, selectErr := repositoryIntegratedStateDir(*repository)
		if selectErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", selectErr)
			return 1
		}
		*stateDir = selected
	}
	coverage := integrated.Coverage(strings.ToLower(strings.TrimSpace(*coverageValue)))
	automationMode := integrated.Automation(strings.ToLower(strings.TrimSpace(*automationValue)))
	executionMode := integrated.ExecutionMode(strings.ToLower(strings.TrimSpace(*runtimeMode)))
	if executionMode != integrated.ExecutionHosted && executionMode != integrated.ExecutionLocal {
		fmt.Fprintln(os.Stderr, "setup failed: --runtime must be hosted or local")
		return 2
	}
	normalizedProvider, normalizeProviderErr := integrated.NormalizeNormalHiveProvider(integrated.SetupOptions{
		Provider: *provider, ProviderArgs: append([]string(nil), providerArgs...), ExecutionMode: executionMode,
		VisualHive: *visualHive, Automation: automationMode,
	})
	if normalizeProviderErr != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", normalizeProviderErr)
		return 2
	}
	providerArgs = append(providerArgs[:0], normalizedProvider.ProviderArgs...)
	hostedSchedule := ""
	releaseIdentity := installedReleaseIdentity{}
	if executionMode == integrated.ExecutionHosted {
		var scheduleErr error
		hostedSchedule, scheduleErr = hostedCronForInterval(*runInterval)
		if scheduleErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", scheduleErr)
			return 2
		}
		releaseIdentity, scheduleErr = loadInstalledReleaseIdentity()
		if scheduleErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", scheduleErr)
			return 2
		}
	}
	parsedAutoMergeRisks, riskErr := parseAutoMergeRiskFlags(autoMergeRisks)
	if riskErr != nil {
		fmt.Fprintln(os.Stderr, "setup failed:", riskErr)
		return 2
	}
	token := resolveGitHubToken(*githubTokenEnv)
	var client *hivegithub.Client
	if token != "" {
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		client = hivegithub.NewClient(token, "", nil, logger, *githubAPIURL)
	}
	var dashboardHandoff dashboardOperatorHandoff
	hasDashboardHandoff := strings.TrimSpace(*dashboardOperatorHandoffPath) != "" || strings.TrimSpace(*dashboardRequestID) != "" || strings.TrimSpace(*dashboardPlanSHA256) != ""
	if hasDashboardHandoff {
		if client == nil || strings.TrimSpace(*dashboardOperatorHandoffPath) == "" || !dashboardOperatorRequestIDPattern.MatchString(*dashboardRequestID) || !dashboardOperatorDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(*dashboardPlanSHA256))) {
			fmt.Fprintln(os.Stderr, "setup failed: dashboard operator handoff is incomplete or invalid")
			return 2
		}
		handoffCtx, handoffCancel := context.WithTimeout(context.Background(), 30*time.Second)
		var handoffErr error
		dashboardHandoff, handoffErr = consumeDashboardOperatorHandoff(handoffCtx, *dashboardOperatorHandoffPath, *repository, *dashboardRequestID, *dashboardPlanSHA256)
		handoffCancel()
		if handoffErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed: consume dashboard operator handoff:", handoffErr)
			return 2
		}
		if strings.EqualFold(dashboardHandoff.Writer.Type, "Bot") {
			if err := client.SetVerifiedAppWriter(dashboardHandoff.Writer, dashboardHandoff.App.BindingDigest); err != nil {
				fmt.Fprintln(os.Stderr, "setup failed: bind dashboard App writer:", err)
				return 2
			}
		}
	}
	if !*planOnly {
		if client == nil {
			fmt.Fprintf(os.Stderr, "GitHub authorization is required. Set %s or sign in once with gh auth login.\n", *githubTokenEnv)
			return 2
		}
	}
	if *visualHive {
		resolvedCommand, resolvedArgs, resolvedRef, resolveErr := resolveSetupVisualHive(*visualCommand, visualArgs, *visualHome, *visualRef)
		if resolveErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", resolveErr)
			return 2
		}
		*visualCommand, visualArgs, *visualRef = resolvedCommand, resolvedArgs, resolvedRef
	}
	if !*planOnly && executionMode == integrated.ExecutionLocal {
		providerCtx, providerCancel := context.WithTimeout(context.Background(), 60*time.Second)
		resolvedProviderCommand, providerErr := resolveIntegratedProvider(providerCtx, *provider, *providerCommand, providerArgs)
		providerCancel()
		if providerErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", providerErr)
			return 2
		}
		*providerCommand = resolvedProviderCommand
	} else if !*planOnly && strings.TrimSpace(*providerCommand) == "" {
		*providerCommand = "codex"
	}
	wasRunning := false
	hadPendingStart := false
	restartInterval := *runInterval
	if !*planOnly && executionMode == integrated.ExecutionLocal {
		if hasPrior {
			store, storeErr := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
			if storeErr != nil {
				fmt.Fprintln(os.Stderr, "setup failed: inspect deferred scheduler start:", storeErr)
				return 1
			}
			if intent, pending, intentErr := store.LoadSchedulerStartIntent(); intentErr != nil {
				fmt.Fprintln(os.Stderr, "setup failed: inspect deferred scheduler start:", intentErr)
				return 1
			} else if pending {
				hadPendingStart = true
				if intent.IntervalSeconds >= 60 {
					restartInterval = time.Duration(intent.IntervalSeconds) * time.Second
				}
			}
		}
		daemon := readIntegratedDaemonStatus(*stateDir)
		if authErr := requirePersistentGitHubAuthorization(*start || daemon.Running || hadPendingStart); authErr != nil {
			fmt.Fprintln(os.Stderr, "setup failed:", authErr)
			return 2
		}
		if daemon.Running {
			wasRunning = true
			if daemon.IntervalSeconds >= 60 {
				restartInterval = time.Duration(daemon.IntervalSeconds) * time.Second
			}
			if _, stopErr := stopIntegratedDaemon(*stateDir); stopErr != nil {
				fmt.Fprintln(os.Stderr, "setup failed: stop scheduler before reconfiguration:", stopErr)
				return 1
			}
		}
	}
	shouldStart := *start || wasRunning || hadPendingStart
	acmm := acmmForIntegratedAutomation(automationMode)
	mode := automationModeForIntegrated(automationMode)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := integrated.RunSetup(ctx, integrated.SetupOptions{
		Repository: *repository, Coverage: coverage, Automation: automationMode, Provider: *provider,
		ProviderCommand: *providerCommand, ProviderArgs: append([]string(nil), providerArgs...),
		ExecutionMode: executionMode, RunInterval: *runInterval, HostedSchedule: hostedSchedule,
		HiveReleaseRepository: releaseIdentity.Repository, HiveReleaseVersion: releaseIdentity.Version,
		HiveCommit: releaseIdentity.HiveCommit, DistributionManifestSHA256: releaseIdentity.ManifestSHA256,
		HostedControllerProtocol: releaseIdentity.HostedControllerProtocol,
		VisualHive:               *visualHive, StateDir: *stateDir, Apply: !*planOnly, Start: shouldStart,
		DirectBootstrap: *directBootstrap, AdoptReviewedBaselines: *adoptReviewedBaselines,
		ExpectedSeedSHA: *expectedSeedSHA, ReviewedBaselineDigest: *reviewedBaselineDigest,
		VisualHiveCommand: *visualCommand, VisualHiveArgs: append([]string(nil), visualArgs...),
		VisualHiveRepo: *visualRepo, VisualHiveRef: *visualRef, GitHub: client, GitTransportToken: token,
		AuthorizationActor: dashboardHandoff.Actor, AuthorizationWriter: dashboardHandoff.Writer, AuthorizationApp: dashboardHandoff.App,
		MaxActiveIssues:        *maxActiveIssues,
		MaxRepairAttempts:      *maxRepairAttempts,
		AllowedAutoMergePaths:  append([]string(nil), autoMergePaths...),
		AllowedAutoMergeRisk:   parsedAutoMergeRisks,
		AutoMergePathsExplicit: explicit["auto-merge-path"],
		AutoMergeRiskExplicit:  explicit["auto-merge-risk"],
		Policy:                 automation.Policy{ACMMLevel: acmm, Mode: mode, AllowedRepositories: []string{*repository}, MaxRepairAttempts: *maxRepairAttempts},
	})
	if err != nil {
		if wasRunning {
			_, _ = ensureIntegratedDaemonStarted(*stateDir, restartInterval)
		}
		fmt.Fprintln(os.Stderr, "setup failed:", err)
		return 1
	}
	if executionMode == integrated.ExecutionHosted && result.Applied {
		bootstrap, bootstrapErr := bootstrapHostedSetup(ctx, client, *stateDir, result)
		if bootstrapErr != nil {
			fmt.Fprintln(os.Stderr, "setup PR is ready, but hosted controller state bootstrap failed:", bootstrapErr)
			return 1
		}
		result.HostedStateReady = true
		result.HostedStateBranch = bootstrap.State.StateBranch
		result.HostedStateCommit = bootstrap.State.StateCommitSHA
		result.HostedStateReused = bootstrap.State.AlreadyConfigured
		if bootstrap.ProviderSecret != nil {
			result.HostedProviderSecretReady = true
			result.HostedProviderSecretName = bootstrap.ProviderSecret.SecretName
			result.HostedProviderSecretReused = bootstrap.ProviderSecret.AlreadyConfigured
		}
	}
	if result.Applied && !explicit["state-dir"] && strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")) == "" {
		if pointerErr := integrated.RememberCurrentState(integratedStateRoot(), *stateDir); pointerErr != nil {
			fmt.Fprintln(os.Stderr, "setup applied but selecting its repository state failed:", pointerErr)
			return 1
		}
	}
	configuredOwner := configuredRuntimeOwnerIntent(integrated.Config{ExecutionMode: executionMode, VisualHive: *visualHive, Automation: automationMode})
	if result.Config != nil {
		configuredOwner = configuredRuntimeOwnerIntent(*result.Config)
	}
	if executionMode == integrated.ExecutionHosted && shouldStart && result.Applied {
		result.SchedulerStartRequested = true
		result.SchedulerStartPending = !result.Idempotent
		result.SchedulerStartMessage = "The hosted controller will own cadence and lifecycle after the exact setup PR is merged; no local scheduler was created."
	} else if shouldStart && result.Applied && configuredOwner == runtimeOwnerNormalHive {
		if clearErr := clearDeferredSchedulerStart(*stateDir); clearErr != nil {
			fmt.Fprintln(os.Stderr, "setup applied but canceling an obsolete legacy scheduler request failed:", clearErr)
			return 1
		}
		_, _, normalRuntimeRunning := normalVisualDaemonObserved(*stateDir)
		result.SchedulerStartRequested = true
		result.SchedulerStartPending = !normalRuntimeRunning
		if normalRuntimeRunning {
			result.SchedulerStartMessage = "Ordinary Hive/dashboard remains the configured runtime owner; no legacy scheduler was created."
		} else {
			result.SchedulerStartMessage = "Ordinary Hive/dashboard is the configured runtime owner; restart the normal Hive/dashboard process to activate polling. No legacy scheduler was created."
		}
	} else if shouldStart && result.Applied {
		result.SchedulerStartRequested = true
		if wasRunning && result.Idempotent {
			if _, startErr := ensureIntegratedDaemonStarted(*stateDir, restartInterval); startErr != nil {
				fmt.Fprintln(os.Stderr, "setup remained unchanged but persistent scheduler restart failed:", startErr)
				return 1
			}
			if clearErr := clearDeferredSchedulerStart(*stateDir); clearErr != nil {
				fmt.Fprintln(os.Stderr, "setup remained unchanged but clearing deferred scheduler state failed:", clearErr)
				return 1
			}
			result.SchedulerStartMessage = "Persistent scheduler remained active after the idempotent setup check."
		} else {
			if intentErr := recordDeferredSchedulerStart(*stateDir, *repository, restartInterval); intentErr != nil {
				fmt.Fprintln(os.Stderr, "setup applied but recording deferred persistent startup failed:", intentErr)
				return 1
			}
			result.SchedulerStartPending = true
			result.SchedulerStartMessage = "Persistent scheduler start is recorded and will activate only after the setup PR is merged and every other doctor check is green."
			if result.Idempotent {
				activated, activationErr := activateDeferredSchedulerIfReady(*stateDir, *githubTokenEnv, *githubAPIURL)
				if activationErr != nil {
					fmt.Fprintln(os.Stderr, "setup is unchanged but deferred persistent startup failed:", activationErr)
					return 1
				}
				if activated {
					result.SchedulerStartPending = false
					result.SchedulerStartMessage = "Setup and doctor prerequisites are green; the persistent scheduler is running."
				}
			}
		}
	}
	if *jsonOutput {
		return encodeJSON(result)
	}
	if result.Applied {
		if *directBootstrap {
			fmt.Printf("Direct bootstrap installed at %s without a setup PR.\nPersistent state: %s\n", result.CommitSHA, *stateDir)
		} else {
			fmt.Printf("Setup PR ready: %s\nPersistent state: %s\n", result.PRURL, *stateDir)
		}
		if result.ActivationMessage != "" {
			fmt.Println(result.ActivationMessage)
		}
		if result.SchedulerStartMessage != "" {
			fmt.Println(result.SchedulerStartMessage)
		}
	} else {
		fmt.Printf("Setup plan for %s: coverage=%s automation=%s ACMM=L%d Visual-Hive=%s@%s\n", result.Plan.Repository, result.Plan.Coverage, result.Plan.Automation, result.Plan.ACMMLevel, result.Plan.VisualHiveRepository, result.Plan.VisualHiveRef)
	}
	return 0
}

func parseAutoMergeRiskFlags(values []string) ([]automation.RiskTier, error) {
	result := make([]automation.RiskTier, 0, len(values))
	for _, value := range values {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "automatic":
			result = append(result, automation.RiskAutomatic)
		case "low":
			result = append(result, automation.RiskLow)
		case "medium":
			result = append(result, automation.RiskMedium)
		case "restricted":
			result = append(result, automation.RiskRestricted)
		default:
			return nil, fmt.Errorf("auto-merge risk %q must be automatic, low, medium, or restricted", value)
		}
	}
	return result, nil
}

func loadExistingSetupConfig(stateDir string) (integrated.Config, bool, error) {
	configPath := filepath.Join(stateDir, "integrated", "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			return integrated.Config{}, false, nil
		}
		return integrated.Config{}, false, err
	}
	store, err := integrated.NewStore(filepath.Dir(configPath))
	if err != nil {
		return integrated.Config{}, false, err
	}
	config, err := store.Load()
	if err != nil {
		return integrated.Config{}, false, err
	}
	return config, true, nil
}

func inheritExistingSetup(prior integrated.Config, explicit map[string]bool, repository, coverageValue, automationValue, provider, providerCommand, visualCommand, visualRepo, visualRef *string, visualHive *bool, maxActiveIssues, maxRepairAttempts *int, providerArgs, visualArgs *stringListFlag) error {
	if explicit["repo"] && !strings.EqualFold(strings.TrimSpace(*repository), prior.Repository) {
		return fmt.Errorf("state directory is already bound to %s; use a different --state-dir for %s", prior.Repository, *repository)
	}
	if !explicit["repo"] {
		*repository = prior.Repository
	}
	if !explicit["coverage"] {
		*coverageValue = string(prior.Coverage)
	}
	if !explicit["automation"] {
		*automationValue = string(prior.Automation)
	}
	if !explicit["provider"] {
		*provider = prior.Provider
	}
	if !explicit["provider-command"] && os.Getenv("HIVE_CODEX_COMMAND") == "" {
		*providerCommand = prior.ProviderCommand
	}
	if !explicit["provider-arg"] {
		*providerArgs = append((*providerArgs)[:0], prior.ProviderArgs...)
	}
	if !explicit["visual-hive"] {
		*visualHive = prior.VisualHive
	}
	if !explicit["visual-hive-repo"] && os.Getenv("VISUAL_HIVE_REPOSITORY") == "" {
		*visualRepo = prior.VisualHiveRepo
	}
	if !explicit["visual-hive-ref"] && os.Getenv("VISUAL_HIVE_REF") == "" {
		*visualRef = prior.VisualHiveRef
	}
	if !explicit["max-active-issues"] {
		*maxActiveIssues = prior.MaxActiveIssues
	}
	if !explicit["max-repair-attempts"] {
		*maxRepairAttempts = prior.MaxRepairAttempts
	}
	runtimeOverride := explicit["visual-hive-command"] || explicit["visual-hive-home"] || explicit["visual-hive-arg"] || os.Getenv("HIVE_VISUAL_HIVE_HOME") != "" || os.Getenv("VISUAL_HIVE_CLI") != ""
	if !runtimeOverride {
		*visualCommand = prior.VisualHiveCommand
		*visualArgs = append((*visualArgs)[:0], prior.VisualHiveArgs...)
	}
	return nil
}

func runIntegratedStatus(args []string) int {
	flags := flag.NewFlagSet("hive status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	store, err := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	config, err := store.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Hive is not set up:", err)
		return 1
	}
	if config.ExecutionMode == integrated.ExecutionHosted {
		return runHostedStatusCommand(*stateDir, config, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var client *hivegithub.Client
	if token := resolveGitHubToken(*githubTokenEnv); token != "" {
		client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), *githubAPIURL)
	}
	liveChecks := liveRepositoryChecks(ctx, client, config)
	runtimeOK, runtimeMessage := validateVisualHiveLauncher(config)
	liveChecks = append(liveChecks, doctorCheck{Name: "visual_hive_runtime", OK: runtimeOK, Message: runtimeMessage})
	pauseRequested, pauseErr := integrated.PauseRequested(*stateDir)
	ready := !config.Paused && !pauseRequested && pauseErr == nil && len(config.VisualHiveRef) == 40
	for _, check := range liveChecks {
		ready = ready && check.OK
	}
	var providerErr error
	providerMessage := "repair provider is not required for this automation level"
	if config.Automation == integrated.AutomationRepairPR || config.Automation == integrated.AutomationAutoMerge {
		provider := integratedSetupCodexProvider(config.ProviderCommand, config.ProviderArgs)
		providerCtx, providerCancel := context.WithTimeout(ctx, 45*time.Second)
		providerErr = provider.Health(providerCtx)
		providerCancel()
		providerMessage = errorOr(providerErr, "provider authenticated")
		ready = ready && providerErr == nil
	}
	daemon := readIntegratedDaemonStatus(*stateDir)
	daemonReady := daemon.RuntimeRunning && daemonServiceReady(daemon.Service)
	normalHealth := inspectNormalVisualServiceHealth(*stateDir, config.Repository, time.Duration(config.RunIntervalSeconds)*time.Second, time.Now().UTC())
	ownerIntent := configuredRuntimeOwnerIntent(config)
	ownerObservation := inspectRuntimeOwner(*stateDir, config, daemon)
	runtimeReady := daemonReady
	if ownerIntent == runtimeOwnerNormalHive {
		runtimeReady = normalHealth.ServiceReady
	}
	runtimeReady = runtimeReady && !ownerObservation.Conflict
	ready = ready && runtimeReady
	normalServiceMessage := normalHealth.Message
	if ownerIntent != runtimeOwnerNormalHive {
		normalServiceMessage = "normal Visual service is not the configured runtime owner; " + ownerObservation.Message
	}
	status := map[string]any{
		"schema_version": "hive.status.v1", "state_dir": *stateDir, "config": config, "paused": config.Paused, "pause_requested": pauseRequested, "production_ready": ready,
		"readiness_checks": liveChecks, "provider_ready": providerErr == nil, "provider_message": providerMessage, "daemon_ready": daemonReady,
		"normal_hive_dashboard_ready": normalHealth.DashboardReady,
		"normal_visual_service_ready": normalHealth.ServiceReady, "normal_visual_service_message": normalServiceMessage, "runtime_ready": runtimeReady,
	}
	for key, value := range runtimeOwnerStatusFields(ownerIntent, ownerObservation) {
		status[key] = value
	}
	status["daemon"] = daemon
	if ownerObservation.NormalLeaseRecordPresent || normalHealth.DashboardReady {
		status["normal_visual_service"] = normalHealth.Owner
	}
	if normalHealth.HealthExists {
		status["normal_visual_service_health"] = normalHealth.Health
	}
	status["runtime_owner"] = ownerObservation.ObservedOwner
	lifecycle := collectIntegratedLifecycleStatus(*stateDir, config, *stateDir)
	status["lifecycle_status"] = lifecycle
	if lifecycleReady, _ := lifecycle["production_ready"].(bool); !lifecycleReady {
		status["production_ready"] = false
	}
	if *jsonOutput {
		return encodeJSON(status)
	}
	fmt.Printf("%s: coverage=%s automation=%s ACMM=L%d paused=%t setup_pr=%s\n", config.Repository, config.Coverage, config.Automation, config.ACMMLevel, config.Paused, config.SetupPRURL)
	return 0
}

// collectIntegratedLifecycleStatus builds a path-safe view of durable Hive
// lifecycle state. commandStateDir is the caller-visible persistent path for
// local mode; hosted runners pass an empty value so ephemeral runner paths can
// never escape in receipts.
func collectIntegratedLifecycleStatus(stateDir string, config integrated.Config, commandStateDir string) map[string]any {
	status := map[string]any{
		"schema_version": "hive.lifecycle-status.v1", "repository": config.Repository, "repository_id": config.RepositoryID,
		"execution_mode": config.ExecutionMode, "paused": config.Paused, "production_ready": !config.Paused,
		"release": map[string]any{"hive_version": config.HiveReleaseVersion, "hive_commit": config.HiveCommit, "visual_hive_commit": config.VisualHiveRef, "manifest_sha256": config.DistributionManifestSHA256, "hosted_controller_protocol": config.HostedControllerProtocol, "workflow_sha256": config.HostedWorkflowSHA256},
	}
	store, err := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		status["state_error"] = lifecycleStatusError(commandStateDir, err)
		status["production_ready"] = false
		return status
	}
	pauseRequested, pauseErr := integrated.PauseRequested(stateDir)
	status["pause_requested"] = pauseRequested
	if pauseErr != nil {
		status["pause_state_error"] = lifecycleStatusError(commandStateDir, pauseErr)
		status["production_ready"] = false
	} else if pauseRequested {
		status["production_ready"] = false
	}
	if schedulerIntent, exists, loadErr := store.LoadSchedulerStartIntent(); loadErr != nil {
		status["scheduler_start_intent_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["scheduler_start_pending"] = true
		status["scheduler_start_intent"] = map[string]any{
			"schema_version": schedulerIntent.SchemaVersion, "repository": schedulerIntent.Repository,
			"interval_seconds": schedulerIntent.IntervalSeconds, "requested_at": schedulerIntent.RequestedAt,
		}
	}
	if approval, exists, loadErr := store.LoadMergeApproval(); loadErr != nil {
		status["merge_approval_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["merge_approval"] = map[string]any{
			"schema_version": approval.SchemaVersion, "repository": approval.Repository, "repository_id": approval.RepositoryID,
			"pr_number": approval.PRNumber, "head_sha": approval.HeadSHA, "base_sha": approval.BaseSHA, "base_branch": approval.BaseBranch,
			"diff_digest": approval.DiffDigest, "actor": approval.Actor, "approved_at": approval.ApprovedAt,
		}
	}
	if intent, exists, loadErr := store.LoadMergeIntent(); loadErr != nil {
		status["merge_intent_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["merge_intent"] = map[string]any{
			"schema_version": intent.SchemaVersion, "repository": intent.Repository, "repository_id": intent.RepositoryID,
			"pr_number": intent.PRNumber, "head_sha": intent.HeadSHA, "base_sha": intent.BaseSHA, "base_branch": intent.BaseBranch,
			"diff_digest": intent.DiffDigest, "writer_actor": intent.WriterActor, "authorization_digest": intent.Authorization,
			"authorized_at": intent.AuthorizedAt, "approval_present": intent.Approval != nil,
		}
	}
	if dispatch, exists, loadErr := store.LoadWorkflowDispatchIntent(); loadErr != nil {
		status["workflow_dispatch_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["workflow_dispatch"] = dispatch
		if integrated.WorkflowDispatchNeedsRecovery(dispatch) {
			status["workflow_dispatch_recovery"] = workflowDispatchRecoveryStatus(commandStateDir, dispatch)
			status["production_ready"] = false
		}
	}
	if activation, exists, loadErr := store.LoadProtectionActivation(); loadErr != nil {
		status["protection_activation_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["protection_activation"] = activation
	} else if config.Automation == integrated.AutomationAutoMerge {
		status["protection_activation_pending"] = true
		status["production_ready"] = false
	}
	if refresh, exists, loadErr := store.LoadRepairRefreshIntent(); loadErr != nil {
		status["repair_refresh_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["repair_refresh"] = map[string]any{
			"schema_version": refresh.SchemaVersion, "repository": refresh.Repository, "repository_id": refresh.RepositoryID,
			"finding": refresh.RepositoryFingerprint, "recurrence": refresh.Recurrence, "attempt": refresh.Attempt,
			"pr_number": refresh.PRNumber, "branch": refresh.Branch, "old_head_sha": refresh.OldHeadSHA,
			"current_head_sha": refresh.CurrentHeadSHA, "base_branch": refresh.BaseBranch, "base_sha": refresh.BaseSHA,
			"changed_files": append([]string(nil), refresh.ChangedFiles...), "candidate_head_sha": refresh.CandidateHeadSHA,
			"final_owned_head_sha": refresh.FinalOwnedHeadSHA, "authorized_at": refresh.AuthorizedAt,
			"local_validation_completed_at": refresh.LocalValidationComplete,
		}
	}
	if retirements, exists, loadErr := store.LoadRepairBranchRetirements(); loadErr != nil {
		status["repair_branch_retirement_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		entries := make([]map[string]any, 0, len(retirements.Entries))
		for _, entry := range retirements.Entries {
			entries = append(entries, map[string]any{
				"id": entry.ID, "finding": entry.RepositoryFingerprint, "pr_number": entry.PRNumber,
				"retained_pr_number": entry.RetainedPRNumber, "branch": entry.Branch, "head_sha": entry.HeadSHA,
				"merge_sha": entry.MergeSHA, "purpose": entry.Purpose, "phase": entry.Phase,
				"repair_attempts": entry.RepairAttempts, "attempts": entry.Attempts,
				"last_error_present": strings.TrimSpace(entry.LastError) != "", "created_at": entry.CreatedAt,
				"last_attempt_at": entry.LastAttemptAt,
			})
		}
		status["repair_branch_retirements"] = map[string]any{
			"migration_completed": retirements.MigrationCompleted, "pending": len(retirements.Entries), "entries": entries,
			"message": "Exact superseded-PR and merged repair-branch retirement is durable; pending entries block production evidence and issue closure until reconciled.",
		}
		if !retirements.MigrationCompleted || len(retirements.Entries) != 0 {
			status["production_ready"] = false
		}
	}
	if transfer, exists, loadErr := store.LoadAuthorizerTransferIntent(); loadErr != nil {
		status["setup_authorizer_transfer_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		value := map[string]any{
			"phase": transfer.Phase, "repository": transfer.Repository, "repository_id": transfer.RepositoryID,
			"old_authorizer_id": transfer.OldAuthorizerID, "old_authorizer_login": transfer.OldAuthorizerLogin,
			"new_authorizer_id": transfer.NewAuthorizerID, "new_authorizer_login": transfer.NewAuthorizerLogin,
			"branch": transfer.Branch, "base_sha": transfer.BaseSHA, "head_sha": transfer.HeadSHA,
			"pr_number": transfer.PRNumber, "pr_url": transfer.PRURL, "prepared_at": transfer.PreparedAt,
			"authorized_at": transfer.AuthorizedAt,
			"message":       "Production mutations are fail-closed until the exact transfer is finalized or cancelled.",
		}
		if commandStateDir != "" {
			value["next_command"] = integrated.AuthorizerTransferNextCommand(commandStateDir, transfer)
			value["cancel_command"] = integrated.AuthorizerTransferCancelCommand(commandStateDir)
		}
		status["setup_authorizer_transfer"] = value
		status["production_ready"] = false
	}
	if rebind, exists, loadErr := store.LoadSetupBaselineRebindIntent(); loadErr != nil {
		status["setup_baseline_rebind_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		status["setup_baseline_rebind"] = map[string]any{
			"phase": rebind.Phase, "repository": rebind.Repository, "repository_id": rebind.RepositoryID,
			"source_phase": rebind.Source.Phase, "source_setup_pr_number": rebind.Source.SetupPRNumber,
			"source_setup_head_sha": rebind.Source.SetupHeadSHA, "target_config_digest": rebind.TargetConfigDigest,
			"created_at": rebind.CreatedAt, "updated_at": rebind.UpdatedAt, "resources_retired_at": rebind.ResourcesRetiredAt,
			"pending_audit": rebind.PendingAudit != nil, "next_command": integrated.SetupBaselineRebindNextCommand(rebind),
			"message": "Production remains fail-closed while Hive retires the obsolete hosted capture and review state."}
		status["production_ready"] = false
	}
	if baseline, exists, loadErr := store.LoadSetupBaselineIntent(); loadErr != nil {
		status["setup_baseline_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		next := integratedCommand("run", commandStateDir, "--json")
		if baseline.Phase == integrated.SetupBaselinePROpen {
			next = integratedCommand("approve-baseline", commandStateDir, "--plan --json")
		}
		status["setup_baseline"] = map[string]any{
			"phase": baseline.Phase, "repository": baseline.Repository, "repository_id": baseline.RepositoryID,
			"authorizer_id": baseline.AuthorizerID, "setup_pr_number": baseline.SetupPRNumber,
			"setup_pr_url": baseline.SetupPRURL, "setup_head_sha": baseline.SetupHeadSHA,
			"capture_head_sha": baseline.CaptureHeadSHA, "capture_run_id": baseline.CaptureRunID,
			"capture_run_url": baseline.CaptureRunURL, "artifact_id": baseline.ArtifactID,
			"artifact_name": baseline.ArtifactName, "candidate_digest": baseline.CandidateDigest,
			"candidate_count": len(baseline.Candidates), "branch": baseline.Branch, "commit_sha": baseline.CommitSHA,
			"pr_number": baseline.PRNumber, "pr_url": baseline.PRURL, "base_sha": baseline.BaseSHA,
			"diff_digest": baseline.DiffDigest, "approval_actor_id": baseline.ApprovalActorID,
			"merge_sha": baseline.MergeSHA, "production_run_id": baseline.ProductionRunID,
			"production_head_sha": baseline.ProductionHeadSHA, "pending_audit": baseline.PendingAudit != nil,
			"created_at": baseline.CreatedAt, "updated_at": baseline.UpdatedAt, "next_command": next,
			"message": "Production and lifecycle writes remain fail-closed until exact authorizer approval, Hive merge, and trusted verification complete."}
		if baseline.Phase != integrated.SetupBaselineProductionVerified || baseline.PendingAudit != nil {
			status["production_ready"] = false
		}
	}
	if uninstall, exists, loadErr := store.LoadUninstallIntent(); loadErr != nil {
		status["uninstall_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		value := map[string]any{
			"phase": uninstall.Phase, "repository": uninstall.Repository, "repository_id": uninstall.RepositoryID,
			"default_branch": uninstall.DefaultBranch, "branch": uninstall.Branch, "cleanup_commit_sha": uninstall.CleanupCommitSHA,
			"base_sha": uninstall.BaseSHA, "managed_paths_digest": uninstall.ManagedPathsDigest,
			"changed_files": append([]string(nil), uninstall.ChangedFiles...), "pr_number": uninstall.PRNumber,
			"pr_url": uninstall.PRURL, "diff_digest": uninstall.DiffDigest, "prepared_at": uninstall.PreparedAt,
			"pr_recorded_at": uninstall.PRRecordedAt,
			"message":        "Automation is paused until exact uninstall finalization or cancellation.",
		}
		if commandStateDir != "" {
			value["next_command"] = integrated.UninstallNextCommand(commandStateDir)
			value["cancel_command"] = integrated.UninstallCancelCommand(commandStateDir)
		}
		status["uninstall"] = value
		status["production_ready"] = false
	}
	if transition, exists, loadErr := store.LoadHostedReleaseTransitionIntent(); loadErr != nil {
		status["hosted_release_transition_error"] = lifecycleStatusError(commandStateDir, loadErr)
		status["production_ready"] = false
	} else if exists {
		value := map[string]any{
			"operation": transition.Operation, "phase": transition.Phase,
			"active_release": transition.ActiveRelease, "target_release": transition.TargetRelease,
			"source_was_paused": transition.SourceWasPaused, "setup_branch": transition.SetupBranch,
			"setup_commit_sha": transition.SetupCommitSHA, "pr_number": transition.SetupPRNumber,
			"pr_url": transition.SetupPRURL, "merge_sha": transition.MergeSHA,
			"message": "Hosted release transition is durably checkpointed and must be resumed to its exact terminal state.",
		}
		if commandStateDir != "" {
			value["next_command"] = integratedCommand(string(transition.Operation), commandStateDir, "--json")
		}
		status["hosted_release_transition"] = value
		status["production_ready"] = false
	}
	populateOperationalStateStatus(status, stateDir, commandStateDir)
	finalizeLifecycleQuiescence(status)
	return status
}

func finalizeLifecycleQuiescence(status map[string]any) {
	blockers := make([]string, 0, 16)
	add := func(value string) {
		for _, existing := range blockers {
			if existing == value {
				return
			}
		}
		blockers = append(blockers, value)
	}
	for key := range status {
		if strings.Contains(key, "error") {
			add(key)
		}
	}
	for _, key := range []string{
		"scheduler_start_pending", "merge_approval", "merge_intent", "workflow_dispatch_recovery", "repair_refresh",
		"setup_authorizer_transfer", "setup_baseline_rebind", "uninstall", "hosted_release_transition",
	} {
		if _, exists := status[key]; exists {
			add(key)
		}
	}
	if value, ok := status["repair_branch_retirements"].(map[string]any); ok {
		pending, _ := value["pending"].(int)
		migration, _ := value["migration_completed"].(bool)
		if pending != 0 || !migration {
			add("repair_branch_retirements")
		}
	}
	if value, ok := status["setup_baseline"].(map[string]any); ok {
		phase, _ := value["phase"].(string)
		pendingAudit, _ := value["pending_audit"].(bool)
		if phase != integrated.SetupBaselineProductionVerified || pendingAudit {
			add("setup_baseline")
		}
	}
	if value, ok := status["lifecycle"].(map[string]any); ok {
		pending, _ := value["pending_outbox"].(int)
		if pending != 0 {
			add("lifecycle_pending_outbox")
		}
	}
	if value, ok := status["repairs"].(map[string]any); ok {
		if attempts, ok := value["attempts"].([]map[string]any); ok {
			for _, attempt := range attempts {
				stage, _ := attempt["stage"].(repair.Stage)
				if stage == "" {
					if raw, ok := attempt["stage"].(string); ok {
						stage = repair.Stage(raw)
					}
				}
				switch stage {
				case repair.StageNoChange, repair.StagePROpen, repair.StageFailed, repair.StageCancelled:
				default:
					add("nonterminal_repair_attempt")
				}
			}
		}
	}
	sort.Strings(blockers)
	status["quiescent"] = len(blockers) == 0
	status["quiescence_blockers"] = blockers
	if len(blockers) != 0 {
		status["production_ready"] = false
	}
}

func addLifecycleQuiescenceBlocker(status map[string]any, blocker string) {
	blockers, _ := status["quiescence_blockers"].([]string)
	for _, existing := range blockers {
		if existing == blocker {
			status["quiescent"] = false
			status["production_ready"] = false
			return
		}
	}
	blockers = append(blockers, blocker)
	sort.Strings(blockers)
	status["quiescence_blockers"] = blockers
	status["quiescent"] = false
	status["production_ready"] = false
}

func lifecycleStatusError(commandStateDir string, err error) string {
	if err == nil {
		return ""
	}
	if commandStateDir == "" {
		return "durable hosted lifecycle state is invalid; inspect the exact controller run logs"
	}
	return err.Error()
}

func integratedCommand(command, stateDir, suffix string) string {
	if stateDir == "" {
		return strings.TrimSpace("hive " + command + " " + suffix)
	}
	return strings.TrimSpace(fmt.Sprintf("hive %s --state-dir %q %s", command, stateDir, suffix))
}

func workflowDispatchRecoveryStatus(stateDir string, dispatch integrated.WorkflowDispatchIntent) map[string]any {
	base := integratedCommand("recover-dispatch", stateDir, "")
	return map[string]any{
		"state":               "ambiguous_transport_failure",
		"correlation_id":      dispatch.CorrelationID,
		"request_digest":      dispatch.RequestDigest,
		"workflow_file":       dispatch.WorkflowFile,
		"ref":                 dispatch.Ref,
		"display_title":       dispatch.ExpectedDisplayTitle,
		"retry_plan_command":  fmt.Sprintf("%s --action retry --correlation %s --plan --json", base, dispatch.CorrelationID),
		"revoke_plan_command": fmt.Sprintf("%s --action revoke --correlation %s --plan --json", base, dispatch.CorrelationID),
		"message":             "Hive found no exact run for an unacknowledged dispatch transport result. Automation is fail-closed until an operator completes an exact plan/apply recovery.",
	}
}

func populateOperationalStateStatus(status map[string]any, stateDir string, commandStateDirs ...string) {
	commandStateDir := stateDir
	if len(commandStateDirs) != 0 {
		commandStateDir = commandStateDirs[0]
	}
	lifecycle, lifecycleExists, lifecycleErr := loadOptionalLifecycleStore(stateDir)
	if lifecycleErr != nil {
		status["lifecycle_state_error"] = lifecycleErr.Error()
		status["production_ready"] = false
	} else if lifecycleExists {
		snapshot := lifecycle.Snapshot()
		counts := map[visualhive.LifecycleStatus]int{}
		items := []map[string]any{}
		for _, finding := range snapshot.Findings {
			if finding == nil {
				continue
			}
			counts[finding.Status]++
			item := map[string]any{
				"fingerprint": finding.RepositoryFingerprint, "recurrence": finding.Recurrences, "status": finding.Status,
				"issue_number": finding.IssueNumber, "issue_url": finding.IssueURL, "pr_number": finding.PRNumber, "pr_url": finding.PRURL,
				"branch": finding.Branch, "repair_commit_sha": finding.RepairCommitSHA, "repair_attempts": finding.RepairAttempts,
				"human_review_required": finding.HumanReviewRequired, "manual_review_kind": finding.ManualReviewKind, "manual_review_reason": finding.ManualReviewReason,
			}
			if finding.HumanReviewRequired && !finding.ObservationHumanReviewRequired && finding.ManualReviewKind == "merge_policy" && finding.PRNumber > 0 && strings.TrimSpace(finding.RepairCommitSHA) != "" {
				item["next_command"] = integratedCommand("approve-merge", commandStateDir, fmt.Sprintf("--pr %d --head %s --plan --json", finding.PRNumber, finding.RepairCommitSHA))
			}
			items = append(items, item)
		}
		status["lifecycle"] = map[string]any{"counts": counts, "findings": items, "pending_outbox": len(lifecycle.PendingOutbox())}
	}

	repairState, repairExists, repairErr := loadOptionalRepairStore(stateDir)
	if repairErr != nil {
		status["repair_state_error"] = lifecycleStatusError(commandStateDir, repairErr)
		status["production_ready"] = false
	} else if repairExists {
		snapshot := repairState.Snapshot()
		repairs := make([]map[string]any, 0, len(snapshot.Attempts))
		resumable := []map[string]any{}
		for _, attempt := range snapshot.Attempts {
			if attempt == nil {
				continue
			}
			repairs = append(repairs, publicRepairAttempt(*attempt))
			if attempt.Stage != repair.StageFailed || (attempt.LastFailureClass != repair.FailureInfrastructure && attempt.LastFailureClass != repair.FailurePatchEngine) {
				continue
			}
			resumable = append(resumable, resumableRepairStatus(commandStateDir, *attempt))
		}
		status["repairs"] = map[string]any{"schema_version": snapshot.SchemaVersion, "attempts": repairs}
		status["resumable_repairs"] = resumable
	}
}

func publicRepairAttempt(attempt repair.Attempt) map[string]any {
	return map[string]any{
		"repository": attempt.Repository, "finding": attempt.RepositoryFingerprint, "recurrence": attempt.Recurrence,
		"attempt": attempt.Attempt, "branch": attempt.Branch, "stage": attempt.Stage, "provider": attempt.Provider,
		"attempt_counted": attempt.AttemptCounted, "last_failure_class": attempt.LastFailureClass,
		"last_failure_id": attempt.LastFailureID, "resume_stage": attempt.ResumeStage,
		"retry_actor": attempt.RetryActor, "retry_transaction_id": attempt.RetryTransactionID,
		"commit_sha": attempt.CommitSHA, "pr_number": attempt.PRNumber, "pr_url": attempt.PRURL,
		"lifecycle_pr_open": attempt.LifecyclePROpen, "changed_files": append([]string(nil), attempt.ChangedFiles...),
		"started_at": attempt.StartedAt, "updated_at": attempt.UpdatedAt,
	}
}

func resumableRepairStatus(stateDir string, attempt repair.Attempt) map[string]any {
	reasonTemplate := "describe the corrected infrastructure or patch-engine cause"
	attestation := repair.RequiredRecoveredProviderAttestation(attempt)
	if attestation != "" {
		reasonTemplate = "describe the completed historical patch provenance review; " + attestation
	}
	command := integratedCommand("retry-repair", stateDir, fmt.Sprintf("--finding %q --recurrence %d --attempt %d --failure-class %s --failure-id %q --reason %q --json", attempt.RepositoryFingerprint, attempt.Recurrence, attempt.Attempt, attempt.LastFailureClass, attempt.LastFailureID, reasonTemplate))
	return map[string]any{
		"finding":                     attempt.RepositoryFingerprint,
		"recurrence":                  attempt.Recurrence,
		"attempt":                     attempt.Attempt,
		"failure_class":               attempt.LastFailureClass,
		"failure_id":                  attempt.LastFailureID,
		"reason_template":             reasonTemplate,
		"required_reason_attestation": attestation,
		"next_command":                command,
		"next_command_template":       command,
	}
}

func loadOptionalLifecycleStore(stateDir string) (*visualhive.LifecycleStore, bool, error) {
	path := filepath.Join(stateDir, "visual-hive", "visual-hive-lifecycle.json")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect durable lifecycle state: %w", err)
	}
	store, err := visualhive.NewLifecycleStore(filepath.Dir(path))
	if err != nil {
		return nil, true, fmt.Errorf("load durable lifecycle state: %w", err)
	}
	return store, true, nil
}

func loadOptionalRepairStore(stateDir string) (*repair.Store, bool, error) {
	path := filepath.Join(stateDir, "repair", "repair-worker-state.json")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect durable repair state: %w", err)
	}
	store, err := repair.NewStore(filepath.Dir(path))
	if err != nil {
		return nil, true, fmt.Errorf("load durable repair state: %w", err)
	}
	return store, true, nil
}

func workflowDispatchDoctorCheck(store *integrated.Store) doctorCheck {
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return doctorCheck{Name: "workflow_dispatch_state", OK: false, Message: err.Error()}
	}
	if exists && integrated.WorkflowDispatchNeedsRecovery(intent) {
		return doctorCheck{Name: "workflow_dispatch_state", OK: false, Message: "ambiguous workflow dispatch requires the exact plan/apply recovery shown by hive status --json"}
	}
	return doctorCheck{Name: "workflow_dispatch_state", OK: true, Message: ternary(exists, "workflow dispatch checkpoint is valid", "no workflow dispatch recovery is pending")}
}

func operationalStateDoctorChecks(stateDir string) []doctorCheck {
	_, lifecycleExists, lifecycleErr := loadOptionalLifecycleStore(stateDir)
	_, repairExists, repairErr := loadOptionalRepairStore(stateDir)
	return []doctorCheck{
		{Name: "lifecycle_state", OK: lifecycleErr == nil, Message: errorOr(lifecycleErr, ternary(lifecycleExists, "durable lifecycle state is valid", "lifecycle state is not initialized yet"))},
		{Name: "repair_state", OK: repairErr == nil, Message: errorOr(repairErr, ternary(repairExists, "durable repair state is valid", "repair state is not initialized yet"))},
	}
}

type doctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func runIntegratedDoctor(args []string) int {
	flags := flag.NewFlagSet("hive doctor", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if config, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return runHostedDoctorCommand(config, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	checks := collectIntegratedDoctorChecks(*stateDir, *githubTokenEnv, *githubAPIURL, true)
	ready := doctorChecksReady(checks)
	output := map[string]any{"schema_version": "hive.doctor.v1", "production_ready": ready, "checks": checks}
	if *jsonOutput {
		_ = encodeJSON(output)
	} else {
		for _, check := range checks {
			fmt.Printf("[%s] %s: %s\n", ternary(check.OK, "ok", "fail"), check.Name, check.Message)
		}
	}
	if !ready {
		return 1
	}
	return 0
}

func collectIntegratedDoctorChecks(stateDir, githubTokenEnv, githubAPIURL string, includeScheduler bool) []doctorCheck {
	checks := []doctorCheck{}
	store, storeErr := integrated.NewStore(filepath.Join(stateDir, "integrated"))
	config, configErr := integrated.Config{}, storeErr
	if storeErr == nil {
		config, configErr = store.Load()
	}
	checks = append(checks, doctorCheck{Name: "config", OK: configErr == nil, Message: errorOr(configErr, "persistent config loaded")})
	if configErr == nil {
		pauseRequested, pauseErr := integrated.PauseRequested(stateDir)
		automationActive := !config.Paused && !pauseRequested && pauseErr == nil
		message := "repository automation is active"
		if pauseErr != nil {
			message = pauseErr.Error()
		} else if config.Paused || pauseRequested {
			message = "repository automation is paused or a pause is pending"
		}
		checks = append(checks, doctorCheck{Name: "automation_active", OK: automationActive, Message: message})
		_, _, approvalErr := store.LoadMergeApproval()
		checks = append(checks, doctorCheck{Name: "merge_approval_state", OK: approvalErr == nil, Message: errorOr(approvalErr, "merge approval state is valid")})
		_, _, intentErr := store.LoadMergeIntent()
		checks = append(checks, doctorCheck{Name: "merge_intent_state", OK: intentErr == nil, Message: errorOr(intentErr, "merge intent state is valid")})
		retirements, retirementsExist, retirementsErr := store.LoadRepairBranchRetirements()
		retirementOK := retirementsErr == nil && (!retirementsExist || (retirements.MigrationCompleted && len(retirements.Entries) == 0))
		retirementMessage := "no exact repair PR/branch retirement is pending"
		if retirementsErr != nil {
			retirementMessage = retirementsErr.Error()
		} else if retirementsExist && !retirements.MigrationCompleted {
			retirementMessage = "legacy repair branch retirement migration has not completed; hive run performs the bounded migration before production evidence"
		} else if retirementsExist && len(retirements.Entries) != 0 {
			retirementMessage = fmt.Sprintf("%d exact repair PR/branch retirement mutation(s) are pending; hive run retries them before production evidence", len(retirements.Entries))
		}
		checks = append(checks, doctorCheck{Name: "repair_branch_retirement_state", OK: retirementOK, Message: retirementMessage})
		transfer, transferExists, transferErr := store.LoadAuthorizerTransferIntent()
		transferMessage := "no setup authorizer transfer is pending"
		if transferErr != nil {
			transferMessage = transferErr.Error()
		} else if transferExists {
			transferMessage = fmt.Sprintf("transfer to %s/%d is pending; finish %s or cancel before merge with %s", transfer.NewAuthorizerLogin, transfer.NewAuthorizerID, integrated.AuthorizerTransferNextCommand(stateDir, transfer), integrated.AuthorizerTransferCancelCommand(stateDir))
		}
		checks = append(checks, doctorCheck{Name: "setup_authorizer_transfer_state", OK: transferErr == nil && !transferExists, Message: transferMessage})
		rebind, rebindExists, rebindErr := store.LoadSetupBaselineRebindIntent()
		rebindMessage := "no setup baseline reconfiguration cleanup is pending"
		if rebindErr != nil {
			rebindMessage = rebindErr.Error()
		} else if rebindExists {
			rebindMessage = fmt.Sprintf("setup baseline reconfiguration phase %s is pending; retry %s", rebind.Phase, integrated.SetupBaselineRebindNextCommand(rebind))
		}
		checks = append(checks, doctorCheck{Name: "setup_baseline_rebind_state", OK: rebindErr == nil && !rebindExists, Message: rebindMessage})
		baseline, baselineExists, baselineErr := store.LoadSetupBaselineIntent()
		baselineMessage := "no setup baseline capture is required"
		baselineOK := baselineErr == nil
		if baselineErr != nil {
			baselineMessage = baselineErr.Error()
		} else if baselineExists && (baseline.Phase != integrated.SetupBaselineProductionVerified || baseline.PendingAudit != nil) {
			baselineOK = false
			next := fmt.Sprintf("hive run --state-dir %q --json", stateDir)
			if baseline.Phase == integrated.SetupBaselinePROpen {
				next = fmt.Sprintf("hive approve-baseline --state-dir %q --plan --json", stateDir)
			}
			baselineMessage = fmt.Sprintf("setup baseline phase %s or its durable audit receipt is pending; next=%s", baseline.Phase, next)
		} else if baselineExists {
			baselineMessage = fmt.Sprintf("setup baselines completed full trusted production verification in run %d at %s", baseline.ProductionRunID, baseline.ProductionHeadSHA)
		}
		checks = append(checks, doctorCheck{Name: "setup_baseline_state", OK: baselineOK, Message: baselineMessage})
		uninstall, uninstallExists, uninstallErr := store.LoadUninstallIntent()
		uninstallMessage := "no uninstall cleanup is pending"
		if uninstallErr != nil {
			uninstallMessage = uninstallErr.Error()
		} else if uninstallExists {
			uninstallMessage = fmt.Sprintf("uninstall phase %s is pending; finish %s or cancel the exact unmerged cleanup with %s", uninstall.Phase, integrated.UninstallNextCommand(stateDir), integrated.UninstallCancelCommand(stateDir))
		}
		checks = append(checks, doctorCheck{Name: "uninstall_state", OK: uninstallErr == nil && !uninstallExists, Message: uninstallMessage})
		checks = append(checks, workflowDispatchDoctorCheck(store))
		checks = append(checks, operationalStateDoctorChecks(stateDir)...)
		_, gitErr := os.Stat(filepath.Join(config.CheckoutDir, ".git"))
		checks = append(checks, doctorCheck{Name: "checkout", OK: gitErr == nil, Message: errorOr(gitErr, "managed checkout is present")})
		immutable := len(config.VisualHiveRef) == 40
		checks = append(checks, doctorCheck{Name: "visual_hive_pin", OK: immutable, Message: ternary(immutable, "Visual Hive is pinned to an immutable commit", "Visual Hive ref is not immutable")})
		runtimeOK, runtimeMessage := validateVisualHiveLauncher(config)
		checks = append(checks, doctorCheck{Name: "visual_hive_runtime", OK: runtimeOK, Message: runtimeMessage})
		if config.Automation == integrated.AutomationRepairPR || config.Automation == integrated.AutomationAutoMerge {
			provider := integratedSetupCodexProvider(config.ProviderCommand, config.ProviderArgs)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			providerErr := provider.Health(ctx)
			cancel()
			checks = append(checks, doctorCheck{Name: "provider", OK: providerErr == nil, Message: errorOr(providerErr, "Codex provider is authenticated")})
		} else {
			checks = append(checks, doctorCheck{Name: "provider", OK: true, Message: "repair provider is not required for this automation level"})
		}
		if includeScheduler {
			normalHealth := inspectNormalVisualServiceHealth(stateDir, config.Repository, time.Duration(config.RunIntervalSeconds)*time.Second, time.Now().UTC())
			ownerIntent := configuredRuntimeOwnerIntent(config)
			daemon := readIntegratedDaemonStatus(stateDir)
			ownerObservation := inspectRuntimeOwner(stateDir, config, daemon)
			checks = append(checks, runtimeOwnerDoctorCheck(ownerIntent, ownerObservation))
			if ownerIntent == runtimeOwnerNormalHive {
				checks = append(checks, normalVisualServiceDoctorChecks(normalHealth)...)
			} else if ownerIntent == runtimeOwnerLegacyScheduler {
				daemonOK := daemon.RuntimeRunning && daemonServiceReady(daemon.Service)
				daemonMessage := "persistent scheduler service is installed, exact, and running"
				if ownerObservation.NormalOwnerLive {
					daemonMessage = ownerObservation.Message
				} else if daemon.Service == nil || !daemon.Service.Managed {
					if _, pending, intentErr := store.LoadSchedulerStartIntent(); intentErr != nil {
						daemonMessage = "persistent scheduler start intent is unreadable: " + intentErr.Error()
					} else if pending {
						daemonMessage = "persistent scheduler start is requested and will activate after setup and all other doctor checks are green"
					} else if ownerObservation.NormalLeaseRecordPresent {
						daemonMessage = ownerObservation.Message
					} else {
						daemonMessage = "persistent scheduler service is not installed; run hive start"
					}
				} else if daemon.Service.InspectionError != "" {
					daemonMessage = daemon.Service.InspectionError
				} else if !daemonServiceReady(daemon.Service) {
					daemonMessage = "persistent scheduler registration is incomplete or disabled; run hive start to repair it"
				} else if !daemon.RuntimeRunning {
					daemonMessage = "persistent scheduler is enabled but its runtime is still in crash-recovery backoff"
				}
				checks = append(checks, doctorCheck{Name: "persistent_scheduler", OK: daemonOK, Message: daemonMessage})
			}
		}
		var client *hivegithub.Client
		if token := resolveGitHubToken(githubTokenEnv); token != "" {
			client = hivegithub.NewClient(token, "", nil, slog.New(slog.NewTextHandler(io.Discard, nil)), githubAPIURL)
		}
		ctx, cancelLive := context.WithTimeout(context.Background(), 45*time.Second)
		checks = append(checks, liveRepositoryChecks(ctx, client, config)...)
		cancelLive()
	}
	return checks
}

func doctorChecksReady(checks []doctorCheck) bool {
	ready := true
	for _, check := range checks {
		ready = ready && check.OK
	}
	return ready
}

func resolveVisualHiveLauncher(command string, args []string, home string) (string, []string, error) {
	command = strings.TrimSpace(command)
	if command != "" {
		return command, append([]string(nil), args...), nil
	}
	if len(args) > 0 {
		node, err := exec.LookPath("node")
		if err != nil {
			return "", nil, fmt.Errorf("VISUAL_HIVE_CLI requires Node 22 or --visual-hive-command")
		}
		return node, append([]string(nil), args...), nil
	}

	homes := []string{}
	if strings.TrimSpace(home) != "" {
		homes = append(homes, home)
	}
	if executable, err := os.Executable(); err == nil {
		homes = append(homes, filepath.Join(filepath.Dir(executable), "visual-hive"))
	}
	for _, candidate := range homes {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		cli := filepath.Join(absolute, "visual-hive.mjs")
		if info, statErr := os.Stat(cli); statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		nodeName := "node"
		if runtime.GOOS == "windows" {
			nodeName = "node.exe"
		}
		node := filepath.Join(filepath.Dir(absolute), "runtime", nodeName)
		if info, statErr := os.Stat(node); statErr == nil && info.Mode().IsRegular() {
			return node, append([]string{cli}, args...), nil
		}
		if pathNode, lookErr := exec.LookPath("node"); lookErr == nil {
			return pathNode, append([]string{cli}, args...), nil
		}
	}
	if visualHive, err := exec.LookPath("visual-hive"); err == nil {
		return visualHive, append([]string(nil), args...), nil
	}
	return "", nil, fmt.Errorf("immutable Visual Hive runtime not found; install the integrated Hive bundle or pass --visual-hive-home")
}

func resolveSetupVisualHive(command string, args []string, home, configuredRef string) (string, []string, string, error) {
	resolvedCommand, resolvedArgs, err := resolveVisualHiveLauncher(command, args, home)
	if err != nil {
		return "", nil, "", err
	}
	configuredRef = strings.ToLower(strings.TrimSpace(configuredRef))
	if len(resolvedArgs) > 0 && filepath.Base(resolvedArgs[0]) == "visual-hive.mjs" {
		manifest, manifestErr := integrated.ValidateVisualHiveRelease(resolvedArgs[0], configuredRef)
		if manifestErr != nil {
			return "", nil, "", manifestErr
		}
		if configuredRef == "" {
			configuredRef = strings.ToLower(manifest.GitCommit)
		}
	}
	if len(configuredRef) != 40 {
		return "", nil, "", fmt.Errorf("Visual Hive setup requires an immutable 40-character commit SHA")
	}
	return resolvedCommand, resolvedArgs, configuredRef, nil
}

func resolveIntegratedProvider(ctx context.Context, provider, command string, args []string) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		if strings.TrimSpace(command) == "" {
			return "", fmt.Errorf("provider %q requires --provider-command", provider)
		}
		return command, nil
	}
	candidates := []string{}
	if strings.TrimSpace(command) != "" {
		candidates = append(candidates, command)
	}
	if resolved, err := exec.LookPath("codex"); err == nil {
		candidates = append(candidates, resolved)
	}
	homes := []string{}
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		homes = append(homes, codexHome)
	}
	if home, err := os.UserHomeDir(); err == nil {
		homes = append(homes, filepath.Join(home, ".codex"))
	}
	for _, home := range homes {
		name := "codex"
		if runtime.GOOS == "windows" {
			name = "codex.exe"
		}
		candidates = append(candidates, filepath.Join(home, ".sandbox-bin", name))
	}
	seen := map[string]bool{}
	errorsSeen := []string{}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[strings.ToLower(candidate)] {
			continue
		}
		seen[strings.ToLower(candidate)] = true
		resolved := candidate
		if !filepath.IsAbs(candidate) {
			if pathValue, err := exec.LookPath(candidate); err == nil {
				resolved = pathValue
			}
		}
		if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
			continue
		}
		healthCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := integratedSetupCodexProvider(resolved, args).Health(healthCtx)
		cancel()
		if err == nil {
			absolute, _ := filepath.Abs(resolved)
			return absolute, nil
		}
		errorsSeen = append(errorsSeen, fmt.Sprintf("%s: %s", resolved, err))
	}
	if len(errorsSeen) > 0 {
		return "", fmt.Errorf("no usable authenticated Codex provider was found (%s)", strings.Join(errorsSeen, "; "))
	}
	return "", fmt.Errorf("Codex provider was not found; install/authenticate Codex or pass --provider-command")
}

func integratedSetupCodexProvider(command string, args []string) repair.CodexProvider {
	return repair.CodexProvider{
		Command:   command,
		Prefix:    append([]string(nil), args...),
		CodexHome: resolveIntegratedCodexHome(),
	}
}

func validateVisualHiveLauncher(config integrated.Config) (bool, string) {
	command := config.VisualHiveCommand
	if !filepath.IsAbs(command) {
		resolved, err := exec.LookPath(command)
		if err != nil {
			return false, "Visual Hive launcher is unavailable"
		}
		command = resolved
	}
	if info, err := os.Stat(command); err != nil || !info.Mode().IsRegular() {
		return false, "Visual Hive launcher is unavailable"
	}
	if len(config.VisualHiveArgs) == 0 || filepath.Base(config.VisualHiveCommand) == "visual-hive" || filepath.Base(config.VisualHiveCommand) == "visual-hive.exe" {
		return true, "Visual Hive launcher is available"
	}
	entrypoint := config.VisualHiveArgs[0]
	if info, err := os.Stat(entrypoint); err != nil || !info.Mode().IsRegular() {
		return false, "Visual Hive release entrypoint is unavailable"
	}
	return validateVisualHiveRelease(entrypoint, config.VisualHiveRef)
}

func validateVisualHiveRelease(entrypoint, expectedCommit string) (bool, string) {
	if _, err := integrated.ValidateVisualHiveRelease(entrypoint, expectedCommit); err != nil {
		return false, err.Error()
	}
	return true, "Visual Hive release manifest matches the immutable pin"
}

func liveRepositoryChecks(ctx context.Context, client *hivegithub.Client, config integrated.Config) []doctorCheck {
	if client == nil || client.GoGitHub() == nil {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "GitHub authorization is required"}}
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return []doctorCheck{{Name: "github_auth", OK: false, Message: "configured repository is invalid"}}
	}
	checks := []doctorCheck{}
	metadata, _, repoErr := client.GoGitHub().Repositories.Get(ctx, owner, repo)
	checks = append(checks, doctorCheck{Name: "github_auth", OK: repoErr == nil, Message: errorOr(repoErr, "GitHub repository access verified")})
	if repoErr != nil {
		return checks
	}
	if metadata.GetID() > 0 && config.RepositoryID != "" && fmt.Sprintf("%d", metadata.GetID()) != config.RepositoryID {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: false, Message: "GitHub repository ID no longer matches persistent configuration"})
	} else {
		checks = append(checks, doctorCheck{Name: "repository_identity", OK: true, Message: "repository identity matches"})
	}
	setupMerged := false
	setupMessage := "setup PR is missing"
	if config.SetupPRNumber > 0 {
		pull, _, pullErr := client.GoGitHub().PullRequests.Get(ctx, owner, repo, config.SetupPRNumber)
		setupMerged = pullErr == nil && pull.GetMerged()
		setupMessage = errorOr(pullErr, ternary(setupMerged, "setup PR is merged", "setup PR has not been merged"))
	}
	installedErr := integrated.VerifyInstalledSetup(ctx, client, config)
	setupReady := installedErr == nil
	if installedErr == nil {
		if !setupMerged {
			setupMessage = "exact managed setup is installed on the target branch; stale historical PR metadata is ignored"
		} else {
			setupMessage = "setup PR is merged and every managed production file matches the durable policy"
		}
	} else {
		setupMessage += "; exact target verification failed: " + installedErr.Error()
	}
	checks = append(checks, doctorCheck{Name: "setup_installed", OK: setupReady, Message: setupMessage})
	content, _, _, workflowErr := client.GoGitHub().Repositories.GetContents(ctx, owner, repo, ".github/workflows/hive-visual-hive.yml", &gh.RepositoryContentGetOptions{Ref: config.DefaultBranch})
	checks = append(checks, doctorCheck{Name: "workflow_installed", OK: workflowErr == nil && content != nil, Message: errorOr(workflowErr, "production workflow is installed on the target branch")})
	visualRefErr := integrated.VerifyVisualHiveWorkflowCommits(ctx, client, config.VisualHiveRepo, config.VisualHiveRef)
	checks = append(checks, doctorCheck{Name: "visual_hive_ref_exists", OK: visualRefErr == nil, Message: errorOr(visualRefErr, "Visual Hive production and audited PR pins resolve to exact remote commits")})
	if config.Automation == integrated.AutomationAutoMerge {
		if !setupReady {
			checks = append(checks, autoMergeProtectionDoctorCheck(false, config, hivegithub.BranchProtectionSummary{}, 0, integrated.ProtectionActivationState{}, false, nil))
			return checks
		}
		protection, protectionErr := client.BranchProtection(ctx, config.Repository, config.DefaultBranch)
		expectedAppID, expectedAppErr := client.ExpectedGitHubAppID(ctx, "github-actions")
		activationState, activationExists, activationStoreErr := integrated.ReadProtectionActivation(config.StateDir)
		liveErr := protectionErr
		if liveErr == nil {
			liveErr = expectedAppErr
		}
		if liveErr == nil {
			liveErr = activationStoreErr
		}
		checks = append(checks, autoMergeProtectionDoctorCheck(true, config, protection, expectedAppID, activationState, activationExists, liveErr))
	}
	return checks
}

func autoMergeProtectionDoctorCheck(setupReady bool, config integrated.Config, protection hivegithub.BranchProtectionSummary, expectedAppID int64, activation integrated.ProtectionActivationState, activationExists bool, liveErr error) doctorCheck {
	if !setupReady {
		return doctorCheck{Name: "branch_protection", OK: false, Message: "setup activation is pending: merge the exact managed setup/upgrade PR; the next started Hive run will complete a trusted default-branch scan before activating protection"}
	}
	visualRequired := false
	for _, check := range protection.RequiredCheckIdentities {
		if strings.EqualFold(strings.TrimSpace(check.Context), "visual-hive") && check.AppID == expectedAppID && expectedAppID > 0 {
			visualRequired = true
			break
		}
	}
	activationDurable := activationExists && liveErr == nil && integrated.ProtectionActivationMatchesConfig(activation, config) && activation.CheckAppID == expectedAppID
	protected := liveErr == nil && protection.Enabled && protection.Strict && protection.AdminEnforced && visualRequired && activationDurable
	activationMessage := ""
	switch {
	case liveErr != nil:
		activationMessage = liveErr.Error()
	case !protection.Enabled:
		activationMessage = "managed setup is installed; protection activation is pending the next trusted Hive run"
	case !protection.Strict || !protection.AdminEnforced || !visualRequired:
		activationMessage = fmt.Sprintf("existing repository policy blocks activation and Hive will not replace it: require strict checks, enforce administrators, and bind visual-hive to GitHub Actions App ID %d", expectedAppID)
	case !activationDurable:
		activationMessage = "live protection is exact, but durable trusted-run activation is pending; run hive run"
	default:
		activationMessage = fmt.Sprintf("protection activated and verified for visual-hive from GitHub Actions App ID %d", expectedAppID)
	}
	message := fmt.Sprintf("%s; protection=%t strict=%t admin_enforced=%t visual_hive_app_id=%d visual_hive_required=%t activation_durable=%t required_checks=%v required_check_identities=%v required_reviews=%d", activationMessage, protection.Enabled, protection.Strict, protection.AdminEnforced, expectedAppID, visualRequired, activationDurable, protection.RequiredChecks, protection.RequiredCheckIdentities, protection.RequiredReviews)
	return doctorCheck{Name: "branch_protection", OK: protected, Message: message}
}

func runIntegratedPause(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	requestID := flags.String("request-id", "", "exact hosted idempotency key used only when resuming an ambiguous dispatch")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if _, hosted, loadErr := hostedConfig(*stateDir); loadErr == nil && hosted {
		return dispatchHostedOperation(*stateDir, command, *requestID, *githubTokenEnv, *githubAPIURL, *jsonOutput)
	}
	priorDaemon := readIntegratedDaemonStatus(*stateDir)
	_, _, normalRuntimeRunning := normalVisualDaemonObserved(*stateDir)
	store, storeErr := integrated.NewStore(filepath.Join(*stateDir, "integrated"))
	if storeErr != nil {
		fmt.Fprintln(os.Stderr, storeErr)
		return 1
	}
	priorConfig, loadErr := store.Load()
	if loadErr != nil {
		fmt.Fprintln(os.Stderr, loadErr)
		return 1
	}
	if command == "resume" && configuredRuntimeOwnerIntent(priorConfig) == runtimeOwnerLegacyScheduler {
		if ownerErr := preflightLegacySchedulerStart(*stateDir, priorConfig); ownerErr != nil {
			fmt.Fprintln(os.Stderr, "Hive authority remains paused because its runtime owner must transition first:", ownerErr)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()
	config, err := integrated.SetPaused(ctx, *stateDir, command == "pause")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	normalRuntimeIntent := configuredRuntimeOwnerIntent(config) == runtimeOwnerNormalHive
	runtimeRunning := priorDaemon.RuntimeRunning
	if normalRuntimeIntent {
		runtimeRunning = normalRuntimeRunning
	}
	if command == "pause" {
		legacySchedulerObserved := priorDaemon.RuntimeRunning || priorDaemon.Service != nil && priorDaemon.Service.Managed
		if !normalRuntimeIntent || legacySchedulerObserved {
			if _, err := stopIntegratedDaemon(*stateDir); err != nil {
				fmt.Fprintln(os.Stderr, "Hive is safely paused, but scheduler shutdown failed:", err)
				return 1
			}
		}
	} else {
		if !normalRuntimeIntent {
			interval := 15 * time.Minute
			if priorDaemon.IntervalSeconds >= 60 {
				interval = time.Duration(priorDaemon.IntervalSeconds) * time.Second
			}
			started, err := ensureIntegratedDaemonStarted(*stateDir, interval)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Hive authority resumed, but scheduler restart failed:", err)
				return 1
			}
			runtimeRunning = started.RuntimeRunning
		}
	}
	ownerObservation := inspectRuntimeOwner(*stateDir, config, readIntegratedDaemonStatus(*stateDir))
	if *jsonOutput {
		response := map[string]any{
			"repository": config.Repository, "paused": config.Paused,
			"runtime_owner": ownerObservation.ObservedOwner, "runtime_running": runtimeRunning,
		}
		for key, value := range runtimeOwnerStatusFields(configuredRuntimeOwnerIntent(config), ownerObservation) {
			response[key] = value
		}
		return encodeJSON(response)
	}
	fmt.Printf("Hive automation for %s is %s.\n", config.Repository, ternary(config.Paused, "paused", "active"))
	if command == "resume" && normalRuntimeIntent && !normalRuntimeRunning {
		fmt.Println(ownerObservation.Message + " No legacy scheduler was started.")
	}
	return 0
}

func runIntegratedSetting(command string, args []string) int {
	flags := flag.NewFlagSet("hive "+command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.String("value", "", "new setting value")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil || *value == "" {
		fmt.Fprintln(os.Stderr, "--value is required")
		return 2
	}
	setupFlag := "--automation"
	if command == "set-coverage" {
		candidate := integrated.Coverage(*value)
		if candidate != integrated.CoverageEssential && candidate != integrated.CoverageStandard && candidate != integrated.CoverageComprehensive && candidate != integrated.CoverageCustom {
			fmt.Fprintln(os.Stderr, "invalid coverage")
			return 2
		}
		setupFlag = "--coverage"
	} else {
		candidate := integrated.Automation(*value)
		if candidate != integrated.AutomationAdvisory && candidate != integrated.AutomationIssues && candidate != integrated.AutomationRepairPR && candidate != integrated.AutomationAutoMerge {
			fmt.Fprintln(os.Stderr, "invalid automation")
			return 2
		}
	}
	return runSetupSetting(*stateDir, setupFlag, *value, *jsonOutput, *githubTokenEnv, *githubAPIURL)
}

func runIntegratedIssueLimit(args []string) int {
	flags := flag.NewFlagSet("hive set-issue-limit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.Int("value", 0, "maximum concurrently open Hive-managed findings (1-100)")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if *value < 1 || *value > 100 {
		fmt.Fprintln(os.Stderr, "--value must be from 1 through 100")
		return 2
	}
	return runSetupSetting(*stateDir, "--max-active-issues", fmt.Sprint(*value), *jsonOutput, *githubTokenEnv, *githubAPIURL)
}

func runIntegratedRetryLimit(args []string) int {
	flags := flag.NewFlagSet("hive set-retry-limit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateDir := flags.String("state-dir", defaultIntegratedStateDir(), "persistent Hive state directory")
	value := flags.Int("value", 0, "maximum bounded model-backed repair attempts per finding (1-10)")
	jsonOutput := flags.Bool("json", false, "emit machine-readable JSON")
	githubTokenEnv := flags.String("github-token-env", "HIVE_GITHUB_TOKEN", "environment variable containing GitHub token")
	githubAPIURL := flags.String("github-api-url", "", "optional GitHub Enterprise API URL")
	if err := parseExactFlags(flags, args); err != nil {
		return 2
	}
	if *value < 1 || *value > 10 {
		fmt.Fprintln(os.Stderr, "--value must be from 1 through 10")
		return 2
	}
	return runSetupSetting(*stateDir, "--max-repair-attempts", fmt.Sprint(*value), *jsonOutput, *githubTokenEnv, *githubAPIURL)
}

func runSetupSetting(stateDir, flagName, value string, jsonOutput bool, githubTokenEnv, githubAPIURL string) int {
	args := []string{"--state-dir", stateDir, flagName, value, "--github-token-env", githubTokenEnv}
	if githubAPIURL != "" {
		args = append(args, "--github-api-url", githubAPIURL)
	}
	if jsonOutput {
		args = append(args, "--json")
	}
	return runSetupCommand(args)
}

func resolveGitHubToken(envName string) string {
	if token := strings.TrimSpace(os.Getenv(envName)); token != "" {
		return token
	}
	return resolveGitHubCLIToken()
}

var githubCLIToken = func() string {
	command := exec.Command("gh", "auth", "token")
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func resolveGitHubCLIToken() string {
	return githubCLIToken()
}

func requirePersistentGitHubAuthorization(start bool) error {
	if !start || resolveGitHubCLIToken() != "" {
		return nil
	}
	return fmt.Errorf("persistent --start requires GitHub CLI credential-store authorization; run gh auth login, verify gh auth token succeeds, then rerun setup (environment-only tokens are deliberately not copied into the service)")
}

func defaultIntegratedStateDir() string {
	if value := os.Getenv("HIVE_STATE_DIR"); value != "" {
		return value
	}
	root := integratedStateRoot()
	if current, ok, err := integrated.CurrentState(root); err == nil && ok {
		return current
	}
	return filepath.Join(root, "repositories", "default")
}

func integratedStateRoot() string {
	if configured := strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err == nil {
			return filepath.Clean(absolute)
		}
		return filepath.Clean(configured)
	}
	if config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "" {
		return hostedIntegratedStateRoot
	}
	home, err := os.UserHomeDir()
	if err != nil {
		absolute, absoluteErr := filepath.Abs(".hive")
		if absoluteErr == nil {
			return absolute
		}
		return ".hive"
	}
	return filepath.Join(home, ".hive")
}

func validateHostedIntegratedStateRoot() error {
	if !(config.IsKubernetesPod() && strings.TrimSpace(os.Getenv("HIVE_ID")) != "") {
		return nil
	}
	root := filepath.Clean(hostedIntegratedStateRoot)
	parent := filepath.Dir(root)
	if configured := strings.TrimSpace(os.Getenv("HIVE_STATE_DIR")); configured != "" {
		absolute, err := filepath.Abs(configured)
		if err != nil {
			return fmt.Errorf("resolve explicit hosted HIVE_STATE_DIR: %w", err)
		}
		root = filepath.Clean(absolute)
		relative, relErr := filepath.Rel(parent, root)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return fmt.Errorf("explicit hosted HIVE_STATE_DIR %s must remain on persistent %s", root, parent)
		}
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("hosted Visual Hive persistent state parent %s is unavailable: %w", parent, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("hosted Visual Hive persistent state parent %s must be a real directory", parent)
	}
	relative, _ := filepath.Rel(parent, root)
	cursor := parent
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		cursor = filepath.Join(cursor, component)
		rootInfo, rootErr := os.Lstat(cursor)
		if os.IsNotExist(rootErr) {
			continue
		}
		if rootErr != nil {
			return fmt.Errorf("inspect hosted Visual Hive persistent state path %s: %w", cursor, rootErr)
		}
		if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
			return fmt.Errorf("hosted Visual Hive persistent state path %s must be a real directory", cursor)
		}
	}
	return nil
}

func repositoryIntegratedStateDir(repository string) (string, error) {
	if err := validateHostedIntegratedStateRoot(); err != nil {
		return "", err
	}
	legacy := legacyRepositoryIntegratedStateDir(repository)
	prior, exists, err := loadExistingSetupConfig(legacy)
	if err != nil {
		return "", fmt.Errorf("inspect existing legacy state at %s: %w; rerun with that exact path through --state-dir after resolving the reported state error", legacy, err)
	}
	if exists && strings.EqualFold(strings.TrimSpace(prior.Repository), strings.TrimSpace(repository)) {
		if prior.StateDir != "" && !strings.EqualFold(filepath.Clean(prior.StateDir), filepath.Clean(legacy)) {
			return "", fmt.Errorf("legacy state at %s embeds a different state path %s; use the original exact --state-dir so Hive can verify it", legacy, prior.StateDir)
		}
		return legacy, nil
	}

	canonical := strings.ToLower(strings.TrimSpace(repository))
	_, repoName, ok := strings.Cut(canonical, "/")
	if !ok {
		repoName = canonical
	}
	short := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, repoName)
	short = strings.Trim(short, "-")
	if len(short) > 20 {
		short = strings.TrimRight(short[:20], "-")
	}
	if short == "" {
		short = "repository"
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	selected := filepath.Join(integratedStateRoot(), "repos", short+"-"+digest[:16])
	if prior, exists, err := loadExistingSetupConfig(selected); err != nil {
		return "", fmt.Errorf("inspect existing repository state at %s: %w", selected, err)
	} else if exists {
		if !strings.EqualFold(strings.TrimSpace(prior.Repository), strings.TrimSpace(repository)) {
			return "", fmt.Errorf("repository state at %s belongs to %s, not %s", selected, prior.Repository, repository)
		}
		if prior.StateDir != "" && !strings.EqualFold(filepath.Clean(prior.StateDir), filepath.Clean(selected)) {
			return "", fmt.Errorf("repository state at %s embeds a different state path %s; use the original exact --state-dir so Hive can verify it", selected, prior.StateDir)
		}
		return selected, nil
	}
	entries, readErr := os.ReadDir(selected)
	if os.IsNotExist(readErr) || (readErr == nil && len(entries) == 0) {
		return selected, nil
	}
	if readErr != nil {
		return "", fmt.Errorf("inspect unowned repository state at %s: %w", selected, readErr)
	}

	// Older read-only setup plans cloned into the auto-selected durable path
	// before a repository ID was available, leaving a markerless state root
	// that a later apply must never adopt. Preserve that directory untouched
	// and select one deterministic sibling for the real managed installation.
	recovery := selected + "-managed"
	if prior, exists, err := loadExistingSetupConfig(recovery); err != nil {
		return "", fmt.Errorf("inspect recovered repository state at %s: %w", recovery, err)
	} else if exists {
		if !strings.EqualFold(strings.TrimSpace(prior.Repository), strings.TrimSpace(repository)) {
			return "", fmt.Errorf("recovered repository state at %s belongs to %s, not %s", recovery, prior.Repository, repository)
		}
		if prior.StateDir != "" && !strings.EqualFold(filepath.Clean(prior.StateDir), filepath.Clean(recovery)) {
			return "", fmt.Errorf("recovered repository state at %s embeds a different state path %s; use the original exact --state-dir so Hive can verify it", recovery, prior.StateDir)
		}
		return recovery, nil
	}
	recoveryEntries, recoveryErr := os.ReadDir(recovery)
	if os.IsNotExist(recoveryErr) || (recoveryErr == nil && len(recoveryEntries) == 0) {
		return recovery, nil
	}
	if recoveryErr != nil {
		return "", fmt.Errorf("inspect recovered repository state at %s: %w", recovery, recoveryErr)
	}
	return "", fmt.Errorf("both auto-selected repository state paths are nonempty and unowned; preserve them and rerun with an explicitly reviewed dedicated --state-dir")
}

func legacyRepositoryIntegratedStateDir(repository string) string {
	name := strings.ToLower(strings.TrimSpace(repository))
	name = strings.NewReplacer("/", "--", "\\", "--").Replace(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-")
	if name == "" {
		name = "default"
	}
	return filepath.Join(integratedStateRoot(), "repositories", name)
}

func isInteractiveTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func prompt(reader *bufio.Reader, label string) string {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	value, _ := reader.ReadString('\n')
	return strings.TrimSpace(value)
}

func acmmForIntegratedAutomation(value integrated.Automation) int {
	switch value {
	case integrated.AutomationAdvisory:
		return 2
	case integrated.AutomationIssues:
		return 4
	case integrated.AutomationRepairPR:
		return 5
	case integrated.AutomationAutoMerge:
		return 6
	default:
		return 1
	}
}

func automationModeForIntegrated(value integrated.Automation) automation.Mode {
	switch value {
	case integrated.AutomationIssues:
		return automation.ModeIssues
	case integrated.AutomationRepairPR:
		return automation.ModeRepairPR
	case integrated.AutomationAutoMerge:
		return automation.ModeAutoMerge
	default:
		return automation.ModeAdvisory
	}
}

func valueOrEnv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func errorOr(err error, success string) string {
	if err != nil {
		return err.Error()
	}
	return success
}

func ternary[T any](condition bool, yes, no T) T {
	if condition {
		return yes
	}
	return no
}

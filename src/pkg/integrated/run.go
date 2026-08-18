package integrated

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/internal/gittransport"
	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/automation"
	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/checkpoint"
	hivegithub "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/repair"
	"github.com/kubestellar/hive/pkg/visualhive"
)

type RunOptions struct {
	StateDir string
	Timeout  time.Duration
	GitHub   *hivegithub.Client
	// VisualHiveGitHub is the optional least-privilege App client used only for
	// workflow dispatch and setup-status publication. Repository reads and all
	// lifecycle writes remain on GitHub. Nil preserves standalone/legacy mode.
	VisualHiveGitHub  *hivegithub.Client
	GitTransportToken string
	Specialists       repair.SpecialistDispatcher
	SpecialistWorkDir string
}

type WorkflowRunEvidence struct {
	CorrelationID         string                            `json:"correlation_id"`
	RunID                 int64                             `json:"run_id"`
	RunURL                string                            `json:"run_url"`
	HeadSHA               string                            `json:"head_sha"`
	Conclusion            string                            `json:"conclusion"`
	EvidenceArtifact      int64                             `json:"evidence_artifact_id"`
	BundleArtifact        int64                             `json:"bundle_artifact_id"`
	RepositoryTests       []visualhive.RepositoryTestResult `json:"repository_tests"`
	RepositoryTestOverall int                               `json:"repository_test_overall"`
	RepositoryTestDigest  string                            `json:"repository_test_digest"`
}

type RunResult struct {
	SchemaVersion        string                           `json:"schema_version"`
	Repository           string                           `json:"repository"`
	Workflow             WorkflowRunEvidence              `json:"workflow"`
	ProtectionActivation *ProtectionActivationResult      `json:"protection_activation,omitempty"`
	PostMergeWorkflow    *WorkflowRunEvidence             `json:"post_merge_workflow,omitempty"`
	Validation           visualhive.Validation            `json:"validation"`
	Lifecycle            visualhive.ApplyLifecycleResult  `json:"lifecycle"`
	PostMergeLifecycle   *visualhive.ApplyLifecycleResult `json:"post_merge_lifecycle,omitempty"`
	Outbox               visualhive.OutboxProcessorResult `json:"outbox"`
	Repairs              []repair.Result                  `json:"repairs,omitempty"`
	Gates                []GateEvaluation                 `json:"gates,omitempty"`
	StartedAt            time.Time                        `json:"started_at"`
	CompletedAt          time.Time                        `json:"completed_at"`
}

type GateEvaluation struct {
	RepositoryFingerprint string                     `json:"repository_fingerprint"`
	Purpose               string                     `json:"purpose,omitempty"`
	Gate                  hivegithub.PullRequestGate `json:"gate"`
	Decision              *automation.Decision       `json:"merge_decision,omitempty"`
	Approval              *MergeApproval             `json:"merge_approval,omitempty"`
	MergeSHA              string                     `json:"merge_sha,omitempty"`
}

const staleWorkflowRedispatchLimit = 3

type staleWorkflowHeadError struct {
	WorkflowHead string
	CurrentHead  string
}

func (e *staleWorkflowHeadError) Error() string {
	return fmt.Sprintf("completed Visual Hive workflow head %s is stale; current default head is %s", e.WorkflowHead, e.CurrentHead)
}

func RunOnce(ctx context.Context, options RunOptions) (RunResult, error) {
	ctx = gittransport.WithControllerToken(ctx, options.GitTransportToken)
	ctx = WithVisualHiveGitHubClients(ctx, options.VisualHiveGitHub, options.VisualHiveGitHub)
	result := RunResult{SchemaVersion: "hive.production-run.v1", StartedAt: time.Now().UTC()}
	if options.GitHub == nil || options.StateDir == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	if (options.Specialists == nil) != (strings.TrimSpace(options.SpecialistWorkDir) == "") {
		return result, fmt.Errorf("specialist dispatcher and absolute specialist work directory must be configured together")
	}
	if options.Specialists != nil && !filepath.IsAbs(options.SpecialistWorkDir) {
		return result, fmt.Errorf("specialist work directory must be absolute")
	}
	if options.Timeout <= 0 {
		options.Timeout = 45 * time.Minute
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	releaseRun, err := acquireProductionRunLease(options.StateDir, options.Timeout+2*time.Minute)
	if err != nil {
		return result, err
	}
	defer releaseRun()
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	if config.ExecutionMode == ExecutionHosted && !checkpoint.Active(ctx) {
		return result, fmt.Errorf("hosted installation lifecycle is owned by the GitHub controller; dispatch it instead of running a local lifecycle writer")
	}
	if transfer, exists, transferErr := store.LoadAuthorizerTransferIntent(); transferErr != nil {
		return result, transferErr
	} else if exists {
		detail := fmt.Sprintf("setup authorizer transfer to %s/%d is pending; next=%s", transfer.NewAuthorizerLogin, transfer.NewAuthorizerID, AuthorizerTransferNextCommand(options.StateDir, transfer))
		if auditErr := store.AuditStrict(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: detail}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("%s", detail)
	}
	if _, err := verifyLiveRepositoryIdentity(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	if err := VerifyInstalledSetup(ctx, options.GitHub, config); err != nil {
		return result, err
	}
	result.Repository = config.Repository
	policy := integratedPolicy(config)
	pauseRequested, pauseRequestErr := PauseRequested(options.StateDir)
	if pauseRequestErr != nil {
		return result, fmt.Errorf("read pause request: %w", pauseRequestErr)
	}
	if config.Paused || pauseRequested {
		if auditErr := store.AuditStrict(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: "repository automation is paused"}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("repository automation is paused")
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, options.Timeout)
	defer timeoutCancel()
	runCtx, pauseCancel := context.WithCancel(timeoutCtx)
	defer pauseCancel()
	go watchPauseRequest(runCtx, options.StateDir, pauseCancel)
	if intent, blocked, reconcileErr := reconcileRequiredSetupBaselineBeforeRun(runCtx, store, config, options.GitHub); reconcileErr != nil {
		detail := reconcileErr.Error()
		if blocked {
			detail = fmt.Sprintf("setup baseline phase %s blocks production: %s", intent.Phase, detail)
		}
		if auditErr := store.AuditStrict(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: detail}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("%s", detail)
	} else if blocked {
		detail := fmt.Sprintf("setup baseline phase %s blocks production; next=%s", intent.Phase, setupBaselinePlanCommand(options.StateDir, intent))
		if auditErr := store.AuditStrict(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: detail}); auditErr != nil {
			return result, auditErr
		}
		return result, fmt.Errorf("%s", detail)
	}
	if err := store.AuditStrict(AuditEntry{Action: "run", Allowed: true, Repository: config.Repository}); err != nil {
		return result, err
	}
	workflow := WorkflowRunEvidence{}
	if config.Automation == AutomationAutoMerge {
		workflow, err = dispatchAndWait(runCtx, options.GitHub, config)
		if err != nil {
			return result, err
		}
		result.Workflow = workflow
		activation, activationErr := ActivateAutoMergeProtection(runCtx, store, options.GitHub, config, workflow)
		result.ProtectionActivation = &activation
		if activationErr != nil {
			if errors.Is(activationErr, ErrProtectionActivationRunStale) {
				if consumeErr := consumeWorkflowDispatch(options.StateDir, workflow); consumeErr != nil {
					return result, fmt.Errorf("%v; discard stale activation workflow: %w", activationErr, consumeErr)
				}
				if auditErr := store.AuditStrict(AuditEntry{Action: "discard_stale_protection_activation_run", Allowed: false, Repository: config.Repository, Detail: activationErr.Error()}); auditErr != nil {
					return result, auditErr
				}
			}
			return result, activationErr
		}
	}
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive"))
	if err != nil {
		return result, err
	}
	if config.Automation == AutomationAdvisory {
		if _, err := lifecycle.CancelPendingIssuePublication("automation authority is advisory"); err != nil {
			return result, fmt.Errorf("cancel pending issue publication for advisory authority: %w", err)
		}
	}
	if err := prepareRepairRetirements(store, config, lifecycle); err != nil {
		return result, fmt.Errorf("prepare durable repair branch retirements: %w", err)
	}
	if err := reconcileStaleMergeApproval(runCtx, options.StateDir, config, lifecycle, options.GitHub); err != nil {
		return result, err
	}
	// Reconcile an unconsumed exact merge intent before retiring its branch. A
	// crash may occur after MarkMerged but before intent consumption; the live
	// PR identity, writer, head, diff, and merge SHA must be re-proven first.
	externalMerge, err := reconcileExternallyMergedRepair(runCtx, options.StateDir, config, lifecycle, options.GitHub, policy)
	if err != nil {
		return result, err
	}
	if err := reconcileRepairRetirements(runCtx, store, config, lifecycle, options.GitHub, policy, false); err != nil {
		return result, fmt.Errorf("reconcile durable repair branch retirements before production evidence: %w", err)
	}
	if _, err := recoverCompletedPostMergeVerification(lifecycle); err != nil {
		return result, err
	}
	if err := reconcileApprovedBaselineBranches(runCtx, options.StateDir, config, lifecycle, options.GitHub, policy); err != nil {
		return result, err
	}
	if err := reconcileOpenRepairDuplicates(runCtx, store, config, lifecycle, options.GitHub, policy); err != nil {
		return result, err
	}
	retiredRepairVerificationOnly := pendingRetiredRepairVerification(lifecycle.Snapshot())
	if config.Automation != AutomationAutoMerge {
		workflow, err = dispatchAndWait(runCtx, options.GitHub, config)
		if err != nil {
			return result, err
		}
		result.Workflow = workflow
	}
	beadStore, err := beads.NewStore(filepath.Join(options.StateDir, "beads", "quality"))
	if err != nil {
		return result, err
	}
	var validation visualhive.Validation
	var apply visualhive.ApplyLifecycleResult
	var outbox visualhive.OutboxProcessorResult
	var verifiedArtifact hivegithub.VerifiedVisualHiveArtifact
	for staleAttempt := 0; ; staleAttempt++ {
		applyOptions := visualhive.ApplyLifecycleOptions{RetiredRepairVerificationOnly: retiredRepairVerificationOnly}
		postMergeFingerprint := ""
		if externalMerge {
			finding, ok := mergedRepairFinding(lifecycle.Snapshot())
			if !ok {
				return result, fmt.Errorf("post-merge verification has no matching reconciled merge")
			}
			applyOptions, err = verifyPostMergeTarget(runCtx, options.StateDir, config, finding, workflow.HeadSHA, options.GitHub)
			if err != nil {
				return result, err
			}
			applyOptions.RetiredRepairVerificationOnly = retiredRepairVerificationOnly
			if err := lifecycle.MarkPostMergeVerifying(finding.RepositoryFingerprint, fmt.Sprintf("%d", workflow.RunID), workflow.RunURL); err != nil {
				return result, err
			}
			postMergeFingerprint = finding.RepositoryFingerprint
		}
		validation, apply, outbox, verifiedArtifact, err = applyWorkflowEvidence(runCtx, options.StateDir, config, workflow, lifecycle, beadStore, options.GitHub, policy, applyOptions)
		if err == nil {
			if err := consumeWorkflowDispatch(options.StateDir, workflow); err != nil {
				return result, fmt.Errorf("consume exact workflow dispatch: %w", err)
			}
			break
		}
		var stale *staleWorkflowHeadError
		if !errors.As(err, &stale) {
			return result, err
		}
		if postMergeFingerprint != "" {
			if resetErr := lifecycle.ResetStalePostMergeVerification(postMergeFingerprint, fmt.Sprintf("%d", workflow.RunID), workflow.HeadSHA); resetErr != nil {
				return result, fmt.Errorf("reset stale post-merge verification: %w", resetErr)
			}
		}
		if discardErr := discardStaleWorkflowDispatch(options.StateDir, config, workflow, stale); discardErr != nil {
			return result, discardErr
		}
		if staleAttempt+1 >= staleWorkflowRedispatchLimit {
			return result, fmt.Errorf("default branch advanced during %d consecutive Visual Hive evidence applications: %w", staleWorkflowRedispatchLimit, err)
		}
		workflow, err = dispatchAndWait(runCtx, options.GitHub, config)
		if err != nil {
			return result, err
		}
		result.Workflow = workflow
	}
	result.Validation, result.Lifecycle, result.Outbox = validation, apply, outbox
	if externalMerge {
		workflowCopy, lifecycleCopy := workflow, apply
		result.PostMergeWorkflow, result.PostMergeLifecycle = &workflowCopy, &lifecycleCopy
		if _, err := recoverCompletedPostMergeVerification(lifecycle); err != nil {
			return result, err
		}
	}
	if retiredRepairVerificationOnly {
		if err := store.AuditStrict(AuditEntry{
			Action: "retired_repair_verification", Allowed: true, Repository: config.Repository,
			Detail: "authoritative evidence reconciled retired repair state; new repair orchestration was deferred to a later production cycle",
		}); err != nil {
			return result, err
		}
	} else if config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge {
		orchestration, orchestrationErr := orchestrateRepairs(runCtx, options.StateDir, config, lifecycle, beadStore, options.GitHub, policy, verifiedArtifact, options.Specialists, options.SpecialistWorkDir)
		result.Repairs, result.Gates = orchestration.Repairs, orchestration.Gates
		result.PostMergeWorkflow, result.PostMergeLifecycle = orchestration.PostMergeWorkflow, orchestration.PostMergeLifecycle
		result.Outbox.Succeeded += orchestration.Outbox.Succeeded
		result.Outbox.Failed += orchestration.Outbox.Failed
		result.Outbox.Processed += orchestration.Outbox.Processed
		result.Outbox.Denied += orchestration.Outbox.Denied
		result.Outbox.StaleSkipped += orchestration.Outbox.StaleSkipped
		result.Outbox.Errors = append(result.Outbox.Errors, orchestration.Outbox.Errors...)
		if orchestrationErr != nil {
			return result, orchestrationErr
		}
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}

func pendingRetiredRepairVerification(state visualhive.LifecycleState) bool {
	for _, finding := range state.Findings {
		if finding == nil || finding.LastRetiredRepair == nil || finding.Status != visualhive.StatusIssueOpen {
			continue
		}
		if !finding.LastSeenAt.After(finding.LastRetiredRepair.RetiredAt) {
			return true
		}
	}
	return false
}

func reconcileApprovedBaselineBranches(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) error {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return err
	}
	snapshot := state.Snapshot()
	keys := make([]string, 0, len(snapshot.Attempts))
	for key := range snapshot.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attempt := snapshot.Attempts[key]
		if attempt == nil || attempt.BaselineReview == nil || attempt.BaselineReview.Status != repair.BaselineReviewApproved || attempt.BaselineReview.ProposalBranch == "" || attempt.BaselineReview.ProposalCommitSHA == "" {
			continue
		}
		finding, exists := lifecycle.Finding(key)
		if !exists {
			return fmt.Errorf("approved baseline branch lost its finding lifecycle")
		}
		decision := policy.Authorize(automation.ActionRequest{Action: automation.ActionDeleteBaselineBranch, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository, RepairAttempts: finding.RepairAttempts})
		lifecycle.RecordAuthorization(key, string(automation.ActionDeleteBaselineBranch), decision.Allowed, strings.Join(decision.Reasons, "; "))
		if !decision.Allowed {
			return fmt.Errorf("delete approved baseline branch %s denied: %s", attempt.BaselineReview.ProposalBranch, strings.Join(decision.Reasons, "; "))
		}
		deleted, err := client.DeleteBaselineBranchExact(ctx, config.Repository, attempt.BaselineReview.ProposalBranch, attempt.BaselineReview.ProposalCommitSHA)
		if err != nil {
			return err
		}
		if deleted {
			lifecycle.RecordAuthorization(key, "approved_baseline_branch_reconciled", true, fmt.Sprintf("deleted %s at exact reviewed head %s from proposal PR #%d", attempt.BaselineReview.ProposalBranch, attempt.BaselineReview.ProposalCommitSHA, attempt.BaselineReview.ProposalPRNumber))
		}
	}
	legacy, err := client.ListMergedBaselineBranches(ctx, config.Repository, config.DefaultBranch)
	if err != nil {
		return err
	}
	for _, proposal := range legacy {
		actor, attempts := "quality", 0
		if finding, exists := lifecycle.Finding(proposal.RepositoryFingerprint); exists {
			actor, attempts = repairActor(finding.OwningAgentHint), finding.RepairAttempts
		}
		decision := policy.Authorize(automation.ActionRequest{Action: automation.ActionDeleteBaselineBranch, Agent: actor, Repository: config.Repository, RepairAttempts: attempts})
		lifecycle.RecordAuthorization(proposal.RepositoryFingerprint, string(automation.ActionDeleteBaselineBranch), decision.Allowed, fmt.Sprintf("legacy proposal PR #%d: %s", proposal.PRNumber, strings.Join(decision.Reasons, "; ")))
		if !decision.Allowed {
			return fmt.Errorf("delete legacy baseline branch %s denied: %s", proposal.Branch, strings.Join(decision.Reasons, "; "))
		}
		deleted, err := client.DeleteBaselineBranchExact(ctx, config.Repository, proposal.Branch, proposal.HeadSHA)
		if err != nil {
			return err
		}
		if deleted {
			lifecycle.RecordAuthorization(proposal.RepositoryFingerprint, "legacy_baseline_branch_reconciled", true, fmt.Sprintf("deleted %s at exact merged proposal head %s from PR #%d", proposal.Branch, proposal.HeadSHA, proposal.PRNumber))
		}
	}
	return nil
}

func reconcileOpenRepairDuplicates(ctx context.Context, store *Store, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) error {
	state := lifecycle.Snapshot()
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.PRNumber <= 0 || finding.RepairCommitSHA == "" || finding.Branch == "" || finding.MergeSHA != "" {
			continue
		}
		marker := fmt.Sprintf("<!-- hive-repair: %s -->", finding.RepositoryFingerprint)
		pulls, err := client.ListOpenRepairPullRequests(ctx, config.Repository, config.DefaultBranch, marker)
		if err != nil {
			return err
		}
		if len(pulls) <= 1 {
			continue
		}
		currentFound := false
		for _, pull := range pulls {
			if pull.Number == finding.PRNumber && pull.Branch == finding.Branch && pull.HeadSHA == finding.RepairCommitSHA {
				currentFound = true
				break
			}
		}
		if !currentFound {
			return fmt.Errorf("multiple open repair PRs exist for %s but none matches the exact durable current repair", finding.RepositoryFingerprint)
		}
		for _, pull := range pulls {
			if pull.Number == finding.PRNumber {
				continue
			}
			if err := enqueueSupersededRepairRetirement(store, config, *finding, pull.Number, pull.Branch, pull.HeadSHA); err != nil {
				return err
			}
			if err := reconcileRepairRetirements(ctx, store, config, lifecycle, client, policy, false); err != nil {
				return err
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "duplicate_repair_reconciled", true, fmt.Sprintf("durably retired superseded PR #%d and %s at %s; retained PR #%d", pull.Number, pull.Branch, pull.HeadSHA, finding.PRNumber))
		}
	}
	return nil
}

func repairAttemptLimit(config Config) int {
	if config.MaxRepairAttempts < 1 {
		return 3
	}
	return config.MaxRepairAttempts
}

type repairOrchestrationResult struct {
	Repairs            []repair.Result
	Gates              []GateEvaluation
	PostMergeWorkflow  *WorkflowRunEvidence
	PostMergeLifecycle *visualhive.ApplyLifecycleResult
	Outbox             visualhive.OutboxProcessorResult
}

func applyWorkflowEvidence(ctx context.Context, stateDir string, config Config, workflow WorkflowRunEvidence, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy, applyOptions visualhive.ApplyLifecycleOptions) (visualhive.Validation, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, hivegithub.VerifiedVisualHiveArtifact, error) {
	bundle, verified, err := client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
		Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact,
		SourceArtifactID: workflow.EvidenceArtifact, DestinationDir: filepath.Join(stateDir, "visual-hive", "artifacts"),
		FetchSourceArtifact: true,
		TargetRef:           config.DefaultBranch, MaxACMM: config.ACMMLevel,
		ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath,
		ExpectedEvent:             visualHiveProductionWorkflowEvent,
		ExpectedRunName:           workflowDispatchDisplayTitle(workflow.CorrelationID),
		ExpectedProducerGitCommit: config.VisualHiveRef,
		AllowFailedWorkflowRun:    workflow.RepositoryTestOverall != 0,
	})
	if err != nil {
		return visualhive.Validation{}, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, hivegithub.VerifiedVisualHiveArtifact{}, err
	}
	currentHead, err := requireLiveInstalledWorkflowHead(ctx, client, config, workflow.HeadSHA)
	if err != nil {
		return bundle.Validation, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, verified, err
	}
	applyOptions.TargetRef = config.DefaultBranch
	applyOptions.CurrentTargetCommitSHA = currentHead
	applyOptions.VerificationRunID = fmt.Sprintf("%d", workflow.RunID)
	applyOptions.VerificationURL = workflow.RunURL
	applyOptions.VerificationCommitSHA = workflow.HeadSHA
	applyOptions.MaxActiveIssues = config.MaxActiveIssues
	applyOptions.PreferRepairable = config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge
	applyOptions.DisableIssuePublication = config.Automation == AutomationAdvisory
	// Repository-authored tests are deterministic evidence alongside Visual
	// Hive's verdict, not a replacement for it. Derive their lifecycle only from
	// the independently verified source artifact. A failed plan may open a
	// repairable finding; only a completely green target-branch plan can provide
	// authoritative absence for post-merge closure. Apply this blocking signal
	// first so it receives one bounded WIP slot before non-blocking advisory
	// observations from the same run.
	repositoryApply := visualhive.ApplyLifecycleResult{}
	repositoryApplied := false
	if workflow.RepositoryTestDigest != "" {
		repositoryTests, repositoryTestsErr := visualhive.BuildRepositoryTestBundle(bundle, workflow.RepositoryTests, workflow.RepositoryTestOverall, workflow.RepositoryTestDigest)
		if repositoryTestsErr != nil {
			return bundle.Validation, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, verified, fmt.Errorf("validate hosted repository test evidence: %w", repositoryTestsErr)
		}
		var repositoryApplyErr error
		repositoryApply, repositoryApplyErr = lifecycle.ApplyBundle(repositoryTests, beadStore, applyOptions)
		if repositoryApplyErr != nil {
			return bundle.Validation, repositoryApply, visualhive.OutboxProcessorResult{}, verified, fmt.Errorf("apply hosted repository test lifecycle: %w", repositoryApplyErr)
		}
		repositoryApplied = true
	}
	apply, err := lifecycle.ApplyBundle(bundle, beadStore, applyOptions)
	if err != nil {
		return bundle.Validation, repositoryApply, visualhive.OutboxProcessorResult{}, verified, err
	}
	if repositoryApplied {
		apply = combineLifecycleApplyResults(apply, repositoryApply)
	}
	outbox := visualhive.OutboxProcessorResult{}
	if config.Automation == AutomationAdvisory {
		return bundle.Validation, apply, outbox, verified, nil
	}
	outbox, err = processWorkflowOutboxAtCurrentHead(ctx, config, workflow, lifecycle, beadStore, client, client, policy)
	if err != nil {
		return bundle.Validation, apply, outbox, verified, err
	}
	return bundle.Validation, apply, outbox, verified, nil
}

func combineLifecycleApplyResults(primary, derived visualhive.ApplyLifecycleResult) visualhive.ApplyLifecycleResult {
	primary.Idempotent = primary.Idempotent && derived.Idempotent
	primary.Created += derived.Created
	primary.Updated += derived.Updated
	primary.Resolved += derived.Resolved
	primary.Reopened += derived.Reopened
	primary.IgnoredAbsent += derived.IgnoredAbsent
	primary.OutboxCreated += derived.OutboxCreated
	primary.BeadsCreated += derived.BeadsCreated
	primary.BeadsSkipped += derived.BeadsSkipped
	primary.Deferred += derived.Deferred
	seen := make(map[string]bool, len(primary.FindingIDs)+len(derived.FindingIDs))
	merged := make([]string, 0, len(primary.FindingIDs)+len(derived.FindingIDs))
	for _, id := range append(append([]string(nil), primary.FindingIDs...), derived.FindingIDs...) {
		if id != "" && !seen[id] {
			seen[id] = true
			merged = append(merged, id)
		}
	}
	sort.Strings(merged)
	primary.FindingIDs = merged
	return primary
}

// processWorkflowOutboxAtCurrentHead closes the apply/side-effect race window.
// ApplyBundle is deliberately durable before remote issue mutations; therefore
// the default branch must be rebound after that write and immediately before
// any pending lifecycle mutation is sent to GitHub. A stale apply remains
// recoverable: its pending entries are never sent, and the next exact-head
// bundle supersedes them by digest before ProcessOutbox runs again.
func processWorkflowOutboxAtCurrentHead(ctx context.Context, config Config, workflow WorkflowRunEvidence, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, issueClient visualhive.LifecycleIssueClient, policy automation.Policy) (visualhive.OutboxProcessorResult, error) {
	if _, err := requireLiveDefaultWorkflowHead(ctx, client, config, workflow.HeadSHA); err != nil {
		return visualhive.OutboxProcessorResult{}, err
	}
	outbox := visualhive.ProcessOutbox(ctx, lifecycle, beadStore, policy, issueClient)
	if outbox.Failed > 0 {
		return outbox, fmt.Errorf("GitHub lifecycle outbox failed: %s", strings.Join(outbox.Errors, "; "))
	}
	return outbox, nil
}

func requireLiveDefaultWorkflowHead(ctx context.Context, client *hivegithub.Client, config Config, workflowHead string) (string, error) {
	if client == nil || client.GoGitHub() == nil {
		return "", fmt.Errorf("GitHub client is required to bind workflow evidence to the live default head")
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(config.Repository), "/")
	if !ok || owner == "" || repo == "" || strings.TrimSpace(config.DefaultBranch) == "" {
		return "", fmt.Errorf("configured repository and default branch are required to bind workflow evidence")
	}
	branch, _, err := client.GoGitHub().Repositories.GetBranch(ctx, owner, repo, config.DefaultBranch, 0)
	if err != nil {
		return "", fmt.Errorf("read live default head before applying workflow evidence: %w", err)
	}
	currentHead := strings.ToLower(strings.TrimSpace(branch.GetCommit().GetSHA()))
	workflowHead = strings.ToLower(strings.TrimSpace(workflowHead))
	if !immutableCommit.MatchString(currentHead) || !immutableCommit.MatchString(workflowHead) {
		return "", fmt.Errorf("live default head or workflow head is not an immutable commit SHA")
	}
	if currentHead != workflowHead {
		return currentHead, &staleWorkflowHeadError{WorkflowHead: workflowHead, CurrentHead: currentHead}
	}
	return currentHead, nil
}

func requireLiveInstalledWorkflowHead(ctx context.Context, client *hivegithub.Client, config Config, workflowHead string) (string, error) {
	currentHead, err := requireLiveDefaultWorkflowHead(ctx, client, config, workflowHead)
	if err != nil {
		return currentHead, err
	}
	if err := VerifyInstalledSetupAtCommit(ctx, client, config, currentHead); err != nil {
		return currentHead, fmt.Errorf("reverify managed setup at exact workflow head before applying evidence: %w", err)
	}
	return currentHead, nil
}

func discardStaleWorkflowDispatch(stateDir string, config Config, workflow WorkflowRunEvidence, stale *staleWorkflowHeadError) error {
	if stale == nil {
		return fmt.Errorf("stale workflow evidence is required")
	}
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return err
	}
	detail := fmt.Sprintf("run=%d workflow_head=%s current_default_head=%s", workflow.RunID, stale.WorkflowHead, stale.CurrentHead)
	if err := store.AuditStrict(AuditEntry{Action: "discard_stale_workflow_evidence", Allowed: false, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	if err := consumeWorkflowDispatch(stateDir, workflow); err != nil {
		return fmt.Errorf("discard exact stale workflow dispatch: %w", err)
	}
	return nil
}

func orchestrateRepairs(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy, verifiedArtifact hivegithub.VerifiedVisualHiveArtifact, specialists repair.SpecialistDispatcher, specialistWorkDir string) (repairOrchestrationResult, error) {
	result := repairOrchestrationResult{}
	resumed, baselineGate, pendingReview, err := reconcileBaselineReview(ctx, stateDir, config, lifecycle, client, policy)
	if baselineGate != nil {
		result.Gates = append(result.Gates, *baselineGate)
	}
	if resumed != nil {
		result.Repairs = append(result.Repairs, *resumed)
	}
	if err != nil || pendingReview {
		return result, err
	}
	if _, err := reconcileMergePolicyHold(ctx, stateDir, config, lifecycle, client, policy); err != nil {
		return result, err
	}
	maxAttempts := policy.MaxRepairAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	for cycle := 0; cycle <= maxAttempts; cycle++ {
		repairs, err := runEligibleRepairs(ctx, config, lifecycle, client, policy, verifiedArtifact, specialists, specialistWorkDir)
		result.Repairs = append(result.Repairs, repairs...)
		if err != nil {
			finding, ok := activeRepairFinding(lifecycle.Snapshot())
			var resumable *repair.ResumableFailureError
			if errors.As(err, &resumable) {
				return result, fmt.Errorf("%w; next command: hive retry-repair --state-dir %q --finding %q --recurrence %d --attempt %d --failure-class %s --failure-id %q --reason <reason>", err, stateDir, resumable.RepositoryFingerprint, resumable.Recurrence, resumable.Attempt, resumable.Class, resumable.FailureID)
			}
			if repair.IsRetryableAttemptError(err) && ok {
				spent, countErr := durableRepairAttempts(stateDir, finding)
				if countErr != nil {
					return result, countErr
				}
				if spent < maxAttempts {
					continue
				}
				return result, fmt.Errorf("repair attempt budget exhausted for %s after %d attempts and retryable failure: %w", finding.RepositoryFingerprint, spent, err)
			}
			return result, err
		}
		finding, ok, findingErr := repairFindingForOrchestration(stateDir, lifecycle.Snapshot())
		if findingErr != nil {
			return result, findingErr
		}
		if !ok {
			return result, nil
		}
		if finding.Status == visualhive.StatusMerged || finding.Status == visualhive.StatusPostMergeVerifying {
			postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
			result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
			return result, verifyErr
		}
		if finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued || finding.Status == visualhive.StatusRepairRunning {
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("repair attempt budget exhausted for %s", finding.RepositoryFingerprint)
			}
			continue
		}
		if finding.Status == visualhive.StatusNeedsRevision {
			pending, pendingErr := hasPendingBaselineCandidate(stateDir, finding.RepositoryFingerprint)
			if pendingErr != nil {
				return result, pendingErr
			}
			if !pending {
				if finding.RepairAttempts >= maxAttempts {
					return result, fmt.Errorf("repair attempt budget exhausted for %s", finding.RepositoryFingerprint)
				}
				continue
			}
		}
		refreshedFinding, _, refreshErr := refreshOwnedRepairBranchIfBehind(ctx, stateDir, config, finding, lifecycle, client, policy)
		if refreshErr != nil {
			return result, refreshErr
		}
		finding = refreshedFinding
		gate, err := waitForPullRequestGate(ctx, client, config.Repository, finding)
		if err != nil {
			return result, err
		}
		green := gateChecksGreen(gate)
		checkEvidence := make([]visualhive.CheckEvidence, 0, len(gate.Checks))
		for _, check := range gate.Checks {
			checkEvidence = append(checkEvidence, visualhive.CheckEvidence{
				Name: check.Name, State: check.State, URL: check.URL, ProvenanceVerified: check.ProvenanceVerified,
				WorkflowRunID: check.WorkflowRunID, WorkflowPath: check.WorkflowPath, WorkflowEvent: check.WorkflowEvent,
			})
		}
		summary := checkSummary(gate)
		evaluation := GateEvaluation{RepositoryFingerprint: finding.RepositoryFingerprint, Purpose: "repair", Gate: gate}
		if !green {
			attemptState, stateErr := repair.NewStore(filepath.Join(stateDir, "repair"))
			if stateErr != nil {
				return result, stateErr
			}
			attempt, exists := attemptState.Get(finding.RepositoryFingerprint)
			if exists && attempt.BaselineReview != nil && attempt.BaselineReview.Status == repair.BaselineReviewCandidateReady {
				decision := policy.Authorize(automation.ActionRequest{
					Action: automation.ActionCreateBaselineReview, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
					Risk: automation.RiskRestricted, ChangedFiles: baselineCandidatePaths(attempt.BaselineReview.Candidates), RepairAttempts: finding.RepairAttempts,
				})
				lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionCreateBaselineReview), decision.Allowed, strings.Join(decision.Reasons, "; "))
				if !decision.Allowed {
					return result, fmt.Errorf("baseline review proposal denied: %s", strings.Join(decision.Reasons, "; "))
				}
				verified, fetchErr := client.FetchAndVerifyPullRequestArtifact(ctx, hivegithub.PullRequestArtifactRequest{
					Repository: config.Repository, PullRequestNumber: finding.PRNumber, ExpectedWorkflowRunID: gate.VisualHiveWorkflowRunID,
					ExpectedHeadSHA: attempt.CommitSHA, ExpectedHeadBranch: attempt.Branch,
					ExpectedWorkflowName: "Visual Hive PR", ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml", ArtifactName: "visual-hive-pr",
					DestinationDir: filepath.Join(stateDir, "repair", "baseline-artifacts", repairStateKey(finding.RepositoryFingerprint)),
				})
				if fetchErr != nil {
					return result, fetchErr
				}
				hosted, recognized, reviewErr := repair.ReadHostedBaselineReview(verified.EvidenceRootPath)
				if reviewErr != nil {
					return result, reviewErr
				}
				if !recognized {
					return result, fmt.Errorf("failed exact-head PR run did not contain an exclusive missing-baseline review")
				}
				updated, proposalErr := repair.CreateBaselineProposal(ctx, baselineProposalConfig(config, stateDir), finding, repair.BaselineProposalSource{
					WorkflowRunID: verified.WorkflowRunID, ArtifactID: verified.ArtifactID, RunURL: verified.RunURL,
				}, hosted, attemptState, client)
				if proposalErr != nil {
					return result, proposalErr
				}
				reason := fmt.Sprintf("Visual baseline proposal %s requires full-resolution review of %d exact candidate image(s)", updated.BaselineReview.ProposalPRURL, len(updated.BaselineReview.Candidates))
				if err := lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "visual_baseline", reason); err != nil {
					return result, err
				}
				replaceRepairResult(&result.Repairs, repairResultFromAttempt(updated, true))
				result.Gates = append(result.Gates, evaluation)
				return result, nil
			}
			if finding.Status != visualhive.StatusReady {
				if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, false, summary, checkEvidence); err != nil {
					return result, err
				}
			}
			result.Gates = append(result.Gates, evaluation)
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("hosted checks remain red after %d attempts: %s", finding.RepairAttempts, summary)
			}
			continue
		}
		if finding.Status != visualhive.StatusReady {
			if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, true, summary, checkEvidence); err != nil {
				return result, err
			}
		}
		if gate.Merged {
			reconciled, reconcileErr := reconcileExternallyMergedRepair(ctx, stateDir, config, lifecycle, client, policy)
			if reconcileErr != nil {
				return result, reconcileErr
			}
			if !reconciled {
				return result, fmt.Errorf("merged pull request #%d was not backed by a consumable Hive merge intent", finding.PRNumber)
			}
			result.Gates = append(result.Gates, evaluation)
			updated, exists := lifecycle.Finding(finding.RepositoryFingerprint)
			if !exists || updated.Status != visualhive.StatusMerged || updated.MergeSHA == "" {
				return result, fmt.Errorf("merged pull request #%d was not durably reconciled", finding.PRNumber)
			}
			finding = updated
			postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
			result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
			return result, verifyErr
		}
		if !gate.Open {
			return result, fmt.Errorf("repair pull request #%d was closed without merging", finding.PRNumber)
		}
		if config.Automation == AutomationAutoMerge && strings.EqualFold(strings.TrimSpace(finding.IssueKind), "test_adequacy_gap") {
			attemptStore, stateErr := repair.NewStore(filepath.Join(stateDir, "repair"))
			if stateErr != nil {
				return result, stateErr
			}
			attempt, exists := attemptStore.Get(finding.RepositoryFingerprint)
			if !exists || attempt.CommitSHA != gate.HeadSHA || !sameStringSet(attempt.ChangedFiles, gate.ChangedFiles) {
				return result, fmt.Errorf("test adequacy automatic merge requires exact durable repair commit and changed-file state")
			}
			if validationErr := repair.ValidateAutonomousTestAdequacyCommit(ctx, config.CheckoutDir, attempt.CommitSHA, attempt.ChangedFiles); validationErr != nil {
				lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "validate_additive_test_repair", false, validationErr.Error())
				return result, fmt.Errorf("test adequacy repair is not eligible for automatic merge: %w", validationErr)
			}
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "validate_additive_test_repair", true, fmt.Sprintf("commit=%s files=%d", attempt.CommitSHA, len(attempt.ChangedFiles)))
		}
		if config.Automation == AutomationRepairPR {
			result.Gates = append(result.Gates, evaluation)
			return result, nil
		}
		request := mergeActionRequest(config, finding, gate)
		mergePolicy := mergePolicyForFinding(policy, finding)
		decision := mergePolicy.Authorize(request)
		var appliedApproval *MergeApproval
		approvalStore, storeErr := NewStore(filepath.Join(stateDir, "integrated"))
		if storeErr != nil {
			return result, storeErr
		}
		approval, approvalExists, approvalErr := approvalStore.LoadMergeApproval()
		if approvalErr != nil {
			return result, approvalErr
		}
		if approvalExists {
			currentDiffDigest, digestErr := client.PullRequestDiffDigest(ctx, config.Repository, gate.Number)
			if digestErr != nil {
				return result, digestErr
			}
			if approvalErr := ValidateMergeApproval(approval, config.Repository, config.RepositoryID, currentDiffDigest, gate); approvalErr != nil {
				if auditErr := approvalStore.AuditStrict(AuditEntry{Action: "apply_merge_approval", Allowed: false, Repository: config.Repository, Detail: approvalErr.Error()}); auditErr != nil {
					return result, auditErr
				}
				lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "apply_merge_approval", false, approvalErr.Error())
				if deleteErr := approvalStore.DeleteMergeApproval(); deleteErr != nil {
					return result, deleteErr
				}
			} else {
				approvalAllowed := decision.Allowed
				if !approvalAllowed {
					if approvedDecision, eligible := authorizePathApprovedMerge(mergePolicy, request, decision); eligible && approvedDecision.Allowed {
						decision, approvalAllowed = approvedDecision, true
					}
				}
				if approvalAllowed {
					appliedApproval = &approval
					detail := fmt.Sprintf("actor=%s pr=%d head=%s diff=%s reason=%s", approval.Actor, approval.PRNumber, approval.HeadSHA, approval.DiffDigest, approval.Reason)
					if auditErr := approvalStore.AuditStrict(AuditEntry{Action: "apply_merge_approval", Allowed: true, Repository: config.Repository, Detail: detail}); auditErr != nil {
						return result, auditErr
					}
					lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "apply_merge_approval", true, detail)
				} else {
					detail := "approval cannot override non-path merge gates: " + strings.Join(decision.Reasons, "; ")
					if auditErr := approvalStore.AuditStrict(AuditEntry{Action: "apply_merge_approval", Allowed: false, Repository: config.Repository, Detail: detail}); auditErr != nil {
						return result, auditErr
					}
					lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "apply_merge_approval", false, detail)
					if deleteErr := approvalStore.DeleteMergeApproval(); deleteErr != nil {
						return result, deleteErr
					}
				}
			}
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionMergePR), decision.Allowed, strings.Join(decision.Reasons, "; "))
		evaluation.Decision = &decision
		evaluation.Approval = appliedApproval
		if !decision.Allowed {
			if err := markMergePolicyHold(lifecycle, finding, decision); err != nil {
				return result, err
			}
			result.Gates = append(result.Gates, evaluation)
			return result, nil
		}
		boundary, err := mergeReadyFindingAtBoundary(ctx, stateDir, config, finding, lifecycle, gate, appliedApproval, client)
		if boundary.Gate.Number > 0 {
			evaluation.Gate = boundary.Gate
		}
		if boundary.Decision.Action == automation.ActionMergePR {
			evaluation.Decision = &boundary.Decision
			evaluation.Approval = boundary.Approval
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "final_live_merge_gate", boundary.Decision.Allowed, strings.Join(boundary.Decision.Reasons, "; "))
		}
		if err != nil {
			var denied *mergeBoundaryDeniedError
			if errors.As(err, &denied) {
				if err := markMergePolicyHold(lifecycle, finding, denied.Decision); err != nil {
					return result, err
				}
				result.Gates = append(result.Gates, evaluation)
				return result, nil
			}
			return result, err
		}
		intent, mergeSHA := boundary.Intent, boundary.MergeSHA
		evaluation.MergeSHA = mergeSHA
		result.Gates = append(result.Gates, evaluation)
		if appliedApproval != nil && finding.ManualReviewKind == "merge_policy" {
			if err := lifecycle.MarkManualReviewComplete(finding.RepositoryFingerprint, "merge_policy"); err != nil {
				return result, err
			}
		}
		if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, mergeSHA); err != nil {
			return result, err
		}
		intentStore, storeErr := NewStore(filepath.Join(stateDir, "integrated"))
		if storeErr != nil {
			return result, storeErr
		}
		if err := enqueueMergedRepairRetirement(intentStore, config, finding, mergeSHA); err != nil {
			return result, err
		}
		if err := consumeMergeIntent(intentStore, config, intent, mergeSHA, false); err != nil {
			return result, err
		}
		if err := reconcileRepairRetirements(ctx, intentStore, config, lifecycle, client, policy, false); err != nil {
			return result, err
		}
		finding.MergeSHA, finding.Status = mergeSHA, visualhive.StatusMerged
		postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
		result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
		return result, verifyErr
	}
	return result, fmt.Errorf("repair orchestration exceeded its bounded iteration budget")
}

func durableRepairAttempts(stateDir string, finding visualhive.FindingLifecycle) (int, error) {
	spent := finding.RepairAttempts
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return 0, err
	}
	attempt, exists := state.Get(finding.RepositoryFingerprint)
	if exists && attempt.Recurrence == finding.Recurrences && attempt.CountedModelAttempts() > spent {
		spent = attempt.CountedModelAttempts()
	}
	return spent, nil
}

func markMergePolicyHold(lifecycle *visualhive.LifecycleStore, finding visualhive.FindingLifecycle, decision automation.Decision) error {
	reason := fmt.Sprintf("Hive cannot auto-merge pull request #%d under the configured policy: %s. Do not merge it directly. Review the exact diff, then run `hive approve-merge --pr %d --head %s --plan --json`; repeat the returned exact base and diff digest in its apply command with your reason. Hive will revalidate every gate and perform the merge.", finding.PRNumber, strings.Join(decision.Reasons, "; "), finding.PRNumber, finding.RepairCommitSHA)
	return lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "merge_policy", reason)
}

func hasPendingBaselineCandidate(stateDir, repositoryFingerprint string) (bool, error) {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return false, err
	}
	attempt, exists := state.Get(repositoryFingerprint)
	return exists && attempt.BaselineReview != nil && attempt.BaselineReview.Status == repair.BaselineReviewCandidateReady, nil
}

func reconcileBaselineReview(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) (*repair.Result, *GateEvaluation, bool, error) {
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return nil, nil, false, err
	}
	snapshot := state.Snapshot()
	keys := make([]string, 0, len(snapshot.Attempts))
	for key := range snapshot.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attempt := snapshot.Attempts[key]
		if attempt == nil || attempt.BaselineReview == nil || attempt.BaselineReview.Status != repair.BaselineReviewProposalOpen || attempt.BaselineReview.ProposalPRNumber <= 0 {
			continue
		}
		finding, exists := lifecycle.Finding(key)
		if !exists || finding.PRNumber != attempt.PRNumber {
			return nil, nil, false, fmt.Errorf("baseline proposal lost its originating repair lifecycle")
		}
		gate, err := client.InspectPullRequestGate(ctx, config.Repository, attempt.BaselineReview.ProposalPRNumber)
		if err != nil {
			return nil, nil, false, err
		}
		evaluation := &GateEvaluation{RepositoryFingerprint: key, Purpose: "baseline_review", Gate: gate}
		expected := baselineCandidatePaths(attempt.BaselineReview.Candidates)
		if gate.HeadSHA != attempt.BaselineReview.ProposalCommitSHA || !sameStringSet(gate.ChangedFiles, expected) || !gate.BaselineChanged || gate.WorkflowChanged || gate.SecuritySensitive || gate.DeploymentChanged {
			return nil, evaluation, false, fmt.Errorf("baseline proposal #%d was tampered or changed outside its exact candidate set", gate.Number)
		}
		reason := fmt.Sprintf("Visual baseline proposal %s requires full-resolution review of %d exact candidate image(s)", attempt.BaselineReview.ProposalPRURL, len(attempt.BaselineReview.Candidates))
		if gate.Open {
			if err := lifecycle.MarkManualReviewRequired(key, "visual_baseline", reason); err != nil {
				return nil, evaluation, false, err
			}
			return nil, evaluation, true, nil
		}
		if !gate.Merged {
			attempt.BaselineReview.Status = repair.BaselineReviewRejected
			attempt.BaselineReview.RejectionReason = "baseline proposal was closed without merging"
			if err := state.Put(*attempt); err != nil {
				return nil, evaluation, false, err
			}
			return nil, evaluation, false, fmt.Errorf("baseline proposal #%d was rejected", gate.Number)
		}
		if !approvedMergedBaselineProposal(gate) {
			return nil, evaluation, false, fmt.Errorf("merged baseline proposal #%d lacks ready-for-review human approval, green exact-head checks, or merge SHA", gate.Number)
		}
		decision := policy.Authorize(automation.ActionRequest{
			Action: automation.ActionApplyBaselineReview, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
			Risk: automation.RiskRestricted, ChangedFiles: expected, RepairAttempts: finding.RepairAttempts,
		})
		lifecycle.RecordAuthorization(key, string(automation.ActionApplyBaselineReview), decision.Allowed, strings.Join(decision.Reasons, "; "))
		if !decision.Allowed {
			return nil, evaluation, false, fmt.Errorf("apply reviewed baseline denied: %s", strings.Join(decision.Reasons, "; "))
		}
		updated, err := repair.ResumeAfterBaselineApproval(ctx, baselineProposalConfig(config, stateDir), finding, gate.MergeSHA, state, client)
		if err != nil {
			return nil, evaluation, false, err
		}
		if err := lifecycle.MarkPRHeadUpdated(key, updated.CommitSHA); err != nil {
			return nil, evaluation, false, err
		}
		if err := lifecycle.MarkManualReviewComplete(key, "visual_baseline"); err != nil {
			return nil, evaluation, false, err
		}
		result := repairResultFromAttempt(updated, true)
		return &result, evaluation, false, nil
	}
	return nil, nil, false, nil
}

// approvedMergedBaselineProposal treats the explicit merge of Hive's exact,
// non-draft baseline-only proposal as the human approval. UpsertReviewPullRequest
// deliberately applies a durable hold label, and GitHub retains that label after
// a reviewer marks the PR ready and merges it. The label must continue blocking
// an open proposal, but cannot make an otherwise exact reviewed merge impossible
// to consume. Exact head, candidate paths, and risk classification are verified
// immediately before this helper is called.
func approvedMergedBaselineProposal(gate hivegithub.PullRequestGate) bool {
	return gate.Merged && !gate.Draft && !gate.HumanReviewRequired && gateChecksGreen(gate) && strings.TrimSpace(gate.MergeSHA) != ""
}

func baselineProposalConfig(config Config, stateDir string) repair.BaselineProposalConfig {
	return repair.BaselineProposalConfig{
		RepositoryDir: config.CheckoutDir, WorktreeRoot: filepath.Join(stateDir, "repair", "baseline-worktrees"), BaseBranch: config.DefaultBranch,
		ExpectedRemoteURL:  RepositoryCloneURL(config.Repository),
		ValidationCommands: []repair.Command{{Name: "git", Args: []string{"diff", "--check"}}}, CommandTimeout: 15 * time.Minute,
	}
}

func baselineCandidatePaths(candidates []repair.BaselineCandidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.BaselinePath)
	}
	sort.Strings(result)
	return result
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := map[string]int{}
	for _, value := range left {
		counts[filepath.ToSlash(strings.TrimPrefix(value, "./"))]++
	}
	for _, value := range right {
		key := filepath.ToSlash(strings.TrimPrefix(value, "./"))
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

func repairResultFromAttempt(attempt repair.Attempt, resumed bool) repair.Result {
	review := attempt.BaselineReview
	return repair.Result{
		RepositoryFingerprint: attempt.RepositoryFingerprint, Branch: attempt.Branch, CommitSHA: attempt.CommitSHA,
		PRNumber: attempt.PRNumber, PRURL: attempt.PRURL, ChangedFiles: append([]string(nil), attempt.ChangedFiles...), BaselineReview: review, Resumed: resumed,
	}
}

func replaceRepairResult(results *[]repair.Result, replacement repair.Result) {
	for index := range *results {
		if (*results)[index].RepositoryFingerprint == replacement.RepositoryFingerprint {
			(*results)[index] = replacement
			return
		}
	}
	*results = append(*results, replacement)
}

func repairStateKey(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:6])
}

func reconcileExternallyMergedRepair(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) (bool, error) {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return false, err
	}
	intent, hasIntent, err := store.LoadMergeIntent()
	if err != nil {
		return false, err
	}
	var finding visualhive.FindingLifecycle
	var ok bool
	if hasIntent {
		var mappingErr error
		finding, ok, mappingErr = findingForPullRequest(lifecycle.Snapshot(), intent.PRNumber)
		if mappingErr != nil {
			return false, mappingErr
		}
		if !ok || !strings.EqualFold(finding.RepairCommitSHA, intent.HeadSHA) {
			return false, fmt.Errorf("durable merge intent does not match exactly one Hive repair finding")
		}
	} else {
		finding, ok = repairPullRequestFinding(lifecycle.Snapshot())
		if !ok {
			return false, nil
		}
	}
	gate, err := client.InspectPullRequestGate(ctx, config.Repository, finding.PRNumber)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(gate.HeadSHA, finding.RepairCommitSHA) {
		return false, fmt.Errorf("pull request #%d head %s does not match Hive repair commit %s", finding.PRNumber, gate.HeadSHA, finding.RepairCommitSHA)
	}
	if !gate.Merged {
		if !gate.Open {
			return false, fmt.Errorf("repair pull request #%d was closed without merging", finding.PRNumber)
		}
		return false, nil
	}
	if strings.TrimSpace(gate.MergeSHA) == "" {
		return false, fmt.Errorf("merged pull request #%d did not report a merge commit", finding.PRNumber)
	}
	if !hasIntent {
		detail := fmt.Sprintf("pull request #%d at %s was merged without a durable Hive merge intent; restore or investigate the repository before running again", gate.Number, gate.HeadSHA)
		if auditErr := store.AuditStrict(AuditEntry{Action: "reconcile_external_merge", Allowed: false, Repository: config.Repository, Detail: detail}); auditErr != nil {
			return false, auditErr
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "reconcile_external_merge", false, detail)
		return false, fmt.Errorf("%s", detail)
	}
	digest, err := client.PullRequestDiffDigest(ctx, config.Repository, gate.Number)
	if err != nil {
		return false, err
	}
	if err := validateRecoveredMergeIntent(intent, config.Repository, config.RepositoryID, digest, gate); err != nil {
		return false, err
	}
	if !strings.EqualFold(strings.TrimSpace(gate.MergedBy), strings.TrimSpace(intent.WriterActor)) {
		return false, fmt.Errorf("merged pull request #%d writer %q does not match Hive intent writer %q", gate.Number, gate.MergedBy, intent.WriterActor)
	}
	wasMerged := finding.Status == visualhive.StatusMerged || finding.Status == visualhive.StatusPostMergeVerifying
	if !wasMerged {
		if finding.Status != visualhive.StatusReady {
			checkEvidence := make([]visualhive.CheckEvidence, 0, len(intent.Gate.Checks))
			for _, check := range intent.Gate.Checks {
				checkEvidence = append(checkEvidence, visualhive.CheckEvidence{Name: check.Name, State: check.State, URL: check.URL})
			}
			if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, intent.HeadSHA, true, checkSummary(intent.Gate), checkEvidence); err != nil {
				return false, err
			}
		}
		if intent.Approval != nil && finding.ManualReviewKind == "merge_policy" {
			if err := lifecycle.MarkManualReviewComplete(finding.RepositoryFingerprint, "merge_policy"); err != nil {
				return false, err
			}
		}
		if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, gate.MergeSHA); err != nil {
			return false, err
		}
	}
	if err := enqueueMergedRepairRetirement(store, config, finding, gate.MergeSHA); err != nil {
		return false, err
	}
	if err := consumeMergeIntent(store, config, intent, gate.MergeSHA, true); err != nil {
		return false, err
	}
	if err := reconcileRepairRetirements(ctx, store, config, lifecycle, client, policy, false); err != nil {
		return false, err
	}
	return !wasMerged, nil
}

func consumeMergeIntent(store *Store, config Config, intent MergeIntent, mergeSHA string, recovered bool) error {
	action := "consume_merge_intent"
	if recovered {
		action += "_recovered"
	}
	detail := fmt.Sprintf("pr=%d base=%s head=%s diff=%s writer=%s authorization=%s merge=%s", intent.PRNumber, intent.BaseSHA, intent.HeadSHA, intent.DiffDigest, intent.WriterActor, intent.Authorization, mergeSHA)
	if err := store.AuditStrict(AuditEntry{Action: action, Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
		return err
	}
	if intent.Approval != nil {
		approval, exists, err := store.LoadMergeApproval()
		if err != nil {
			return err
		}
		if exists && approval != *intent.Approval {
			return fmt.Errorf("active merge approval changed after the merge intent was authorized")
		}
		approvalDetail := fmt.Sprintf("actor=%s pr=%d base=%s head=%s diff=%s merge=%s reason=%s", intent.Approval.Actor, intent.Approval.PRNumber, intent.Approval.BaseSHA, intent.Approval.HeadSHA, intent.Approval.DiffDigest, mergeSHA, intent.Approval.Reason)
		if err := store.AuditStrict(AuditEntry{Action: "consume_merge_approval", Allowed: true, Repository: config.Repository, Detail: approvalDetail}); err != nil {
			return err
		}
		if exists {
			if err := store.DeleteMergeApproval(); err != nil {
				return err
			}
		}
	}
	return store.DeleteMergeIntent()
}

func repairPullRequestFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || (finding.HumanReviewRequired && finding.ManualReviewKind != "merge_policy") || finding.PRNumber <= 0 || finding.RepairCommitSHA == "" || finding.MergeSHA != "" {
			continue
		}
		switch finding.Status {
		case visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision, visualhive.StatusReady:
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
}

func mergedRepairFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	for _, finding := range state.Findings {
		if finding != nil && finding.Status == visualhive.StatusMerged && finding.MergeSHA != "" {
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
}

func recoverCompletedPostMergeVerification(lifecycle *visualhive.LifecycleStore) (bool, error) {
	state := lifecycle.Snapshot()
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.Status != visualhive.StatusPostMergeVerifying || finding.ValidationRunID == "" || finding.LastWorkflowRunID != finding.ValidationRunID {
			continue
		}
		summary := fmt.Sprintf("finding remained present after authoritative target-branch verification %s", finding.ValidationRunURL)
		if err := lifecycle.MarkPostMergeFailed(finding.RepositoryFingerprint, summary); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func verifyMergedFinding(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy) (WorkflowRunEvidence, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, error) {
	postMerge, err := dispatchAndWait(ctx, client, config)
	if err != nil {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	var postApply visualhive.ApplyLifecycleResult
	var postOutbox visualhive.OutboxProcessorResult
	for staleAttempt := 0; ; staleAttempt++ {
		applyOptions, verifyErr := verifyPostMergeTarget(ctx, stateDir, config, finding, postMerge.HeadSHA, client)
		if verifyErr != nil {
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "post_merge_descendant", false, verifyErr.Error())
			return postMerge, postApply, postOutbox, verifyErr
		}
		if postMerge.HeadSHA != finding.MergeSHA {
			lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "post_merge_descendant", true, fmt.Sprintf("repair merge %s is an unchanged-file ancestor of target head %s", finding.MergeSHA, postMerge.HeadSHA))
		}
		if err := lifecycle.MarkPostMergeVerifying(finding.RepositoryFingerprint, fmt.Sprintf("%d", postMerge.RunID), postMerge.RunURL); err != nil {
			return postMerge, postApply, postOutbox, err
		}
		_, postApply, postOutbox, _, err = applyWorkflowEvidence(ctx, stateDir, config, postMerge, lifecycle, beadStore, client, policy, applyOptions)
		if err == nil {
			if err := consumeWorkflowDispatch(stateDir, postMerge); err != nil {
				return postMerge, postApply, postOutbox, fmt.Errorf("consume exact post-merge workflow dispatch: %w", err)
			}
			break
		}
		var stale *staleWorkflowHeadError
		if !errors.As(err, &stale) {
			return postMerge, postApply, postOutbox, err
		}
		if resetErr := lifecycle.ResetStalePostMergeVerification(finding.RepositoryFingerprint, fmt.Sprintf("%d", postMerge.RunID), postMerge.HeadSHA); resetErr != nil {
			return postMerge, postApply, postOutbox, fmt.Errorf("reset stale post-merge verification: %w", resetErr)
		}
		if discardErr := discardStaleWorkflowDispatch(stateDir, config, postMerge, stale); discardErr != nil {
			return postMerge, postApply, postOutbox, discardErr
		}
		if staleAttempt+1 >= staleWorkflowRedispatchLimit {
			return postMerge, postApply, postOutbox, fmt.Errorf("default branch advanced during %d consecutive post-merge verifications: %w", staleWorkflowRedispatchLimit, err)
		}
		postMerge, err = dispatchAndWait(ctx, client, config)
		if err != nil {
			return postMerge, postApply, postOutbox, err
		}
	}
	verified, exists := lifecycle.Finding(finding.RepositoryFingerprint)
	if !exists || verified.Status != visualhive.StatusIssueClosed {
		if _, recoveryErr := recoverCompletedPostMergeVerification(lifecycle); recoveryErr != nil {
			return postMerge, postApply, postOutbox, recoveryErr
		}
		return postMerge, postApply, postOutbox, fmt.Errorf("post-merge verification did not resolve and close finding %s", finding.RepositoryFingerprint)
	}
	return postMerge, postApply, postOutbox, nil
}

func verifyPostMergeTarget(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, headSHA string, client *hivegithub.Client) (visualhive.ApplyLifecycleOptions, error) {
	options := visualhive.ApplyLifecycleOptions{}
	if headSHA == finding.MergeSHA {
		return options, nil
	}
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok || owner == "" || repo == "" {
		return options, fmt.Errorf("invalid configured repository")
	}
	state, err := repair.NewStore(filepath.Join(stateDir, "repair"))
	if err != nil {
		return options, err
	}
	attempt, exists := state.Get(finding.RepositoryFingerprint)
	if !exists || attempt.CommitSHA != finding.RepairCommitSHA || attempt.PRNumber != finding.PRNumber || len(attempt.ChangedFiles) == 0 {
		return options, fmt.Errorf("cannot verify descendant target for %s without the exact durable repair attempt", finding.RepositoryFingerprint)
	}
	comparison, _, err := client.GoGitHub().Repositories.CompareCommits(ctx, owner, repo, finding.MergeSHA, headSHA, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return options, fmt.Errorf("compare repair merge to target head: %w", err)
	}
	if comparison.GetStatus() != "ahead" || comparison.GetMergeBaseCommit().GetSHA() != finding.MergeSHA || comparison.GetTotalCommits() < 1 || comparison.GetTotalCommits() > 250 {
		return options, fmt.Errorf("target head %s is not a bounded descendant of repair merge %s", headSHA, finding.MergeSHA)
	}
	if len(comparison.Files) == 0 || len(comparison.Files) >= 300 {
		return options, fmt.Errorf("target descendant comparison did not provide a complete bounded file inventory")
	}
	repairFiles := make(map[string]bool, len(attempt.ChangedFiles))
	for _, file := range attempt.ChangedFiles {
		repairFiles[filepath.ToSlash(strings.TrimSpace(file))] = true
	}
	for _, file := range comparison.Files {
		if repairFiles[filepath.ToSlash(file.GetFilename())] || repairFiles[filepath.ToSlash(file.GetPreviousFilename())] {
			return options, fmt.Errorf("target descendant changed repair file %s after merge", file.GetFilename())
		}
	}
	options.VerifiedMergeAncestorFingerprint = finding.RepositoryFingerprint
	options.VerifiedMergeAncestorSHA = finding.MergeSHA
	return options, nil
}

func dispatchAndWait(ctx context.Context, client *hivegithub.Client, config Config) (WorkflowRunEvidence, error) {
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return WorkflowRunEvidence{}, fmt.Errorf("invalid configured repository")
	}
	const workflowFile = "hive-visual-hive.yml"
	const dispatchAttemptLimit = 3
	store, err := NewStore(filepath.Join(config.StateDir, "integrated"))
	if err != nil {
		return WorkflowRunEvidence{}, err
	}
	for attempt := 1; attempt <= dispatchAttemptLimit; attempt++ {
		selected, intent, err := dispatchAndWaitAttempt(ctx, client, store, config, owner, repo, workflowFile, config.DefaultBranch)
		if err != nil {
			return WorkflowRunEvidence{}, err
		}
		if intent.RecoveryCount > 0 {
			unique, discoveryErr := findCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, config.DefaultBranch, intent)
			if discoveryErr != nil {
				return WorkflowRunEvidence{}, fmt.Errorf("revalidate recovered workflow correlation uniqueness: %w", discoveryErr)
			}
			if unique == nil || unique.GetID() != selected.GetID() {
				return WorkflowRunEvidence{}, fmt.Errorf("recovered workflow run %d is not the sole exact correlation match", selected.GetID())
			}
		}
		if retryCancelledDispatch(selected.GetConclusion(), attempt, dispatchAttemptLimit) {
			if err := discardWorkflowDispatch(store, intent, selected.GetID()); err != nil {
				return WorkflowRunEvidence{}, err
			}
			continue
		}
		if selected.GetConclusion() != "success" && selected.GetConclusion() != "failure" {
			if err := discardWorkflowDispatch(store, intent, selected.GetID()); err != nil {
				return WorkflowRunEvidence{}, err
			}
			return WorkflowRunEvidence{}, fmt.Errorf("Visual Hive workflow %s concluded unsupported state %s", selected.GetHTMLURL(), selected.GetConclusion())
		}
		terminalRunError := func(cause error) (WorkflowRunEvidence, error) {
			// A conclusively failed, exactly bound run cannot become usable on a
			// later controller retry. Retaining it would replay the same failed run
			// forever and block a managed workflow upgrade. Repository-test-caused
			// failures that publish complete evidence continue through the normal
			// success path below and are consumed only after evidence application.
			if selected.GetConclusion() == "failure" {
				if discardErr := discardWorkflowDispatch(store, intent, selected.GetID()); discardErr != nil {
					return WorkflowRunEvidence{}, fmt.Errorf("%v; retire exact failed workflow dispatch: %w", cause, discardErr)
				}
			}
			return WorkflowRunEvidence{}, cause
		}
		repositoryTests, err := client.VerifyRepositoryTestJobs(ctx, hivegithub.RepositoryTestJobsRequest{
			Repository: config.Repository, WorkflowRunID: selected.GetID(), ExpectedHeadSHA: selected.GetHeadSHA(),
			ExpectedWorkflowName: visualHiveProductionWorkflowName, ExpectedWorkflowPath: visualHiveProductionWorkflowPath,
			Commands: config.TestCommands, VisualJobName: visualHiveProductionCheckContext,
			RequiredSuccessfulJobNames: []string{visualHivePRCheckContext, visualExecutionJobName},
		})
		if err != nil {
			return terminalRunError(fmt.Errorf("verify runner-controlled repository test jobs: %w", err))
		}
		if (repositoryTests.Overall == 0) != (selected.GetConclusion() == "success") {
			return terminalRunError(fmt.Errorf("workflow conclusion %s is inconsistent with runner-controlled repository test outcome %d", selected.GetConclusion(), repositoryTests.Overall))
		}
		artifacts, _, err := client.GoGitHub().Actions.ListWorkflowRunArtifacts(ctx, owner, repo, selected.GetID(), &gh.ListOptions{PerPage: 100})
		if err != nil {
			return terminalRunError(fmt.Errorf("list production evidence artifacts: %w", err))
		}
		workflow := WorkflowRunEvidence{
			CorrelationID: intent.CorrelationID, RunID: selected.GetID(), RunURL: selected.GetHTMLURL(), HeadSHA: selected.GetHeadSHA(), Conclusion: selected.GetConclusion(),
			RepositoryTests: repositoryTests.Results, RepositoryTestOverall: repositoryTests.Overall, RepositoryTestDigest: repositoryTests.EvidenceDigest,
		}
		evidenceName := fmt.Sprintf("visual-hive-evidence-%d", selected.GetID())
		bundleName := fmt.Sprintf("visual-hive-bundle-%d", selected.GetID())
		for _, artifact := range artifacts.Artifacts {
			switch {
			case artifact.GetName() == evidenceName:
				workflow.EvidenceArtifact = artifact.GetID()
			case artifact.GetName() == bundleName:
				workflow.BundleArtifact = artifact.GetID()
			}
		}
		if workflow.EvidenceArtifact <= 0 || workflow.BundleArtifact <= 0 {
			return terminalRunError(fmt.Errorf("workflow did not publish both evidence and provenance-bound bundle artifacts"))
		}
		return workflow, nil
	}
	return WorkflowRunEvidence{}, fmt.Errorf("Visual Hive workflow was cancelled by concurrency %d consecutive times", dispatchAttemptLimit)
}

func dispatchAndWaitAttempt(ctx context.Context, client *hivegithub.Client, store *Store, config Config, owner, repo, workflowFile, ref string, requestedOperation ...string) (*gh.WorkflowRun, WorkflowDispatchIntent, error) {
	operation := "production"
	if len(requestedOperation) > 0 {
		operation = strings.TrimSpace(requestedOperation[0])
	}
	if operation != "production" && operation != setupBaselineWorkflowOperation {
		return nil, WorkflowDispatchIntent{}, fmt.Errorf("unsupported exact workflow dispatch operation %q", operation)
	}
	intent, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return nil, WorkflowDispatchIntent{}, err
	}
	if exists {
		if err := validateWorkflowDispatchOperationBinding(intent, config, workflowFile, ref, operation); err != nil {
			return nil, intent, err
		}
	} else {
		intent, err = newWorkflowDispatchIntentForOperation(config, workflowFile, ref, operation, "", time.Time{})
		if err != nil {
			return nil, WorkflowDispatchIntent{}, err
		}
		if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
			return nil, intent, fmt.Errorf("persist workflow dispatch intent: %w", err)
		}
	}

	if intent.RunID > 0 {
		selected, err := waitForExactWorkflowRun(ctx, client, owner, repo, intent, nil)
		return selected, intent, err
	}
	if WorkflowDispatchNeedsRecovery(intent) {
		recovered, _, _, discoveryErr := discoverCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, ref, intent)
		if discoveryErr != nil {
			return nil, intent, fmt.Errorf("recheck ambiguous workflow dispatch %s: %w", intent.CorrelationID, discoveryErr)
		}
		if recovered == nil {
			planCommand := fmt.Sprintf("hive recover-dispatch --state-dir %q --action retry --correlation %s --plan --json", config.StateDir, intent.CorrelationID)
			return nil, intent, fmt.Errorf("workflow dispatch %s has an ambiguous transport outcome and no exact run was found; state remains fail-closed; review and authorize recovery with %s", intent.CorrelationID, planCommand)
		}
		intent.RunID, intent.RunURL, intent.MatchedAt = recovered.GetID(), recovered.GetHTMLURL(), time.Now().UTC()
		if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
			return nil, intent, fmt.Errorf("persist exact workflow match after ambiguous transport: %w", err)
		}
		selected, waitErr := waitForExactWorkflowRun(ctx, client, owner, repo, intent, recovered)
		return selected, intent, waitErr
	}
	if intent.DispatchAttemptedAt.IsZero() {
		// Credential authority is checked before the durable mutation checkpoint.
		// A missing optional App is a clean precondition failure, never an
		// ambiguous workflow dispatch that would require operator recovery.
		if config.VisualHiveGitHubAppID > 0 && !hasVisualHiveWorkflowClient(ctx) {
			return nil, intent, fmt.Errorf("dedicated Visual Hive workflow App runtime is unavailable; use managed upgrade/rebind")
		}
		if config.SetupAuthorizationAppID > 0 && config.VisualHiveGitHubAppID == 0 {
			return nil, intent, fmt.Errorf("legacy App-backed Visual Hive contract requires managed upgrade/rebind before workflow dispatch")
		}
		if intent.RecoveryAction == string(WorkflowDispatchRecoveryRetry) {
			recovered, discoveryErr := findCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, ref, intent)
			if discoveryErr != nil {
				return nil, intent, fmt.Errorf("recheck recovered dispatch correlation before retry: %w", discoveryErr)
			}
			if recovered != nil {
				intent.RunID, intent.RunURL, intent.MatchedAt = recovered.GetID(), recovered.GetHTMLURL(), time.Now().UTC()
				// The recovered run came from the original ambiguous request, whose
				// digest remains bound in RecoveryRequestDigest. Restore that attempt
				// metadata so the run binding remains structurally complete.
				intent.DispatchAttemptedAt = intent.RecoveryOriginalAttemptedAt
				intent.RequestDigest = intent.RecoveryRequestDigest
				if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
					return nil, intent, fmt.Errorf("persist delayed exact workflow match: %w", err)
				}
				selected, waitErr := waitForExactWorkflowRun(ctx, client, owner, repo, intent, recovered)
				return selected, intent, waitErr
			}
		}
		intent.DispatchAttemptedAt = time.Now().UTC()
		intent.RequestDigest = ""
		digest, digestErr := workflowDispatchRequestDigest(intent)
		if digestErr != nil {
			return nil, intent, digestErr
		}
		intent.RequestDigest = digest
		if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
			return nil, intent, fmt.Errorf("persist workflow dispatch mutation checkpoint: %w", err)
		}
		dispatchClient := visualHiveWorkflowClient(ctx, client)
		dispatch, response, dispatchErr := createExactWorkflowDispatchOperation(ctx, dispatchClient, owner, repo, workflowFile, ref, intent.CorrelationID, operation)
		if dispatchErr != nil {
			// A GitHub response proves the mutation was rejected. A transport error
			// is ambiguous, so retain the pre-mutation checkpoint for exact recovery.
			if response != nil {
				if deleteErr := store.DeleteWorkflowDispatchIntent(); deleteErr != nil {
					return nil, intent, fmt.Errorf("dispatch Visual Hive %s workflow: %v; remove rejected dispatch intent: %w", operation, dispatchErr, deleteErr)
				}
			}
			return nil, intent, fmt.Errorf("dispatch Visual Hive %s workflow: %w", operation, dispatchErr)
		}
		intent.DispatchAcknowledgedAt = time.Now().UTC()
		if dispatch.WorkflowRunID > 0 {
			intent.RunID = dispatch.WorkflowRunID
			intent.RunURL = dispatch.HTMLURL
			intent.MatchedAt = time.Now().UTC()
		}
		if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
			return nil, intent, fmt.Errorf("persist acknowledged workflow dispatch: %w", err)
		}
	}
	if intent.RunID > 0 {
		selected, err := waitForExactWorkflowRun(ctx, client, owner, repo, intent, nil)
		return selected, intent, err
	}

	selected, err := waitForCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, ref, intent)
	if err != nil {
		return nil, intent, err
	}
	intent.RunID = selected.GetID()
	intent.RunURL = selected.GetHTMLURL()
	intent.MatchedAt = time.Now().UTC()
	if err := store.SaveWorkflowDispatchIntent(intent); err != nil {
		return nil, intent, fmt.Errorf("persist exact workflow run binding: %w", err)
	}
	selected, err = waitForExactWorkflowRun(ctx, client, owner, repo, intent, selected)
	return selected, intent, err
}

type exactWorkflowDispatchResponse struct {
	WorkflowRunID int64  `json:"workflow_run_id"`
	RunURL        string `json:"run_url"`
	HTMLURL       string `json:"html_url"`
}

func createExactWorkflowDispatch(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref, correlation string) (exactWorkflowDispatchResponse, *gh.Response, error) {
	return createExactWorkflowDispatchOperation(ctx, client, owner, repo, workflowFile, ref, correlation, "production")
}

func createExactWorkflowDispatchOperation(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref, correlation, operation string) (exactWorkflowDispatchResponse, *gh.Response, error) {
	body := struct {
		Ref    string                 `json:"ref"`
		Inputs map[string]interface{} `json:"inputs"`
	}{
		Ref: ref,
		Inputs: map[string]interface{}{
			workflowDispatchInput: correlation,
			"hive_operation":      strings.TrimSpace(operation),
		},
	}
	endpoint := fmt.Sprintf("repos/%s/%s/actions/workflows/%s/dispatches", owner, repo, workflowFile)
	request, err := client.GoGitHub().NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		return exactWorkflowDispatchResponse{}, nil, err
	}
	// GitHub REST API 2026-03-10 returns the created workflow run ID. Do not
	// request an unsupported API version from self-hosted GHES; its 204 response
	// is handled by the exact correlation fallback.
	if usesCurrentWorkflowDispatchAPI(client.GoGitHub().BaseURL.Hostname()) {
		request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	}
	var dispatch exactWorkflowDispatchResponse
	response, err := client.GoGitHub().Do(ctx, request, &dispatch)
	return dispatch, response, err
}

func usesCurrentWorkflowDispatchAPI(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "api.github.com" || strings.HasSuffix(host, ".ghe.com")
}

func waitForCorrelatedWorkflowRun(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref string, intent WorkflowDispatchIntent) (*gh.WorkflowRun, error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		selected, err := findCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, ref, intent)
		if err != nil {
			return nil, err
		}
		if selected != nil {
			return selected, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for exactly correlated workflow dispatch %s: %w", intent.CorrelationID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func findCorrelatedWorkflowRun(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref string, intent WorkflowDispatchIntent) (*gh.WorkflowRun, error) {
	selected, _, _, err := discoverCorrelatedWorkflowRun(ctx, client, owner, repo, workflowFile, ref, intent)
	return selected, err
}

func discoverCorrelatedWorkflowRun(ctx context.Context, client *hivegithub.Client, owner, repo, workflowFile, ref string, intent WorkflowDispatchIntent) (*gh.WorkflowRun, int, int, error) {
	options := &gh.ListWorkflowRunsOptions{
		Branch: ref, Event: "workflow_dispatch", ExcludePullRequests: true,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var selected *gh.WorkflowRun
	pages, scanned := 0, 0
	for {
		runs, response, err := client.GoGitHub().Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, options)
		if err != nil {
			return nil, pages, scanned, fmt.Errorf("list exactly correlated workflow dispatches: %w", err)
		}
		pages++
		scanned += len(runs.WorkflowRuns)
		for _, candidate := range runs.WorkflowRuns {
			if candidate.GetDisplayTitle() != intent.ExpectedDisplayTitle {
				continue
			}
			if candidate.GetID() <= 0 || candidate.GetEvent() != "workflow_dispatch" || candidate.GetHeadBranch() != ref {
				return nil, pages, scanned, fmt.Errorf("workflow dispatch correlation %s matched a run with an invalid event or ref binding", intent.CorrelationID)
			}
			if selected != nil && selected.GetID() != candidate.GetID() {
				return nil, pages, scanned, fmt.Errorf("workflow dispatch correlation %s matched multiple run IDs; refusing to consume either", intent.CorrelationID)
			}
			selected = candidate
		}
		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}
	return selected, pages, scanned, nil
}

const (
	exactWorkflowRunPollInterval         = 5 * time.Second
	exactWorkflowRunBindingPollInterval  = time.Second
	exactWorkflowRunBindingStabilization = 30 * time.Second
)

func waitForExactWorkflowRun(ctx context.Context, client *hivegithub.Client, owner, repo string, intent WorkflowDispatchIntent, selected *gh.WorkflowRun) (*gh.WorkflowRun, error) {
	return waitForExactWorkflowRunWithTiming(
		ctx, client, owner, repo, intent, selected,
		exactWorkflowRunPollInterval,
		exactWorkflowRunBindingPollInterval,
		exactWorkflowRunBindingStabilization,
	)
}

func waitForExactWorkflowRunWithTiming(ctx context.Context, client *hivegithub.Client, owner, repo string, intent WorkflowDispatchIntent, selected *gh.WorkflowRun, pollInterval, bindingPollInterval, bindingStabilization time.Duration) (*gh.WorkflowRun, error) {
	if pollInterval <= 0 || bindingPollInterval <= 0 || bindingStabilization <= 0 {
		return nil, fmt.Errorf("exact workflow run wait timing must be positive")
	}
	bindingDeadline := time.Now().Add(bindingStabilization)
	bindingVerified := false
	var lastReadErr error
	var lastMissing []string
	for {
		if selected == nil {
			readCtx := ctx
			cancelRead := func() {}
			if !bindingVerified {
				readCtx, cancelRead = context.WithDeadline(ctx, bindingDeadline)
			}
			current, _, err := client.GoGitHub().Actions.GetWorkflowRunByID(readCtx, owner, repo, intent.RunID)
			cancelRead()
			if err == nil {
				selected = current
				lastReadErr = nil
			} else {
				lastReadErr = err
			}
		}
		delay := bindingPollInterval
		if selected != nil {
			missing, err := exactWorkflowRunBindingState(selected, intent)
			if err != nil {
				return nil, err
			}
			if len(missing) == 0 {
				bindingVerified = true
				lastMissing = nil
				if selected.GetStatus() == "completed" {
					return selected, nil
				}
				delay = pollInterval
			} else {
				lastMissing = append(lastMissing[:0], missing...)
				if !time.Now().Before(bindingDeadline) {
					return nil, fmt.Errorf("workflow run %d exact durable dispatch binding did not stabilize within %s (still missing %s); the persisted intent remains bound to run %d and Hive will not select a different run", intent.RunID, bindingStabilization, strings.Join(lastMissing, ", "), intent.RunID)
				}
			}
		} else if !time.Now().Before(bindingDeadline) {
			if len(lastMissing) > 0 {
				return nil, fmt.Errorf("workflow run %d exact durable dispatch binding did not stabilize within %s (still missing %s); the persisted intent remains bound to run %d and Hive will not select a different run", intent.RunID, bindingStabilization, strings.Join(lastMissing, ", "), intent.RunID)
			}
			if lastReadErr != nil {
				return nil, fmt.Errorf("workflow run %d exact durable dispatch binding could not be read within %s; the persisted intent remains bound to run %d and Hive will not select a different run: %w", intent.RunID, bindingStabilization, intent.RunID, lastReadErr)
			}
			return nil, fmt.Errorf("workflow run %d exact durable dispatch binding was unavailable for %s; the persisted intent remains bound to run %d and Hive will not select a different run", intent.RunID, bindingStabilization, intent.RunID)
		}

		if !bindingVerified {
			remaining := time.Until(bindingDeadline)
			if remaining < delay {
				delay = remaining
			}
			if delay <= 0 {
				continue
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("wait for exact workflow run %d: %w", intent.RunID, ctx.Err())
		case <-timer.C:
		}
		selected = nil
	}
}

func validateExactWorkflowRun(run *gh.WorkflowRun, intent WorkflowDispatchIntent) error {
	missing, err := exactWorkflowRunBindingState(run, intent)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("workflow run does not yet have its complete exact durable dispatch binding (missing %s)", strings.Join(missing, ", "))
	}
	return nil
}

func exactWorkflowRunBindingState(run *gh.WorkflowRun, intent WorkflowDispatchIntent) ([]string, error) {
	if run == nil {
		return []string{"run response"}, nil
	}
	missing := make([]string, 0, 4)
	if run.GetID() == 0 {
		missing = append(missing, "id")
	} else if run.GetID() != intent.RunID {
		return nil, fmt.Errorf("workflow run violates its exact durable dispatch binding: id %d does not match persisted run id %d", run.GetID(), intent.RunID)
	}
	// GitHub can briefly return the static workflow definition name after the
	// dispatch API has already returned the exact run ID, before the custom
	// run-name expression is materialized. Treat only that known placeholder
	// (and an empty value) as incomplete; every other mismatched title remains
	// a permanent fail-closed correlation violation.
	displayTitle := strings.TrimSpace(run.GetDisplayTitle())
	if displayTitle == "" || displayTitle == visualHiveProductionWorkflowName {
		missing = append(missing, "display_title")
	} else if displayTitle != intent.ExpectedDisplayTitle {
		return nil, fmt.Errorf("workflow run %d violates its exact durable dispatch binding: display_title does not match the persisted correlation", intent.RunID)
	}
	if run.GetEvent() == "" {
		missing = append(missing, "event")
	} else if run.GetEvent() != "workflow_dispatch" {
		return nil, fmt.Errorf("workflow run %d violates its exact durable dispatch binding: event %q is not workflow_dispatch", intent.RunID, run.GetEvent())
	}
	if run.GetHeadBranch() == "" {
		missing = append(missing, "head_branch")
	} else if run.GetHeadBranch() != intent.Ref {
		return nil, fmt.Errorf("workflow run %d violates its exact durable dispatch binding: head branch %q does not match persisted ref %q", intent.RunID, run.GetHeadBranch(), intent.Ref)
	}
	return missing, nil
}

func discardWorkflowDispatch(store *Store, intent WorkflowDispatchIntent, runID int64) error {
	current, exists, err := store.LoadWorkflowDispatchIntent()
	if err != nil {
		return err
	}
	if !exists || current.CorrelationID != intent.CorrelationID || current.RunID != runID {
		return fmt.Errorf("refusing to discard a workflow run without its exact durable dispatch binding")
	}
	return store.DeleteWorkflowDispatchIntent()
}

func retryCancelledDispatch(conclusion string, attempt, limit int) bool {
	return strings.EqualFold(strings.TrimSpace(conclusion), "cancelled") && attempt < limit
}

func runEligibleRepairs(ctx context.Context, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy, verifiedArtifact hivegithub.VerifiedVisualHiveArtifact, specialists repair.SpecialistDispatcher, specialistWorkDir string) ([]repair.Result, error) {
	snapshot := lifecycle.Snapshot()
	key := selectedRepairKey(snapshot)
	if key == "" {
		return nil, nil
	}
	baselineProtection, err := repair.InspectVisualBaselineProtection(ctx, config.CheckoutDir)
	if err != nil {
		return nil, fmt.Errorf("inspect trusted visual baseline protection before repair: %w", err)
	}
	for _, key := range []string{key} {
		finding := snapshot.Findings[key]
		if finding == nil || finding.HumanReviewRequired || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued && finding.Status != visualhive.StatusNeedsRevision && finding.Status != visualhive.StatusRepairRunning) || finding.IssueNumber <= 0 {
			continue
		}
		state, err := repair.NewStore(filepath.Join(config.StateDir, "repair"))
		if err != nil {
			return nil, err
		}
		// Integrated mode executes no target-authored preparation or test command
		// locally. Safe git index validation remains; exact hosted PR jobs are the
		// executable validation authority before merge.
		commands := []repair.Command{{Name: "git", Args: []string{"diff", "--check"}}}
		repairEvidenceRoot := verifiedArtifact.EvidenceRootPath
		var revisionArtifact *hivegithub.VerifiedPullRequestArtifact
		if needsHostedRevisionEvidence(*finding) {
			expectedRunID, runIdentityErr := failedVisualHiveWorkflowRunID(*finding)
			if runIdentityErr != nil {
				return nil, runIdentityErr
			}
			verified, fetchErr := client.FetchAndVerifyPullRequestArtifact(ctx, hivegithub.PullRequestArtifactRequest{
				Repository: config.Repository, PullRequestNumber: finding.PRNumber, ExpectedWorkflowRunID: expectedRunID,
				ExpectedHeadSHA: finding.RepairCommitSHA, ExpectedHeadBranch: finding.Branch,
				ExpectedWorkflowName: "Visual Hive PR", ExpectedWorkflowPath: ".github/workflows/visual-hive-pr.yml", ArtifactName: "visual-hive-pr",
				DestinationDir: filepath.Join(config.StateDir, "repair", "revision-artifacts", repairStateKey(finding.RepositoryFingerprint)),
			})
			if fetchErr != nil {
				return nil, fmt.Errorf("fetch exact-head failed Visual Hive evidence for repair revision: %w", fetchErr)
			}
			repairEvidenceRoot = verified.EvidenceRootPath
			revisionArtifact = &verified
		}
		evidenceSummary, err := repair.LoadEvidenceSummary(repairEvidenceRoot, *finding)
		if err != nil {
			if errors.Is(err, repair.ErrNoActionableEvidence) {
				reason := "Verified evidence does not contain a safe, repository-scoped repair contribution; keep the issue for operator review without dispatching a model or PR."
				if reviewErr := lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "repair_scope", reason); reviewErr != nil {
					return nil, reviewErr
				}
				return nil, nil
			}
			return nil, err
		}
		allowedPaths := repairPathsForFinding(config.AllowedRepairPaths, *finding)
		var provider repair.Provider = repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}
		repairAgent := ""
		attemptStartedAt := time.Time{}
		if specialists != nil {
			resolution := specialistResolutionForFinding(*finding)
			if !resolution.DispatchAllowed {
				reason := "Verified Visual Hive evidence cannot be routed to a proposal-capable Hive specialist: " + resolution.Reason
				if reviewErr := lifecycle.MarkManualReviewRequired(finding.RepositoryFingerprint, "specialist_route", reason); reviewErr != nil {
					return nil, reviewErr
				}
				return nil, nil
			}
			expectedBaseSHA := verifiedArtifact.CommitSHA
			evidenceIdentity, identityErr := specialistEvidenceIdentity(verifiedArtifact, *finding)
			if revisionArtifact != nil {
				expectedBaseSHA = finding.RepairCommitSHA
				evidenceIdentity, identityErr = specialistRevisionEvidenceIdentity(*revisionArtifact, *finding)
			}
			if identityErr != nil {
				return nil, identityErr
			}
			binding, bindingErr := nextSpecialistAttemptBinding(state, *finding, time.Now().UTC())
			if bindingErr != nil {
				return nil, bindingErr
			}
			deadline := binding.DeadlineAnchor.Add(25 * time.Minute)
			existingAttempt, hasAttempt := state.Get(finding.RepositoryFingerprint)
			recoveringExactSpecialistOrder := hasAttempt && existingAttempt.Recurrence == finding.Recurrences && existingAttempt.Stage == repair.StageModelRunning
			if !deadline.After(time.Now().UTC()) && !recoveringExactSpecialistOrder {
				return nil, fmt.Errorf("persisted specialist attempt deadline has expired; recover or explicitly retry the existing repair attempt")
			}
			role := agent.SpecialistRole(resolution.Role)
			mailbox, mailboxErr := agent.NewSpecialistMailbox(filepath.Join(specialistWorkDir, resolution.Role, "mailbox"), agent.SpecialistMailboxOptions{})
			if mailboxErr != nil {
				return nil, fmt.Errorf("open %s specialist mailbox: %w", resolution.Role, mailboxErr)
			}
			validation := make([]string, 0, len(commands))
			for _, command := range commands {
				validation = append(validation, strings.Join(append([]string{command.Name}, command.Args...), " "))
			}
			specialistProvider, providerErr := repair.NewSpecialistProvider(repair.SpecialistProviderConfig{
				Dispatcher: specialists, Mailbox: mailbox,
				Repository: config.Repository, RepositoryFingerprint: finding.RepositoryFingerprint,
				RecurrenceKey: fmt.Sprintf("%s:r%d", finding.RepositoryFingerprint, finding.Recurrences), Attempt: binding.Attempt,
				ExpectedBaseSHA: expectedBaseSHA, Evidence: evidenceIdentity,
				Specialist: role, RouteReason: resolution.Reason, AllowedPaths: allowedPaths, Validation: validation,
				Deadline: deadline,
			})
			if providerErr != nil {
				return nil, providerErr
			}
			provider = specialistProvider
			repairAgent = resolution.Role
			attemptStartedAt = binding.StartedAt
		}
		worker := repair.Worker{
			Config: repair.Config{
				RepositoryDir: config.CheckoutDir, WorktreeRoot: filepath.Join(config.StateDir, "repair", "worktrees"), BaseBranch: config.DefaultBranch,
				ExpectedRemoteURL:  RepositoryCloneURL(config.Repository),
				BaselineProtection: baselineProtection,
				Agent:              repairAgent, Policy: policy, AllowedRepairPaths: allowedPaths, ValidationCommands: commands,
				EvidenceSummary: evidenceSummary, AttemptStartedAt: attemptStartedAt,
				ModelTimeout: 20 * time.Minute, CommandTimeout: 15 * time.Minute,
			},
			Provider: provider, State: state, Lifecycle: lifecycle, GitHub: client,
		}
		result, err := worker.Run(ctx, *finding)
		if err != nil {
			if errors.Is(err, repair.ErrSpecialistWorkPending) {
				return nil, nil
			}
			return nil, err
		}
		return []repair.Result{result}, nil // repository concurrency budget defaults to one repair
	}
	return nil, nil
}

func repairPathsForFinding(configured []string, finding visualhive.FindingLifecycle) []string {
	result := append([]string(nil), configured...)
	if !strings.EqualFold(strings.TrimSpace(finding.IssueKind), visualhive.RepositoryTestFailureKind) {
		return result
	}
	// A repository-test failure may be caused by stale dependency metadata. It
	// is deliberately a per-finding expansion, never a global auto-merge
	// allowlist expansion. Manifest/lockfile repairs still receive a merge-policy
	// hold and require the exact-head approval path.
	result = append(result,
		"package.json", "**/package.json", "package-lock.json", "**/package-lock.json",
		"pnpm-lock.yaml", "**/pnpm-lock.yaml", "yarn.lock", "**/yarn.lock",
		"pyproject.toml", "**/pyproject.toml", "requirements*.txt", "**/requirements*.txt",
		"poetry.lock", "**/poetry.lock", "uv.lock", "**/uv.lock", "pdm.lock", "**/pdm.lock",
		"Pipfile", "**/Pipfile", "Pipfile.lock", "**/Pipfile.lock",
		"go.mod", "**/go.mod", "go.sum", "**/go.sum",
	)
	return result
}

// RepairPathsForFinding exposes the installed integrated repair-path policy to
// the normal VisualWorkController without duplicating its per-finding rules.
func RepairPathsForFinding(configured []string, finding visualhive.FindingLifecycle) []string {
	return repairPathsForFinding(configured, finding)
}

func availableRepairCommands(commands []repair.Command) ([]repair.Command, []string) {
	available := make([]repair.Command, 0, len(commands))
	deferred := []string{}
	seen := map[string]bool{}
	for _, command := range commands {
		name := strings.TrimSpace(command.Name)
		if name == "" {
			continue
		}
		key := name + "\x00" + strings.Join(command.Args, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := exec.LookPath(name); err != nil {
			deferred = append(deferred, strings.Join(append([]string{name}, command.Args...), " "))
			continue
		}
		available = append(available, command)
	}
	return available, deferred
}

func needsHostedRevisionEvidence(finding visualhive.FindingLifecycle) bool {
	if finding.Status != visualhive.StatusNeedsRevision || finding.PRNumber <= 0 || strings.TrimSpace(finding.RepairCommitSHA) == "" || strings.TrimSpace(finding.Branch) == "" {
		return false
	}
	for _, check := range finding.LastCheckRuns {
		name := strings.NewReplacer("-", " ", "_", " ").Replace(strings.ToLower(strings.TrimSpace(check.Name)))
		state := strings.ToLower(strings.TrimSpace(check.State))
		if strings.Contains(name, "visual hive") && state != "success" && state != "skipped" && state != "neutral" {
			return true
		}
	}
	return false
}

func failedVisualHiveWorkflowRunID(finding visualhive.FindingLifecycle) (int64, error) {
	var runID int64
	for _, check := range finding.LastCheckRuns {
		if !check.ProvenanceVerified || check.WorkflowRunID <= 0 || check.State != "failure" || check.WorkflowEvent != "pull_request" ||
			!workflowPathMatchesForSpecialist(check.WorkflowPath, ".github/workflows/visual-hive-pr.yml") {
			continue
		}
		if runID != 0 && runID != check.WorkflowRunID {
			return 0, fmt.Errorf("failed Visual Hive checks map to multiple workflow runs")
		}
		runID = check.WorkflowRunID
	}
	if runID == 0 {
		return 0, fmt.Errorf("repair revision requires one provenance-verified failed Visual Hive workflow run")
	}
	return runID, nil
}

// selectedRepairKey enforces the repository-wide concurrency budget before a
// worker is constructed. Any existing branch/PR/merge lifecycle blocks a new
// issue from starting; only the already-active repair may resume or revise.
func selectedRepairKey(state visualhive.LifecycleState) string {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	resumeKey := ""
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil {
			continue
		}
		switch finding.Status {
		case visualhive.StatusRepairRunning, visualhive.StatusNeedsRevision:
			if finding.HumanReviewRequired {
				return ""
			}
			if resumeKey == "" {
				resumeKey = key
			}
		case visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusReady,
			visualhive.StatusMerged, visualhive.StatusPostMergeVerifying:
			return ""
		}
	}
	if resumeKey != "" {
		return resumeKey
	}
	selected := ""
	selectedRank := -1
	for _, key := range keys {
		finding := state.Findings[key]
		if finding != nil && !finding.HumanReviewRequired && finding.IssueNumber > 0 &&
			(finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued) {
			rank := 0
			if strings.EqualFold(strings.TrimSpace(finding.IssueKind), visualhive.RepositoryTestFailureKind) {
				rank = 1
			}
			if selected == "" || rank > selectedRank {
				selected, selectedRank = key, rank
			}
		}
	}
	return selected
}

func repairPreparationCommands(checkout string) []repair.Command {
	type lockfile struct {
		path string
		name string
	}
	locks := []lockfile{}
	_ = filepath.WalkDir(checkout, func(filePath string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".visual-hive", "node_modules", "dist", "build", "coverage", "vendor":
				if filePath != checkout {
					return filepath.SkipDir
				}
			}
			return nil
		}
		switch entry.Name() {
		case "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb":
			if len(locks) < 100 {
				locks = append(locks, lockfile{path: filePath, name: entry.Name()})
			}
		}
		return nil
	})
	sort.Slice(locks, func(i, j int) bool { return locks[i].path < locks[j].path })
	commands := make([]repair.Command, 0, len(locks)+1)
	for _, lock := range locks {
		dir, _ := filepath.Rel(checkout, filepath.Dir(lock.path))
		dir = filepath.ToSlash(dir)
		if dir == "" {
			dir = "."
		}
		switch lock.name {
		case "package-lock.json":
			args := []string{"ci"}
			if dir != "." {
				args = []string{"--prefix", dir, "ci"}
			}
			commands = append(commands, repair.Command{Name: "npm", Args: args})
		case "pnpm-lock.yaml":
			args := []string{"install", "--frozen-lockfile"}
			if dir != "." {
				args = []string{"--dir", dir, "install", "--frozen-lockfile"}
			}
			commands = append(commands, repair.Command{Name: "pnpm", Args: args})
		case "yarn.lock":
			args := []string{"install", "--immutable"}
			if dir != "." {
				args = []string{"--cwd", dir, "install", "--immutable"}
			}
			commands = append(commands, repair.Command{Name: "yarn", Args: args})
		case "bun.lock", "bun.lockb":
			args := []string{"install", "--frozen-lockfile"}
			if dir != "." {
				args = []string{"--cwd", dir, "install", "--frozen-lockfile"}
			}
			commands = append(commands, repair.Command{Name: "bun", Args: args})
		}
	}
	if _, err := os.Stat(filepath.Join(checkout, "pyproject.toml")); err == nil {
		commands = append(commands, repair.Command{Name: "python", Args: []string{"-m", "pip", "install", "."}})
	} else if _, err := os.Stat(filepath.Join(checkout, "requirements.txt")); err == nil {
		commands = append(commands, repair.Command{Name: "python", Args: []string{"-m", "pip", "install", "-r", "requirements.txt"}})
	}
	return commands
}

func repairValidationEnvironment(config Config) map[string]string {
	for _, argument := range config.VisualHiveArgs {
		trimmed := strings.TrimSpace(argument)
		if filepath.IsAbs(trimmed) && (strings.HasSuffix(strings.ToLower(trimmed), ".js") || strings.HasSuffix(strings.ToLower(trimmed), ".mjs") || strings.HasSuffix(strings.ToLower(trimmed), ".cjs")) {
			return map[string]string{"VISUAL_HIVE_CLI": trimmed}
		}
	}
	return nil
}

func activeRepairFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// An existing branch/PR/merge owns the repository concurrency slot even if
	// another open issue sorts first. A held active repair blocks orchestration;
	// it must never make Hive select a second issue.
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.IssueNumber <= 0 {
			continue
		}
		switch finding.Status {
		case visualhive.StatusRepairRunning, visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision,
			visualhive.StatusReady, visualhive.StatusMerged, visualhive.StatusPostMergeVerifying:
			if finding.HumanReviewRequired {
				continue
			}
			return *finding, true
		}
	}
	selected := selectedRepairKey(state)
	if selected == "" {
		return visualhive.FindingLifecycle{}, false
	}
	finding := state.Findings[selected]
	if finding == nil || finding.IssueNumber <= 0 || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued) {
		return visualhive.FindingLifecycle{}, false
	}
	return *finding, true
}

func repairFindingForOrchestration(stateDir string, state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool, error) {
	store, err := NewStore(filepath.Join(stateDir, "integrated"))
	if err != nil {
		return visualhive.FindingLifecycle{}, false, err
	}
	approval, exists, err := store.LoadMergeApproval()
	if err != nil {
		return visualhive.FindingLifecycle{}, false, err
	}
	if !exists {
		if finding, ok := activeRepairFinding(state); ok {
			return finding, true, nil
		}
		return visualhive.FindingLifecycle{}, false, nil
	}
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matched := []visualhive.FindingLifecycle{}
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.Status != visualhive.StatusReady || !finding.HumanReviewRequired || finding.ManualReviewKind != "merge_policy" || finding.ObservationHumanReviewRequired {
			continue
		}
		if finding.PRNumber == approval.PRNumber && strings.EqualFold(finding.RepairCommitSHA, approval.HeadSHA) {
			matched = append(matched, *finding)
		}
	}
	if len(matched) != 1 {
		return visualhive.FindingLifecycle{}, false, fmt.Errorf("merge approval must match exactly one ready merge-policy-held finding; matched %d", len(matched))
	}
	return matched[0], true, nil
}

func mergePolicyHeldFindings(state visualhive.LifecycleState) []visualhive.FindingLifecycle {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matched := []visualhive.FindingLifecycle{}
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.Status != visualhive.StatusReady || !finding.HumanReviewRequired || finding.ManualReviewKind != "merge_policy" || finding.PRNumber <= 0 || strings.TrimSpace(finding.RepairCommitSHA) == "" || finding.MergeSHA != "" {
			continue
		}
		matched = append(matched, *finding)
	}
	return matched
}

func reconcileMergePolicyHold(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) (bool, error) {
	if client == nil {
		return false, fmt.Errorf("GitHub client is required to re-evaluate merge-policy holds")
	}
	for _, finding := range mergePolicyHeldFindings(lifecycle.Snapshot()) {
		// Never erase an observation-level review requirement while clearing a
		// merge-policy hold. It remains explicitly held for operator review.
		if finding.ObservationHumanReviewRequired {
			continue
		}
		gate, err := client.InspectPullRequestGate(ctx, config.Repository, finding.PRNumber)
		if err != nil {
			return false, fmt.Errorf("inspect merge-policy-held pull request #%d: %w", finding.PRNumber, err)
		}
		decision, allowed := mergePolicyRecheckDecision(config, policy, finding, gate)
		if !allowed {
			continue
		}
		store, err := NewStore(filepath.Join(stateDir, "integrated"))
		if err != nil {
			return false, err
		}
		detail := fmt.Sprintf("pr=%d head=%s current exact gate and policy now authorize repair", gate.Number, gate.HeadSHA)
		if err := store.AuditStrict(AuditEntry{Action: "recheck_merge_policy", Allowed: true, Repository: config.Repository, Detail: detail}); err != nil {
			return false, err
		}
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, "recheck_merge_policy", decision.Allowed, detail)
		if err := lifecycle.MarkManualReviewComplete(finding.RepositoryFingerprint, "merge_policy"); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func mergePolicyRecheckDecision(config Config, policy automation.Policy, finding visualhive.FindingLifecycle, gate hivegithub.PullRequestGate) (automation.Decision, bool) {
	if finding.Status != visualhive.StatusReady || !finding.HumanReviewRequired || finding.ManualReviewKind != "merge_policy" || finding.ObservationHumanReviewRequired ||
		finding.PRNumber <= 0 || strings.TrimSpace(finding.RepairCommitSHA) == "" || gate.Number != finding.PRNumber || !strings.EqualFold(gate.HeadSHA, finding.RepairCommitSHA) {
		return automation.Decision{}, false
	}
	policy = mergePolicyForFinding(policy, finding)
	decision := policy.Authorize(mergeActionRequest(config, finding, gate))
	return decision, decision.Allowed
}

func waitForPullRequestGate(ctx context.Context, client *hivegithub.Client, repository string, finding visualhive.FindingLifecycle) (hivegithub.PullRequestGate, error) {
	if finding.PRNumber <= 0 || finding.RepairCommitSHA == "" {
		return hivegithub.PullRequestGate{}, fmt.Errorf("finding %s has no persisted repair PR and exact head SHA", finding.RepositoryFingerprint)
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		gate, err := client.InspectPullRequestGate(ctx, repository, finding.PRNumber)
		if err != nil {
			return gate, err
		}
		if gate.HeadSHA != finding.RepairCommitSHA {
			return gate, fmt.Errorf("pull request #%d head %s does not match Hive repair commit %s", finding.PRNumber, gate.HeadSHA, finding.RepairCommitSHA)
		}
		pending := !hasVisualHiveCheck(gate)
		for _, state := range gate.RequiredCheckStates {
			pending = pending || state == "pending" || state == "queued" || state == "in_progress"
		}
		if !pending {
			return gate, nil
		}
		select {
		case <-ctx.Done():
			return gate, fmt.Errorf("wait for exact-head pull request gates: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func hasVisualHiveCheck(gate hivegithub.PullRequestGate) bool {
	if !gate.VisualHiveProvenanceVerified {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(gate.VisualHiveCheckState))
	return state != "" && state != "pending" && state != "queued" && state != "in_progress"
}

func gateChecksGreen(gate hivegithub.PullRequestGate) bool {
	if !gate.VisualHiveVerdictGreen {
		return false
	}
	for _, state := range gate.RequiredCheckStates {
		if strings.ToLower(strings.TrimSpace(state)) != "success" {
			return false
		}
	}
	return true
}

func checkSummary(gate hivegithub.PullRequestGate) string {
	parts := make([]string, 0, len(gate.Checks))
	for _, check := range gate.Checks {
		parts = append(parts, fmt.Sprintf("%s=%s", check.Name, check.State))
	}
	if len(parts) == 0 {
		return "no hosted checks observed"
	}
	return strings.Join(parts, ", ")
}

func mergeRisk(files []string) automation.RiskTier {
	for _, file := range files {
		trimmed := strings.Trim(strings.ToLower(strings.ReplaceAll(file, "\\", "/")), "/")
		normalized := "/" + trimmed + "/"
		base := trimmed
		if separator := strings.LastIndex(base, "/"); separator >= 0 {
			base = base[separator+1:]
		}
		if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_test.go") {
			continue
		}
		if strings.Contains(normalized, "/src/") {
			return automation.RiskLow
		}
	}
	return automation.RiskAutomatic
}

func repairActor(hint string) string {
	lower := strings.ToLower(hint)
	if strings.Contains(lower, "security") || strings.Contains(lower, "sec-check") {
		return "sec-check"
	}
	if strings.Contains(lower, "ci") {
		return "ci-maintainer"
	}
	return "quality"
}

func automationMode(value Automation) automation.Mode {
	switch value {
	case AutomationIssues:
		return automation.ModeIssues
	case AutomationRepairPR:
		return automation.ModeRepairPR
	case AutomationAutoMerge:
		return automation.ModeAutoMerge
	default:
		return automation.ModeAdvisory
	}
}

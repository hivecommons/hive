package integrated

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/kubestellar/hive/v2/pkg/automation"
	"github.com/kubestellar/hive/v2/pkg/beads"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
)

type RunOptions struct {
	StateDir string
	Timeout  time.Duration
	GitHub   *hivegithub.Client
}

type WorkflowRunEvidence struct {
	RunID            int64  `json:"run_id"`
	RunURL           string `json:"run_url"`
	HeadSHA          string `json:"head_sha"`
	Conclusion       string `json:"conclusion"`
	EvidenceArtifact int64  `json:"evidence_artifact_id"`
	BundleArtifact   int64  `json:"bundle_artifact_id"`
}

type RunResult struct {
	SchemaVersion      string                           `json:"schema_version"`
	Repository         string                           `json:"repository"`
	Workflow           WorkflowRunEvidence              `json:"workflow"`
	PostMergeWorkflow  *WorkflowRunEvidence             `json:"post_merge_workflow,omitempty"`
	Validation         visualhive.Validation            `json:"validation"`
	Lifecycle          visualhive.ApplyLifecycleResult  `json:"lifecycle"`
	PostMergeLifecycle *visualhive.ApplyLifecycleResult `json:"post_merge_lifecycle,omitempty"`
	Outbox             visualhive.OutboxProcessorResult `json:"outbox"`
	Repairs            []repair.Result                  `json:"repairs,omitempty"`
	Gates              []GateEvaluation                 `json:"gates,omitempty"`
	StartedAt          time.Time                        `json:"started_at"`
	CompletedAt        time.Time                        `json:"completed_at"`
}

type GateEvaluation struct {
	RepositoryFingerprint string                     `json:"repository_fingerprint"`
	Gate                  hivegithub.PullRequestGate `json:"gate"`
	Decision              *automation.Decision       `json:"merge_decision,omitempty"`
	MergeSHA              string                     `json:"merge_sha,omitempty"`
}

func RunOnce(ctx context.Context, options RunOptions) (RunResult, error) {
	result := RunResult{SchemaVersion: "hive.production-run.v1", StartedAt: time.Now().UTC()}
	if options.GitHub == nil || options.StateDir == "" {
		return result, fmt.Errorf("GitHub client and persistent state directory are required")
	}
	store, err := NewStore(filepath.Join(options.StateDir, "integrated"))
	if err != nil {
		return result, err
	}
	config, err := store.Load()
	if err != nil {
		return result, err
	}
	result.Repository = config.Repository
	if config.Paused {
		store.Audit(AuditEntry{Action: "run", Allowed: false, Repository: config.Repository, Detail: "repository automation is paused"})
		return result, fmt.Errorf("repository automation is paused")
	}
	store.Audit(AuditEntry{Action: "run", Allowed: true, Repository: config.Repository})
	if options.Timeout <= 0 {
		options.Timeout = 45 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	workflow, err := dispatchAndWait(runCtx, options.GitHub, config)
	if err != nil {
		return result, err
	}
	result.Workflow = workflow
	lifecycle, err := visualhive.NewLifecycleStore(filepath.Join(options.StateDir, "visual-hive"))
	if err != nil {
		return result, err
	}
	beadStore, err := beads.NewStore(filepath.Join(options.StateDir, "beads", "quality"))
	if err != nil {
		return result, err
	}
	validation, apply, outbox, err := applyWorkflowEvidence(runCtx, options.StateDir, config, workflow, lifecycle, beadStore, options.GitHub, automation.Policy{
		ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), Paused: config.Paused,
		AllowedRepositories: []string{config.Repository}, MaxRepairAttempts: 3,
		AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
	})
	if err != nil {
		return result, err
	}
	result.Validation, result.Lifecycle, result.Outbox = validation, apply, outbox
	policy := automation.Policy{
		ACMMLevel: config.ACMMLevel, Mode: automationMode(config.Automation), Paused: config.Paused,
		AllowedRepositories: []string{config.Repository}, MaxRepairAttempts: 3,
		AllowedAutoMergePaths: config.AllowedAutoMergePaths, AllowedAutoMergeRisk: config.AllowedAutoMergeRisk,
	}
	if config.Automation == AutomationRepairPR || config.Automation == AutomationAutoMerge {
		orchestration, orchestrationErr := orchestrateRepairs(runCtx, options.StateDir, config, lifecycle, beadStore, options.GitHub, policy)
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

type repairOrchestrationResult struct {
	Repairs            []repair.Result
	Gates              []GateEvaluation
	PostMergeWorkflow  *WorkflowRunEvidence
	PostMergeLifecycle *visualhive.ApplyLifecycleResult
	Outbox             visualhive.OutboxProcessorResult
}

func applyWorkflowEvidence(ctx context.Context, stateDir string, config Config, workflow WorkflowRunEvidence, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy) (visualhive.Validation, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, error) {
	bundle, _, err := client.FetchAndVerifyVisualHiveBundle(ctx, hivegithub.VisualHiveArtifactRequest{
		Repository: config.Repository, WorkflowRunID: workflow.RunID, ArtifactID: workflow.BundleArtifact,
		SourceArtifactID: workflow.EvidenceArtifact, DestinationDir: filepath.Join(stateDir, "visual-hive", "artifacts"),
		TargetRef: config.DefaultBranch, MaxACMM: config.ACMMLevel,
	})
	if err != nil {
		return visualhive.Validation{}, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	apply, err := lifecycle.ApplyBundle(bundle, beadStore, visualhive.ApplyLifecycleOptions{
		TargetRef: config.DefaultBranch, VerificationRunID: fmt.Sprintf("%d", workflow.RunID), VerificationURL: workflow.RunURL,
	})
	if err != nil {
		return bundle.Validation, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	outbox := visualhive.ProcessOutbox(ctx, lifecycle, beadStore, policy, client)
	if outbox.Failed > 0 {
		return bundle.Validation, apply, outbox, fmt.Errorf("GitHub lifecycle outbox failed: %s", strings.Join(outbox.Errors, "; "))
	}
	return bundle.Validation, apply, outbox, nil
}

func orchestrateRepairs(ctx context.Context, stateDir string, config Config, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy) (repairOrchestrationResult, error) {
	result := repairOrchestrationResult{}
	maxAttempts := policy.MaxRepairAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	for cycle := 0; cycle <= maxAttempts; cycle++ {
		repairs, err := runEligibleRepairs(ctx, config, lifecycle, client, policy)
		result.Repairs = append(result.Repairs, repairs...)
		if err != nil {
			return result, err
		}
		finding, ok := activeRepairFinding(lifecycle.Snapshot())
		if !ok {
			return result, nil
		}
		if finding.Status == visualhive.StatusMerged || finding.Status == visualhive.StatusPostMergeVerifying {
			postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
			result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
			return result, verifyErr
		}
		if finding.Status == visualhive.StatusIssueOpen || finding.Status == visualhive.StatusFixQueued || finding.Status == visualhive.StatusNeedsRevision || finding.Status == visualhive.StatusRepairRunning {
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("repair attempt budget exhausted for %s", finding.RepositoryFingerprint)
			}
			continue
		}
		gate, err := waitForPullRequestGate(ctx, client, config.Repository, finding)
		if err != nil {
			return result, err
		}
		green := gateChecksGreen(gate)
		checkEvidence := make([]visualhive.CheckEvidence, 0, len(gate.Checks))
		for _, check := range gate.Checks {
			checkEvidence = append(checkEvidence, visualhive.CheckEvidence{Name: check.Name, State: check.State, URL: check.URL})
		}
		summary := checkSummary(gate)
		if err := lifecycle.MarkChecksWithEvidence(finding.RepositoryFingerprint, gate.HeadSHA, green, summary, checkEvidence); err != nil {
			return result, err
		}
		evaluation := GateEvaluation{RepositoryFingerprint: finding.RepositoryFingerprint, Gate: gate}
		if !green {
			result.Gates = append(result.Gates, evaluation)
			if finding.RepairAttempts >= maxAttempts {
				return result, fmt.Errorf("hosted checks remain red after %d attempts: %s", finding.RepairAttempts, summary)
			}
			continue
		}
		if config.Automation == AutomationRepairPR {
			result.Gates = append(result.Gates, evaluation)
			return result, nil
		}
		decision := policy.Authorize(automation.ActionRequest{
			Action: automation.ActionMergePR, Agent: repairActor(finding.OwningAgentHint), Repository: config.Repository,
			Risk: mergeRisk(gate.ChangedFiles), ChangedFiles: gate.ChangedFiles, RepairAttempts: finding.RepairAttempts,
			ExpectedHeadSHA: finding.RepairCommitSHA, TestedHeadSHA: gate.HeadSHA,
			MergeableKnown: gate.MergeableKnown, Mergeable: gate.Mergeable, VisualHiveVerdictGreen: gate.VisualHiveVerdictGreen,
			RequiredCheckStates: gate.RequiredCheckStates, BranchProtectionEnabled: gate.BranchProtectionEnabled,
			Hold: gate.Hold, HumanReviewRequired: gate.HumanReviewRequired, BaselineChanged: gate.BaselineChanged,
			WorkflowChanged: gate.WorkflowChanged, SecuritySensitive: gate.SecuritySensitive, DeploymentChanged: gate.DeploymentChanged,
		})
		lifecycle.RecordAuthorization(finding.RepositoryFingerprint, string(automation.ActionMergePR), decision.Allowed, strings.Join(decision.Reasons, "; "))
		evaluation.Decision = &decision
		if !decision.Allowed {
			result.Gates = append(result.Gates, evaluation)
			return result, fmt.Errorf("merge denied for pull request #%d: %s", finding.PRNumber, strings.Join(decision.Reasons, "; "))
		}
		mergeSHA, err := client.MergePullRequestExact(ctx, config.Repository, finding.PRNumber, gate.HeadSHA)
		if err != nil {
			return result, err
		}
		evaluation.MergeSHA = mergeSHA
		result.Gates = append(result.Gates, evaluation)
		if err := lifecycle.MarkMerged(finding.RepositoryFingerprint, mergeSHA); err != nil {
			return result, err
		}
		finding.MergeSHA, finding.Status = mergeSHA, visualhive.StatusMerged
		postMerge, postApply, postOutbox, verifyErr := verifyMergedFinding(ctx, stateDir, config, finding, lifecycle, beadStore, client, policy)
		result.PostMergeWorkflow, result.PostMergeLifecycle, result.Outbox = &postMerge, &postApply, postOutbox
		return result, verifyErr
	}
	return result, fmt.Errorf("repair orchestration exceeded its bounded iteration budget")
}

func verifyMergedFinding(ctx context.Context, stateDir string, config Config, finding visualhive.FindingLifecycle, lifecycle *visualhive.LifecycleStore, beadStore *beads.Store, client *hivegithub.Client, policy automation.Policy) (WorkflowRunEvidence, visualhive.ApplyLifecycleResult, visualhive.OutboxProcessorResult, error) {
	postMerge, err := dispatchAndWait(ctx, client, config)
	if err != nil {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	if postMerge.HeadSHA != finding.MergeSHA {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, fmt.Errorf("post-merge verification ran at %s, expected exact merge SHA %s", postMerge.HeadSHA, finding.MergeSHA)
	}
	if err := lifecycle.MarkPostMergeVerifying(finding.RepositoryFingerprint, fmt.Sprintf("%d", postMerge.RunID), postMerge.RunURL); err != nil {
		return postMerge, visualhive.ApplyLifecycleResult{}, visualhive.OutboxProcessorResult{}, err
	}
	_, postApply, postOutbox, err := applyWorkflowEvidence(ctx, stateDir, config, postMerge, lifecycle, beadStore, client, policy)
	if err != nil {
		return postMerge, postApply, postOutbox, err
	}
	verified, exists := lifecycle.Finding(finding.RepositoryFingerprint)
	if !exists || verified.Status != visualhive.StatusIssueClosed {
		return postMerge, postApply, postOutbox, fmt.Errorf("post-merge verification did not resolve and close finding %s", finding.RepositoryFingerprint)
	}
	return postMerge, postApply, postOutbox, nil
}

func dispatchAndWait(ctx context.Context, client *hivegithub.Client, config Config) (WorkflowRunEvidence, error) {
	owner, repo, ok := strings.Cut(config.Repository, "/")
	if !ok {
		return WorkflowRunEvidence{}, fmt.Errorf("invalid configured repository")
	}
	const workflowFile = "hive-visual-hive.yml"
	started := time.Now().UTC().Add(-5 * time.Second)
	_, err := client.GoGitHub().Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, workflowFile, gh.CreateWorkflowDispatchEventRequest{Ref: config.DefaultBranch})
	if err != nil {
		return WorkflowRunEvidence{}, fmt.Errorf("dispatch Visual Hive production workflow: %w", err)
	}
	var selected *gh.WorkflowRun
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for selected == nil {
		runs, _, listErr := client.GoGitHub().Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &gh.ListWorkflowRunsOptions{
			Branch: config.DefaultBranch, Event: "workflow_dispatch", ExcludePullRequests: true, ListOptions: gh.ListOptions{PerPage: 20},
		})
		if listErr == nil {
			for _, candidate := range runs.WorkflowRuns {
				if candidate.GetCreatedAt().Time.Before(started) {
					continue
				}
				if selected == nil || candidate.GetCreatedAt().Time.After(selected.GetCreatedAt().Time) {
					selected = candidate
				}
			}
		}
		if selected != nil {
			break
		}
		select {
		case <-ctx.Done():
			return WorkflowRunEvidence{}, fmt.Errorf("wait for dispatched workflow: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	for selected.GetStatus() != "completed" {
		select {
		case <-ctx.Done():
			return WorkflowRunEvidence{}, fmt.Errorf("wait for workflow completion: %w", ctx.Err())
		case <-ticker.C:
		}
		current, _, getErr := client.GoGitHub().Actions.GetWorkflowRunByID(ctx, owner, repo, selected.GetID())
		if getErr != nil {
			continue
		}
		selected = current
	}
	if selected.GetConclusion() != "success" {
		return WorkflowRunEvidence{}, fmt.Errorf("Visual Hive workflow %s concluded %s", selected.GetHTMLURL(), selected.GetConclusion())
	}
	artifacts, _, err := client.GoGitHub().Actions.ListWorkflowRunArtifacts(ctx, owner, repo, selected.GetID(), &gh.ListOptions{PerPage: 100})
	if err != nil {
		return WorkflowRunEvidence{}, fmt.Errorf("list production evidence artifacts: %w", err)
	}
	workflow := WorkflowRunEvidence{RunID: selected.GetID(), RunURL: selected.GetHTMLURL(), HeadSHA: selected.GetHeadSHA(), Conclusion: selected.GetConclusion()}
	for _, artifact := range artifacts.Artifacts {
		switch {
		case strings.HasPrefix(artifact.GetName(), "visual-hive-evidence-"):
			workflow.EvidenceArtifact = artifact.GetID()
		case strings.HasPrefix(artifact.GetName(), "visual-hive-bundle-"):
			workflow.BundleArtifact = artifact.GetID()
		}
	}
	if workflow.EvidenceArtifact <= 0 || workflow.BundleArtifact <= 0 {
		return WorkflowRunEvidence{}, fmt.Errorf("workflow did not publish both evidence and provenance-bound bundle artifacts")
	}
	return workflow, nil
}

func runEligibleRepairs(ctx context.Context, config Config, lifecycle *visualhive.LifecycleStore, client *hivegithub.Client, policy automation.Policy) ([]repair.Result, error) {
	snapshot := lifecycle.Snapshot()
	keys := make([]string, 0, len(snapshot.Findings))
	for key := range snapshot.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := snapshot.Findings[key]
		if finding == nil || (finding.Status != visualhive.StatusIssueOpen && finding.Status != visualhive.StatusFixQueued && finding.Status != visualhive.StatusNeedsRevision && finding.Status != visualhive.StatusRepairRunning) || finding.IssueNumber <= 0 {
			continue
		}
		state, err := repair.NewStore(filepath.Join(config.StateDir, "repair"))
		if err != nil {
			return nil, err
		}
		commands := make([]repair.Command, 0, len(config.TestCommands))
		for _, parts := range config.TestCommands {
			if len(parts) > 0 {
				commands = append(commands, repair.Command{Name: parts[0], Args: append([]string(nil), parts[1:]...)})
			}
		}
		if len(commands) == 0 {
			return nil, fmt.Errorf("validated repository test plan has no executable commands")
		}
		worker := repair.Worker{
			Config: repair.Config{
				RepositoryDir: config.CheckoutDir, WorktreeRoot: filepath.Join(config.StateDir, "repair", "worktrees"), BaseBranch: config.DefaultBranch,
				Policy: policy, AllowedRepairPaths: config.AllowedRepairPaths, ValidationCommands: commands,
				ModelTimeout: 20 * time.Minute, CommandTimeout: 15 * time.Minute,
			},
			Provider: repair.CodexProvider{Command: config.ProviderCommand, Prefix: config.ProviderArgs}, State: state, Lifecycle: lifecycle, GitHub: client,
		}
		result, err := worker.Run(ctx, *finding)
		if err != nil {
			return nil, err
		}
		return []repair.Result{result}, nil // repository concurrency budget defaults to one repair
	}
	return nil, nil
}

func activeRepairFinding(state visualhive.LifecycleState) (visualhive.FindingLifecycle, bool) {
	keys := make([]string, 0, len(state.Findings))
	for key := range state.Findings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		finding := state.Findings[key]
		if finding == nil || finding.IssueNumber <= 0 {
			continue
		}
		switch finding.Status {
		case visualhive.StatusIssueOpen, visualhive.StatusFixQueued, visualhive.StatusRepairRunning,
			visualhive.StatusPROpen, visualhive.StatusChecksRunning, visualhive.StatusNeedsRevision,
			visualhive.StatusReady, visualhive.StatusMerged, visualhive.StatusPostMergeVerifying:
			return *finding, true
		}
	}
	return visualhive.FindingLifecycle{}, false
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
	for _, check := range gate.Checks {
		if strings.Contains(strings.ToLower(check.Name), "visual hive") && check.State != "pending" && check.State != "queued" && check.State != "in_progress" {
			return true
		}
	}
	return false
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
		if strings.HasPrefix(strings.ToLower(strings.ReplaceAll(file, "\\", "/")), "src/") {
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

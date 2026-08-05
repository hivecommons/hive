package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/internal/gittransport"
	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/automation"
	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/integrated"
	"github.com/kubestellar/hive/v2/pkg/repair"
	"github.com/kubestellar/hive/v2/pkg/scheduler"
	"github.com/kubestellar/hive/v2/pkg/visualhive"
	visualcontroller "github.com/kubestellar/hive/v2/pkg/visualhive/controller"
	"github.com/kubestellar/hive/v2/pkg/visualhive/normalservice"
)

const (
	normalVisualArtifactFetchTimeout = 45 * time.Minute
	normalVisualRepairModelTimeout   = 20 * time.Minute
	normalVisualRepairCommandTimeout = 15 * time.Minute
	normalVisualCycleFixedOverhead   = 30 * time.Minute

	// Visual Hive emits this canonical command as a request for its managed
	// exact-PR-head workflow verdict, not as target-authored local argv.
	normalVisualExactHeadValidationCommand = visualhive.ExactHeadValidationCommand
)

type normalVisualArtifactSource struct {
	stateDir          string
	timeout           time.Duration
	github            *hivegithub.Client
	gitTransportToken func(context.Context) (string, error)
}

func (source *normalVisualArtifactSource) Fetch(ctx context.Context) (integrated.NormalVisualWork, error) {
	if source.gitTransportToken != nil {
		ctx = gittransport.WithControllerTokenSource(ctx, source.gitTransportToken)
	}
	return integrated.FetchNormalVisualWork(ctx, source.stateDir, source.timeout, source.github)
}

func (source *normalVisualArtifactSource) Consume(workflow integrated.WorkflowRunEvidence, allowAlreadyConsumed bool) error {
	return integrated.ConsumeNormalVisualWork(source.stateDir, workflow, allowAlreadyConsumed)
}

type normalVisualRepairer struct {
	scheduler         *scheduler.Scheduler
	manager           *agent.Manager
	controller        *visualcontroller.Controller
	lifecycle         *visualhive.LifecycleStore
	github            *hivegithub.Client
	gitTransportToken func(context.Context) (string, error)
	expectedRemoteURL string
	providerCommand   string
	providerArgs      []string
	loadConfig        func() (integrated.Config, int, error)
	mu                sync.Mutex
}

func (runner *normalVisualRepairer) Run(ctx context.Context, supplied visualcontroller.DispatchEnvelope) (normalservice.RepairOutcome, error) {
	if runner == nil || runner.scheduler == nil || runner.manager == nil || runner.controller == nil || runner.lifecycle == nil || runner.github == nil || runner.loadConfig == nil {
		return normalservice.RepairOutcome{}, errors.New("normal Visual Hive Worker bridge is not configured")
	}
	// Worker is repository-sequential. The service already serializes cycles;
	// this lock also protects direct test/operator invocation of the bridge.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	current, normalACMM, err := runner.loadConfig()
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if err := runner.validateRuntime(current, supplied); err != nil {
		return normalservice.RepairOutcome{}, err
	}
	envelope, err := runner.controller.RevalidateSpecialistBoundary(supplied.SourceExternalRef, "", "")
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if !reflect.DeepEqual(envelope, supplied) {
		return normalservice.RepairOutcome{}, errors.New("service dispatch differs from the native intake-owned envelope")
	}
	commands, err := exactWorkerCommands(envelope.ValidationCommands, current.TestCommands)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	evidenceSummary, err := repair.LoadEvidenceSummary(envelope.EvidenceRoot, envelope.Finding)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	state, err := repair.NewStore(filepath.Join(current.StateDir, "repair"))
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	role := agent.SpecialistRole(envelope.Work.Role)
	mailbox, err := agent.NewSpecialistMailbox(filepath.Join(current.StateDir, "repair", "mailbox", string(role)), agent.SpecialistMailboxOptions{})
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	reservedID, reservedDigest := envelope.SpecialistWorkOrderID, envelope.SpecialistRequestSHA256
	builder := func(buildCtx context.Context, worktree, baseSHA, baseTreeSHA, _ string, readiness agent.SpecialistSessionIdentity) (agent.SpecialistWorkOrderRequest, error) {
		profile := scheduler.GovernedProposalExecutorProfile{
			Backend: readiness.ExecutorBackend, ProviderSHA256: readiness.ProviderSHA256, Model: readiness.ExecutorModel,
			ConfigurationSHA256: readiness.ExecutorConfigSHA256, ContainmentProfile: readiness.ContainmentProfile,
			BackendParityClaimed: readiness.BackendParityClaimed,
		}
		if err := runner.scheduler.SetGovernedProposalExecutorProfile(profile); err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		admitted, receipt, err := visualcontroller.BuildSchedulerAdmittedWork(envelope)
		if err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		composer, err := repair.NewGovernedSourceContextComposer(repair.GovernedSourceContextOptions{
			Context: buildCtx, Worktree: worktree, Repository: current.Repository, BaseSHA: baseSHA, BaseTreeSHA: baseTreeSHA,
			EvidenceRoot: envelope.EvidenceRoot, AllowedPaths: envelope.AllowedRepairPaths,
			AffectedContracts: envelope.Work.AffectedContracts,
			RelevanceHints:    append([]string{envelope.Finding.Title, envelope.Finding.Body}, envelope.Work.AffectedContracts...),
			ManifestSHA256:    envelope.Evidence.ArtifactSHA256, VerificationReceiptSHA256: envelope.Evidence.VerificationReceiptSHA256,
		})
		if err != nil {
			return agent.SpecialistWorkOrderRequest{}, err
		}
		defer composer.Close()
		return runner.scheduler.BuildGovernedProposalMessage(role, admitted, receipt, composer)
	}
	reserve := func(order agent.SpecialistWorkOrder) error {
		persisted, err := runner.controller.ReserveSpecialistWorkOrderIdentity(envelope.SourceExternalRef, order.ID, order.RequestSHA256)
		if err != nil {
			return err
		}
		if persisted.SpecialistWorkOrderID != order.ID || persisted.SpecialistRequestSHA256 != order.RequestSHA256 {
			return errors.New("native intake did not persist the exact Scheduler work-order identity")
		}
		reservedID, reservedDigest = order.ID, order.RequestSHA256
		return nil
	}
	guard := func(order agent.SpecialistWorkOrder) error {
		_, err := runner.controller.RevalidateSpecialistBoundary(envelope.SourceExternalRef, order.ID, order.RequestSHA256)
		return err
	}
	provider, err := repair.NewSpecialistProvider(repair.SpecialistProviderConfig{
		Dispatcher: runner.manager, Mailbox: mailbox, Repository: current.Repository,
		RepositoryFingerprint: envelope.Work.RepositoryFingerprint,
		RecurrenceKey:         fmt.Sprintf("%s:r%d", envelope.Work.RepositoryFingerprint, envelope.Recurrence), Attempt: envelope.Attempt,
		ExpectedBaseSHA: envelope.BaseSHA, ExpectedBaseTreeSHA: envelope.BaseTreeSHA, Evidence: envelope.Evidence,
		WorkOrderKind: agent.SpecialistWorkOrderKindGovernedVisualHiveProposal, GovernedBuilder: builder,
		ReserveWorkOrder: reserve, LaunchGuard: guard, Specialist: role, RouteReason: envelope.Work.RoutingReason,
		AllowedPaths: envelope.AllowedRepairPaths, Validation: envelope.ValidationCommands, Deadline: envelope.CompositionDeadline,
	})
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	policyLoader := func() (automation.Policy, error) {
		latest, latestACMM, err := runner.loadConfig()
		if err != nil {
			return automation.Policy{}, err
		}
		if err := runner.validateRuntime(latest, envelope); err != nil {
			return automation.Policy{}, err
		}
		policy := integrated.PolicyForConfig(latest)
		policy.Mode = automation.ModeRepairPR
		if latestACMM < policy.ACMMLevel {
			policy.ACMMLevel = latestACMM
		}
		return policy, nil
	}
	runtimeGuard := func(_ visualhive.FindingLifecycle, _ automation.Action, _ []string, _ int) error {
		_, err := runner.controller.RevalidateSpecialistBoundary(envelope.SourceExternalRef, reservedID, reservedDigest)
		return err
	}
	policy, err := policyLoader()
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	if normalACMM < policy.ACMMLevel {
		policy.ACMMLevel = normalACMM
	}
	baselineProtection, err := repair.InspectVisualBaselineProtectionAtCommit(ctx, current.CheckoutDir, envelope.BaseSHA)
	if err != nil {
		return normalservice.RepairOutcome{}, fmt.Errorf("inspect exact-base visual baseline protection before normal governed repair: %w", err)
	}
	expectedRemoteURL := strings.TrimSpace(runner.expectedRemoteURL)
	if expectedRemoteURL == "" {
		expectedRemoteURL = integrated.RepositoryCloneURL(current.Repository)
	}
	worker := repair.Worker{
		Config: repair.Config{
			RepositoryDir: current.CheckoutDir, WorktreeRoot: filepath.Join(current.StateDir, "repair", "worktrees"), BaseBranch: current.DefaultBranch,
			ExpectedRemoteURL: expectedRemoteURL, BaselineProtection: baselineProtection,
			Agent: string(role), Policy: policy, PolicyLoader: policyLoader, RuntimeGuard: runtimeGuard,
			AllowedRepairPaths: envelope.AllowedRepairPaths, ValidationCommands: commands, EvidenceSummary: evidenceSummary,
			ModelTimeout: boundedVisualWorkDuration(envelope.CompositionDeadline, normalVisualRepairModelTimeout), CommandTimeout: normalVisualRepairCommandTimeout,
		},
		Provider: provider, State: state, Lifecycle: runner.lifecycle, GitHub: runner.github,
	}
	workerCtx := ctx
	if runner.gitTransportToken != nil {
		workerCtx = gittransport.WithControllerTokenSource(ctx, runner.gitTransportToken)
	}
	result, err := worker.Run(workerCtx, envelope.Finding)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	attempt, exists := state.Get(envelope.Work.RepositoryFingerprint)
	if !exists || attempt.ModelInvocationID == "" || attempt.PRNumber != result.PRNumber || attempt.CommitSHA != result.CommitSHA {
		return normalservice.RepairOutcome{}, errors.New("Worker PR result has no exact durable specialist checkpoint")
	}
	order, err := mailbox.LoadWorkOrder(attempt.ModelInvocationID)
	if err != nil {
		return normalservice.RepairOutcome{}, err
	}
	return normalservice.RepairOutcome{WorkOrderID: order.ID, RequestSHA256: order.RequestSHA256, Result: result}, nil
}

func (runner *normalVisualRepairer) Plan(ctx context.Context, binding normalservice.RepairRetirementPlan) (normalservice.RepairRetirementPlan, error) {
	if runner == nil || runner.github == nil || runner.lifecycle == nil || runner.loadConfig == nil {
		return normalservice.RepairRetirementPlan{}, errors.New("normal Visual Hive repair retirement is not configured")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	current, _, err := runner.loadConfig()
	if err != nil {
		return normalservice.RepairRetirementPlan{}, err
	}
	if !strings.EqualFold(current.Repository, binding.Repository) || current.DefaultBranch != binding.BaseBranch ||
		current.Automation != integrated.AutomationRepairPR && current.Automation != integrated.AutomationAutoMerge {
		return normalservice.RepairRetirementPlan{}, errors.New("repair retirement does not match the authoritative installed repository")
	}
	if err := validateNormalRepairRetirementFinding(runner.lifecycle, binding, false); err != nil {
		return normalservice.RepairRetirementPlan{}, err
	}
	parts := strings.Split(binding.Repository, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return normalservice.RepairRetirementPlan{}, errors.New("repair retirement repository is invalid")
	}
	defaultHead, err := runner.github.LatestCommitHash(ctx, parts[0], parts[1], binding.BaseBranch)
	if err != nil {
		return normalservice.RepairRetirementPlan{}, fmt.Errorf("read current default head before repair retirement: %w", err)
	}
	defaultHead = strings.ToLower(strings.TrimSpace(defaultHead))
	if defaultHead == "" || strings.EqualFold(defaultHead, binding.BaseSHA) {
		return normalservice.RepairRetirementPlan{}, errors.New("default branch has not changed since the repair proposal was created")
	}
	binding.CurrentDefaultHeadSHA = defaultHead
	return binding, nil
}

func (runner *normalVisualRepairer) Apply(ctx context.Context, plan normalservice.RepairRetirementPlan) error {
	if runner == nil || runner.github == nil || runner.lifecycle == nil || runner.controller == nil || runner.loadConfig == nil {
		return errors.New("normal Visual Hive repair retirement is not configured")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	current, normalACMM, err := runner.loadConfig()
	if err != nil {
		return err
	}
	if !strings.EqualFold(current.Repository, plan.Repository) || current.DefaultBranch != plan.BaseBranch ||
		current.Automation != integrated.AutomationRepairPR && current.Automation != integrated.AutomationAutoMerge {
		return errors.New("repair retirement no longer matches the authoritative installed repository")
	}
	if err := validateNormalRepairRetirementFinding(runner.lifecycle, plan, true); err != nil {
		return err
	}
	repairState, err := repair.NewStore(filepath.Join(current.StateDir, "repair"))
	if err != nil {
		return fmt.Errorf("load exact Worker checkpoint before repair retirement: %w", err)
	}
	workerRetirement := repair.PullRequestRetirement{
		RepositoryFingerprint: plan.RepositoryFingerprint,
		PRNumber:              plan.PullRequestNumber,
		PRURL:                 plan.PullRequestURL,
		Branch:                plan.Branch,
		CommitSHA:             plan.HeadSHA,
	}
	if err := repairState.ValidatePullRequestRetirement(workerRetirement); err != nil {
		return fmt.Errorf("validate exact Worker checkpoint before repair retirement: %w", err)
	}
	finding, exists := runner.lifecycle.Finding(plan.RepositoryFingerprint)
	if !exists {
		return errors.New("repair retirement lost its exact lifecycle finding")
	}
	retired := visualhive.RetiredRepair{
		PullRequestNumber: plan.PullRequestNumber, PullRequestURL: plan.PullRequestURL,
		Branch: plan.Branch, HeadSHA: plan.HeadSHA, BaseBranch: plan.BaseBranch, BaseSHA: plan.BaseSHA,
		CurrentDefaultHeadSHA: plan.CurrentDefaultHeadSHA,
		VerdictReceipt:        append([]byte(nil), plan.VerdictReceipt...),
		VerdictReceiptSHA256:  plan.VerdictReceiptSHA256,
	}
	if err := runner.lifecycle.ValidateRepairRetirement(plan.RepositoryFingerprint, retired); err != nil {
		return err
	}
	decisions := normalRepairRetirementDecisions(current, normalACMM, finding)
	for _, decision := range decisions {
		detail := strings.Join(decision.Reasons, "; ")
		if decision.Allowed {
			detail = fmt.Sprintf("owner-authorized exact repair retirement for pull request #%d at %s", plan.PullRequestNumber, plan.HeadSHA)
		}
		if err := runner.lifecycle.RecordAuthorizationStrict(plan.RepositoryFingerprint, string(decision.Action), decision.Allowed, detail); err != nil {
			return fmt.Errorf("persist repair-retirement %s authorization: %w", decision.Action, err)
		}
	}
	for _, decision := range decisions {
		if !decision.Allowed {
			return fmt.Errorf("current Hive authority denies %s: %s", decision.Action, strings.Join(decision.Reasons, "; "))
		}
	}
	marker := "<!-- hive-repair: " + plan.RepositoryFingerprint + " -->"
	if err := runner.github.CloseRepairPullRequestExact(ctx, plan.Repository, plan.PullRequestNumber, marker, plan.Branch, plan.HeadSHA); err != nil {
		return fmt.Errorf("close exact repair proposal for fresh verification: %w", err)
	}
	if err := runner.github.DeleteRepairBranchExact(ctx, plan.Repository, plan.Branch, plan.HeadSHA); err != nil {
		return fmt.Errorf("delete exact repair branch for fresh verification: %w", err)
	}
	if err := runner.lifecycle.RetireRepairForVerification(plan.RepositoryFingerprint, retired); err != nil {
		return err
	}
	if err := repairState.RetirePullRequestForVerification(workerRetirement); err != nil {
		return fmt.Errorf("retire exact Worker checkpoint for fresh verification: %w", err)
	}
	return runner.controller.RetireSpecialistPullRequest(plan.SourceExternalRef, visualcontroller.SpecialistPullRequestRetirement{
		WorkOrderID: plan.WorkOrderID, RequestSHA256: plan.RequestSHA256,
		Branch: plan.Branch, CommitSHA: plan.HeadSHA, BaseBranch: plan.BaseBranch, BaseSHA: plan.BaseSHA,
		CurrentDefaultHeadSHA: plan.CurrentDefaultHeadSHA,
		PullRequestNumber:     plan.PullRequestNumber, PullRequestURL: plan.PullRequestURL,
		VerdictReceiptSHA256: plan.VerdictReceiptSHA256,
		RetirementReasonCode: plan.RepairRetirementReasonCode,
	})
}

func normalRepairRetirementDecisions(current integrated.Config, normalACMM int, finding visualhive.FindingLifecycle) []automation.Decision {
	policy := integrated.PolicyForConfig(current)
	if normalACMM < policy.ACMMLevel {
		policy.ACMMLevel = normalACMM
	}
	route := visualhive.ResolveSpecialist(visualhive.SpecialistRoutingInput{
		IssueKind: finding.IssueKind, OwningAgentHint: finding.OwningAgentHint,
		GroundedRepairScope:      strings.TrimSpace(finding.ValidationCommand) != "" && len(finding.AffectedContracts) > 0,
		BaselineChangesForbidden: true,
	})
	actor := strings.TrimSpace(route.Role)
	if actor == "" {
		actor = "quality"
	}
	return []automation.Decision{
		policy.Authorize(automation.ActionRequest{
			Action: automation.ActionCloseRepairPR, Agent: actor, Repository: current.Repository,
			RepairAttempts: finding.RepairAttempts,
		}),
		policy.Authorize(automation.ActionRequest{
			Action: automation.ActionDeleteRepairBranch, Agent: actor, Repository: current.Repository,
			RepairAttempts: finding.RepairAttempts,
		}),
	}
}

func validateNormalRepairRetirementFinding(lifecycle *visualhive.LifecycleStore, plan normalservice.RepairRetirementPlan, allowRetired bool) error {
	finding, exists := lifecycle.Finding(plan.RepositoryFingerprint)
	if !exists || !strings.EqualFold(finding.Repository, plan.Repository) {
		return errors.New("repair retirement lost its exact lifecycle finding")
	}
	if allowRetired && finding.Status == visualhive.StatusIssueOpen && finding.LastRetiredRepair != nil {
		retired := finding.LastRetiredRepair
		if retired.PullRequestNumber == plan.PullRequestNumber && retired.PullRequestURL == plan.PullRequestURL &&
			retired.Branch == plan.Branch && strings.EqualFold(retired.HeadSHA, plan.HeadSHA) &&
			retired.BaseBranch == plan.BaseBranch && strings.EqualFold(retired.BaseSHA, plan.BaseSHA) &&
			strings.EqualFold(retired.CurrentDefaultHeadSHA, plan.CurrentDefaultHeadSHA) &&
			string(retired.VerdictReceipt) == string(plan.VerdictReceipt) &&
			strings.EqualFold(retired.VerdictReceiptSHA256, plan.VerdictReceiptSHA256) {
			return nil
		}
	}
	if (finding.Status != visualhive.StatusNeedsRevision && finding.Status != visualhive.StatusReady) || finding.MergeSHA != "" ||
		finding.PRNumber != plan.PullRequestNumber || finding.PRURL != plan.PullRequestURL ||
		finding.Branch != plan.Branch || !strings.EqualFold(finding.RepairCommitSHA, plan.HeadSHA) {
		return errors.New("repair retirement no longer matches the exact verified, unmerged lifecycle proposal")
	}
	return nil
}

func (runner *normalVisualRepairer) validateRuntime(current integrated.Config, envelope visualcontroller.DispatchEnvelope) error {
	if !strings.EqualFold(current.Repository, envelope.Work.Packet.Repository) || current.RepositoryID != envelope.Work.Packet.RepositoryID ||
		current.StateDir == "" || current.CheckoutDir == "" || (current.Automation != integrated.AutomationRepairPR && current.Automation != integrated.AutomationAutoMerge) ||
		current.Paused || current.ProviderCommand != runner.providerCommand || !reflect.DeepEqual(current.ProviderArgs, runner.providerArgs) {
		return errors.New("current installed Visual Hive runtime no longer matches the configured normal Worker bridge")
	}
	return nil
}

func exactWorkerCommands(required []string, configured [][]string) ([]repair.Command, error) {
	argv, err := visualhive.ResolveValidationCommandArgv(required, configured)
	if err != nil {
		return nil, err
	}
	result := make([]repair.Command, 0, len(argv))
	for _, command := range argv {
		result = append(result, repair.Command{Name: command[0], Args: append([]string(nil), command[1:]...)})
	}
	return result, nil
}

func boundedVisualWorkDuration(deadline time.Time, maximum time.Duration) time.Duration {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return time.Second
	}
	if remaining < maximum {
		return remaining
	}
	return maximum
}

func configureNormalVisualWorkRunner(
	installed integrated.Config,
	controller *visualcontroller.Controller,
	lifecycle *visualhive.LifecycleStore,
	sched *scheduler.Scheduler,
	manager *agent.Manager,
	github *hivegithub.Client,
	gitTransportToken func(context.Context) (string, error),
	dashboardReady func() bool,
	logger *slog.Logger,
) (*normalservice.Service, *normalVisualServiceHealthReporter, error) {
	if configuredRuntimeOwnerIntent(installed) != runtimeOwnerNormalHive {
		return nil, nil, nil
	}
	if github == nil {
		return nil, nil, errors.New("normal governed repair service requires the existing GitHub client")
	}
	if gitTransportToken == nil {
		return nil, nil, errors.New("normal governed repair service requires a controller-owned Git transport credential source")
	}
	if dashboardReady == nil {
		return nil, nil, errors.New("normal governed repair service requires the existing dashboard listener readiness probe")
	}
	executor, err := repair.NewCodexSpecialistChildExecutor(integratedSetupCodexProvider(installed.ProviderCommand, installed.ProviderArgs))
	if err != nil {
		return nil, nil, err
	}
	loader := func() (integrated.Config, int, error) {
		current, exists, err := loadAuthoritativeVisualWorkContract()
		if err != nil {
			return integrated.Config{}, 0, err
		}
		if !exists {
			return integrated.Config{}, 0, errors.New("authoritative installed Visual Hive contract is unavailable")
		}
		return current, manager.GetACMMLevel(), nil
	}
	source := &normalVisualArtifactSource{
		stateDir: installed.StateDir, timeout: normalVisualArtifactFetchTimeout, github: github,
		gitTransportToken: gitTransportToken,
	}
	repairer := &normalVisualRepairer{
		scheduler: sched, manager: manager, controller: controller, lifecycle: lifecycle, github: github,
		gitTransportToken: gitTransportToken, expectedRemoteURL: integrated.RepositoryCloneURL(installed.Repository),
		providerCommand: installed.ProviderCommand, providerArgs: append([]string(nil), installed.ProviderArgs...), loadConfig: loader,
	}
	verdict := &normalVisualPullRequestVerifier{github: github, lifecycle: lifecycle, loadConfig: loader}
	poll := time.Duration(installed.RunIntervalSeconds) * time.Second
	maxCycle := configuredNormalVisualServiceMaxCycle(installed)
	health, err := newNormalVisualServiceHealthReporter(installed.StateDir, installed.Repository, poll, maxCycle, logger)
	if err != nil {
		return nil, nil, err
	}
	service, err := normalservice.New(normalservice.Options{
		StateDir: filepath.Join(installed.StateDir, "visual-hive"), PollInterval: poll, MaxCycleDuration: maxCycle, LeaseRetry: 30 * time.Second, QuiesceInterval: 100 * time.Millisecond,
		AcquireLease: func() (func(), error) { return integrated.AcquireNormalVisualWorkLease(installed.StateDir) },
		ShouldQuiesce: func() (bool, error) {
			if !dashboardReady() {
				return true, nil
			}
			requested, err := integrated.PauseRequested(installed.StateDir)
			if err != nil || requested {
				return requested, err
			}
			setupHeld, err := integrated.NormalVisualWorkRequiresSetupQuiescence(installed.StateDir)
			if err != nil || setupHeld {
				return true, err
			}
			current, _, err := loader()
			if err != nil {
				return true, err
			}
			// A replacement authoritative state root must start its own service;
			// this runner must not retain or resume ownership of the old root.
			if !sameSpecialistRuntimePath(current.StateDir, installed.StateDir) {
				return true, nil
			}
			return current.Paused, nil
		},
		Source: source, Intake: controller, Repairer: repairer, RepairRetirement: repairer, PullRequestState: verdict,
		// The verifier applies only its opaque check-evidence capability. The
		// service/controller still own completion and workflow consumption; no
		// baseline approval, issue-resolution, merge, or unrelated repository-write
		// authority exists. Setup baseline progression reuses the integrated state
		// machine and quiesces for exact external approval.
		Verdict: verdict, Logger: logger, OnCycleStart: health.StartCycle, OnCycle: health.RecordCycle, OnInactive: health.RecordInactive,
	})
	if err != nil {
		return nil, nil, err
	}
	// Configure the ordinary Manager only after every other fallible runtime
	// dependency has been constructed. A failed hot activation must not leave
	// a half-installed specialist dispatcher behind.
	if err := manager.ConfigureSpecialistChildDispatcher(agent.SpecialistChildDispatcherOptions{
		RepairStateRoot: filepath.Join(installed.StateDir, "repair"), Executor: executor,
	}); err != nil {
		return nil, nil, err
	}
	return service, health, nil
}

// configuredNormalVisualServiceMaxCycle is a hard context deadline, not only
// a health hint. Its budget covers the hosted evidence fetch, one bounded model
// invocation, every configured validation command, and bounded Git/GitHub /
// checkpoint overhead. Interrupted command recovery starts a later cycle with
// a new bound. Pathological configurations remain capped rather than creating
// an unbounded reconciler.
func configuredNormalVisualServiceMaxCycle(installed integrated.Config) time.Duration {
	fixed := normalVisualArtifactFetchTimeout + normalVisualRepairModelTimeout + normalVisualCycleFixedOverhead
	perCommand := normalVisualRepairCommandTimeout
	maximumCommands := int((normalVisualServiceMaxCycleCap - fixed) / perCommand)
	if len(installed.TestCommands) > maximumCommands {
		return normalVisualServiceMaxCycleCap
	}
	return fixed + time.Duration(len(installed.TestCommands))*perCommand
}
